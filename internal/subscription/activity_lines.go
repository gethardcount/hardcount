package subscription

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
)

// The subscription timeline's activity lines (ADR-128). describeSubscription
// Action turns one audit row into an activity.Line: title, problem, meta and
// tone. It takes no clock input; activityTimeline stamps the row's times and
// its actor after describing, so a test-clock line differs from a real-clock
// one only by its Recorded part.

// lineLabels are the names a line shows for the objects its row names by id,
// looked up once per timeline.
type lineLabels struct {
	// plans maps a plan id to its name. A plan that could not be read shows
	// its id.
	plans map[string]string
	// invoices maps an invoice id to its number, for the invoices that still
	// exist. A row's invoice is linked only when it is here (rule 8).
	invoices map[string]string
}

func (lb lineLabels) plan(id string) string {
	return planLabel(id, lb.plans)
}

// Kinds of the subscription lines: the machine name of each event.
const (
	kindCreated             = "subscription.created"
	kindActivated           = "subscription.activated"
	kindCanceled            = "subscription.canceled"
	kindCancelScheduled     = "subscription.cancel_scheduled"
	kindCancelCleared       = "subscription.cancel_cleared"
	kindCollectionPaused    = "subscription.collection_paused"
	kindCollectionResumed   = "subscription.collection_resumed"
	kindTrialEnded          = "subscription.trial_ended"
	kindTrialEndedEarly     = "subscription.trial_ended_early"
	kindTrialExtended       = "subscription.trial_extended"
	kindBillingRetried      = "subscription.billing_retried"
	kindThresholdsSet       = "subscription.spend_thresholds_set"
	kindThresholdsRemoved   = "subscription.spend_thresholds_removed"
	kindItemAdded           = "subscription.item_added"
	kindItemRemoved         = "subscription.item_removed"
	kindPlanChangeCanceled  = "subscription.plan_change_canceled"
	kindPlanChanged         = "subscription.plan_changed"
	kindPlanChangeScheduled = "subscription.plan_change_scheduled"
	kindQuantityChanged     = "subscription.quantity_changed"
	kindPlanChangeApplied   = "subscription.plan_change_applied"
	kindThresholdCrossed    = "subscription.spend_threshold_crossed"
	kindProrationNotApplied = "subscription.proration_not_applied"
	kindUsageBelowZero      = "subscription.usage_below_zero"
	// kindChanged is the one fallback, for a row no describe case knows. A
	// golden test fails if a token a writer emits reaches it.
	kindChanged = "subscription.changed"
)

// describeSubscriptionAction words one subscription audit row (S-1 to S-25 in
// the activity line standard). The actor is the caller's: it comes from the
// audit row, not from its metadata.
func describeSubscriptionAction(action string, meta map[string]any, lb lineLabels) activity.Line {
	l := activity.Line{Tone: activity.ToneNeutral}
	switch action {
	case domain.AuditActionCreate:
		l.Kind, l.Title = kindCreated, "Subscription created"
	case domain.AuditActionActivate:
		l.Kind, l.Title = kindActivated, "Subscription activated"
	case domain.AuditActionCancel:
		l.Kind, l.Title = kindCanceled, "Subscription canceled"
		if why := cancelWhy(meta); why != "" {
			l.Detail = append(l.Detail, activity.Text(why))
		}
		if c, ok := metaInt(meta, "prorated_credit_cents"); ok && c > 0 {
			l.Detail = append(l.Detail, activity.Segment{activity.T("Prorated credit "), activity.M(c, metaString(meta, "currency"))})
		}
	case domain.AuditActionUpdate:
		return describeUpdate(meta, lb)
	case domain.AuditActionSubscriptionItemUpdated:
		return describeItemUpdated(meta, lb)
	case domain.AuditActionSubscriptionPendingChangeApplied:
		l.Kind, l.Title = kindPlanChangeApplied, "Scheduled plan change applied"
		if p := planArrow(meta, lb); p != "" {
			l.Detail = append(l.Detail, activity.Text(p))
		}
	case domain.AuditActionSubscriptionThresholdCrossed:
		l.Kind, l.Title = kindThresholdCrossed, "Spend threshold crossed"
		id, num := metaString(meta, "invoice_id"), metaString(meta, "invoice_number")
		switch {
		case lb.invoices[id] != "":
			l.Detail = append(l.Detail, activity.Segment{activity.T("Invoice "), activity.L(activity.LinkInvoice, id, lb.invoices[id]), activity.T(" issued early")})
		case num != "":
			l.Detail = append(l.Detail, activity.Text("Invoice "+num+" issued early"))
		default:
			l.Detail = append(l.Detail, activity.Text("Invoice issued early"))
		}
		if amt, ok := metaInt(meta, "amount_cents"); ok {
			l.Detail = append(l.Detail, activity.Segment{activity.M(amt, metaString(meta, "currency"))})
		}
	case domain.AuditActionSubscriptionThresholdDeferred:
		// ADR-066 §4b: peak/last-style charges can't be split across an early
		// invoice and the cycle close, so the crossing bills at cycle close.
		// One event, one title: the meta says which outcome.
		l.Kind, l.Title = kindThresholdCrossed, "Spend threshold crossed"
		l.Detail = append(l.Detail, activity.Text("Billed at the end of the cycle, because this price bills on peak usage"))
	case domain.AuditActionSubscriptionProrationFailed:
		// B10: what did not happen, per change type. The raw error stays in
		// the audit entry.
		l.Kind, l.Title, l.Tone = kindProrationNotApplied, "Proration not applied", activity.ToneWarning
		ct := domain.ItemChangeType(metaString(meta, "change_type"))
		switch ct {
		case domain.ItemChangeTypeAdd, domain.ItemChangeTypeRemove:
			if p := metaString(meta, "plan_id"); p != "" {
				l.Detail = append(l.Detail, activity.Text(lb.plan(p)))
			}
		case domain.ItemChangeTypePlan:
			if p := planArrow(meta, lb); p != "" {
				l.Detail = append(l.Detail, activity.Text(p))
			}
		case domain.ItemChangeTypeQuantity:
			if q := quantityArrow(meta, "old_quantity", "new_quantity"); q != "" {
				l.Detail = append(l.Detail, activity.Text(q))
			}
		}
		l.Problem = prorationNotAppliedProblem(ct)
	case domain.AuditActionSubscriptionNetNegativeUsage:
		// #1193: an invoice decision counted usage below zero after
		// corrections as 0, and built the rest of the invoice.
		l.Kind, l.Title, l.Tone = kindUsageBelowZero, "Usage below zero billed as zero", activity.ToneWarning
		for _, m := range belowZeroMeters(meta) {
			l.Detail = append(l.Detail, activity.Text(m))
		}
		l.Problem = "Usage corrections were larger than the usage they offset. If an earlier invoice billed that usage, issue a credit note on it."
	default:
		return changedFallback("action", action)
	}
	return l
}

// describeUpdate words an AuditActionUpdate row, by its metadata "action".
func describeUpdate(meta map[string]any, lb lineLabels) activity.Line {
	l := activity.Line{Tone: activity.ToneNeutral}
	switch a := metaString(meta, "action"); a {
	case domain.SubAuditCancelScheduled:
		l.Kind, l.Title = kindCancelScheduled, "Cancellation scheduled"
		if seg := cancelScheduledWhen(meta); seg != nil {
			l.Detail = append(l.Detail, seg)
		}
	case domain.SubAuditCancelCleared:
		l.Kind, l.Title = kindCancelCleared, "Scheduled cancellation cleared"
	case domain.SubAuditCollectionPaused:
		// Warning: invoices will not be collected while it lasts. The title
		// says it all, so there is no problem line; the meta says why or
		// until when.
		l.Kind, l.Title, l.Tone = kindCollectionPaused, "Collection paused", activity.ToneWarning
		resumes, hasResume := metaTime(meta, "resumes_at")
		switch {
		case metaString(meta, "triggered_by") == "dunning":
			l.Detail = append(l.Detail, activity.Text("Payment retries ran out"))
		case hasResume:
			l.Detail = append(l.Detail, activity.Segment{activity.T("Resumes "), activity.At(resumes)})
		default:
			l.Detail = append(l.Detail, activity.Text("Invoices are created but not charged until collection resumes"))
		}
	case domain.SubAuditCollectionResumed:
		l.Kind, l.Title = kindCollectionResumed, "Collection resumed"
		switch metaString(meta, "triggered_by") {
		case "schedule":
			l.Detail = append(l.Detail, activity.Text("On its scheduled date"))
		case "payment":
			// Dunning paused it, and a payment left nothing owed.
			l.Detail = append(l.Detail, activity.Text("The overdue invoice was paid"))
		}
	case domain.SubAuditTrialEnded:
		// B2: the three schedule writers mark the row triggered_by=schedule;
		// only the operator's End trial ends it early.
		if metaString(meta, "triggered_by") == "schedule" {
			l.Kind, l.Title = kindTrialEnded, "Trial ended"
		} else {
			l.Kind, l.Title = kindTrialEndedEarly, "Trial ended early"
		}
		// A trial_ended row is written only when the subscription became
		// active; a trial that ends with a cancellation due writes a cancel
		// row instead (SubAuditTrialEndCancel).
		l.Detail = append(l.Detail, activity.Text("Subscription is now active"))
	case domain.SubAuditTrialExtended:
		l.Kind, l.Title = kindTrialExtended, "Trial extended"
		if t, ok := metaTime(meta, "trial_end"); ok {
			l.Detail = append(l.Detail, activity.Segment{activity.T("Trial now ends "), activity.At(t)})
		}
	case domain.SubAuditBillingRetried:
		// The title matches the "Retry billing" button.
		l.Kind, l.Title = kindBillingRetried, "Billing retried"
		if n, ok := metaInt(meta, "attempts_before"); ok && n > 0 {
			l.Detail = append(l.Detail, activity.Text(failedTriesText(n)))
		}
	case domain.SubAuditThresholdsSet:
		// B9: the amount as money in the subscription's currency, never cents.
		l.Kind, l.Title = kindThresholdsSet, "Spend thresholds set"
		if amt, ok := metaInt(meta, "amount_gte"); ok && amt > 0 {
			l.Detail = append(l.Detail, activity.Segment{activity.M(amt, metaString(meta, "currency")), activity.T(" total")})
		}
		if n, ok := metaInt(meta, "item_threshold_count"); ok && n > 0 {
			l.Detail = append(l.Detail, activity.Text(fmt.Sprintf("%d item threshold%s", n, plural(int(n)))))
		}
	case domain.SubAuditThresholdsCleared:
		l.Kind, l.Title = kindThresholdsRemoved, "Spend thresholds removed"
	case domain.SubAuditItemAdded:
		l.Kind, l.Title = kindItemAdded, "Item added"
		if p := metaString(meta, "plan_id"); p != "" {
			l.Detail = append(l.Detail, activity.Text(lb.plan(p)))
		}
		if q, ok := metaInt(meta, "quantity"); ok && q > 0 {
			l.Detail = append(l.Detail, activity.Text(fmt.Sprintf("Quantity %d", q)))
		}
		if seg := prorationSegment(meta, lb); seg != nil {
			l.Detail = append(l.Detail, seg)
		}
	case domain.SubAuditItemRemoved:
		// No link on the credit: a downgrade credit can be several credit
		// notes or a credit-balance grant, and the row stores none of their
		// ids.
		l.Kind, l.Title = kindItemRemoved, "Item removed"
		if p := metaString(meta, "plan_id"); p != "" {
			l.Detail = append(l.Detail, activity.Text(lb.plan(p)))
		}
		if seg := prorationSegment(meta, lb); seg != nil {
			l.Detail = append(l.Detail, seg)
		}
	case domain.SubAuditPendingPlanChangeCanceled:
		l.Kind, l.Title = kindPlanChangeCanceled, "Scheduled plan change canceled"
	default:
		return changedFallback("update action", a)
	}
	return l
}

// describeItemUpdated words an AuditActionSubscriptionItemUpdated row.
func describeItemUpdated(meta map[string]any, lb lineLabels) activity.Line {
	l := activity.Line{Tone: activity.ToneNeutral}
	switch a := metaString(meta, "action"); a {
	case domain.SubAuditItemPlanChanged:
		immediate, _ := meta["immediate"].(bool)
		arrow := planArrow(meta, lb)
		if immediate {
			l.Kind, l.Title = kindPlanChanged, "Plan changed"
			if arrow != "" {
				l.Detail = append(l.Detail, activity.Text(arrow))
			}
			if seg := prorationSegment(meta, lb); seg != nil {
				l.Detail = append(l.Detail, seg)
			}
			break
		}
		// A change that has not happened yet is not "changed".
		l.Kind, l.Title = kindPlanChangeScheduled, "Plan change scheduled"
		when := "At renewal"
		switch newPlan := metaString(meta, "new_plan_id"); {
		case arrow != "":
			when = arrow + " at renewal"
		case newPlan != "":
			// Rows written before the old plan was recorded on a scheduled
			// change name only the new one, as "To 5" does for a quantity.
			when = "To " + lb.plan(newPlan) + " at renewal"
		}
		if t, ok := metaTime(meta, "effective_at"); ok {
			l.Detail = append(l.Detail, activity.Segment{activity.T(when + " on "), activity.At(t)})
		} else {
			l.Detail = append(l.Detail, activity.Text(when))
		}
	case domain.SubAuditItemQuantityChanged:
		// A quantity change always applies at once (UpdateItem settles it in
		// the current period), whatever `immediate` the caller sent.
		l.Kind, l.Title = kindQuantityChanged, "Quantity changed"
		if p := metaString(meta, "plan_id"); p != "" {
			l.Detail = append(l.Detail, activity.Text(lb.plan(p)))
		}
		if q := quantityArrow(meta, "old_quantity", "quantity"); q != "" {
			l.Detail = append(l.Detail, activity.Text(q))
		} else if n, ok := metaInt(meta, "quantity"); ok {
			l.Detail = append(l.Detail, activity.Text(fmt.Sprintf("To %d", n)))
		}
		if seg := prorationSegment(meta, lb); seg != nil {
			l.Detail = append(l.Detail, seg)
		}
	default:
		return changedFallback("item update action", a)
	}
	return l
}

// changedFallback is S-16: a row no case knows. It logs, because a writer
// emitting a token with no words is a bug the golden test should have caught.
func changedFallback(what, token string) activity.Line {
	slog.Warn("subscription timeline: audit row with no words", "token_kind", what, "token", token)
	return activity.Line{Kind: kindChanged, Title: "Subscription changed", Tone: activity.ToneNeutral}
}

// cancelWhy says why a subscription was canceled when no person did it: the
// schedule, the trial's end, or payment recovery. A person's or API key's
// cancel needs no why; the actor says who.
func cancelWhy(meta map[string]any) string {
	if metaString(meta, "action") == domain.SubAuditTrialEndCancel {
		return "At trial end"
	}
	switch metaString(meta, "canceled_by") {
	case "schedule":
		return "Scheduled cancellation"
	case "dunning":
		return "Payment retries ran out"
	}
	return ""
}

// cancelScheduledWhen is S-4's meta: when the scheduled cancellation lands.
func cancelScheduledWhen(meta map[string]any) activity.Segment {
	if t, ok := metaTime(meta, "cancel_at"); ok {
		return activity.Segment{activity.T("On "), activity.At(t)}
	}
	atPeriodEnd, _ := meta["cancel_at_period_end"].(bool)
	if !atPeriodEnd {
		return nil
	}
	// At period end on a trialing subscription is at trial end (ADR-069).
	label := "At the end of the current period"
	if metaString(meta, "status") == string(domain.SubscriptionTrialing) {
		label = "At trial end"
	}
	if t, ok := metaTime(meta, "cancel_effective_at"); ok {
		return activity.Segment{activity.T(label + " ("), activity.At(t), activity.T(")")}
	}
	return activity.Text(label)
}

// prorationNotAppliedProblem says what did not happen when an item change's
// proration failed, and what to do about it.
func prorationNotAppliedProblem(ct domain.ItemChangeType) string {
	switch ct {
	case domain.ItemChangeTypeAdd:
		return "The item was added, but its prorated charge was not billed. Velox will not retry it; bill it with a one-off invoice."
	case domain.ItemChangeTypeRemove:
		return "The item was removed, but its prorated credit was not issued. Issue a credit note by hand."
	case domain.ItemChangeTypePlan:
		return "The plan changed, but the prorated difference was not billed or credited."
	case domain.ItemChangeTypeQuantity:
		return "The quantity changed, but the prorated difference was not billed or credited."
	}
	return "The change was made, but its prorated amount was not billed or credited."
}

// prorationSegment is the proration outcome a change produced, from the
// metadata addProrationMeta stamps: "Proration invoice VLX-000123 $18.00",
// "Credit $19.80", "Open invoice VLX-000120 adjusted by $54.44". Nil when the
// change produced none.
func prorationSegment(meta map[string]any, lb lineLabels) activity.Segment {
	amt, ok := metaInt(meta, "proration_amount_cents")
	if !ok {
		return nil
	}
	money := activity.M(amt, metaString(meta, "currency"))
	switch metaString(meta, "proration_type") {
	case "invoice":
		if id := metaString(meta, "proration_invoice_id"); lb.invoices[id] != "" {
			return activity.Segment{activity.T("Proration invoice "), activity.L(activity.LinkInvoice, id, lb.invoices[id]), activity.T(" "), money}
		}
		return activity.Segment{activity.T("Proration invoice "), money}
	case "credit":
		return activity.Segment{activity.T("Credit "), money}
	case "adjustment":
		if id := metaString(meta, "adjusted_invoice_id"); lb.invoices[id] != "" {
			return activity.Segment{activity.T("Open invoice "), activity.L(activity.LinkInvoice, id, lb.invoices[id]), activity.T(" adjusted by "), money}
		}
		return activity.Segment{activity.T("Open invoice adjusted by "), money}
	}
	return nil
}

// planArrow is "Starter → Pro" from a row's old and new plan ids; empty when
// either is missing.
func planArrow(meta map[string]any, lb lineLabels) string {
	oldPlan, newPlan := metaString(meta, "old_plan_id"), metaString(meta, "new_plan_id")
	if oldPlan == "" || newPlan == "" {
		return ""
	}
	return lb.plan(oldPlan) + " → " + lb.plan(newPlan)
}

// quantityArrow is "3 → 5"; empty when the old quantity is unknown.
func quantityArrow(meta map[string]any, oldKey, newKey string) string {
	o, okOld := metaInt(meta, oldKey)
	n, okNew := metaInt(meta, newKey)
	if !okOld || !okNew || o <= 0 {
		return ""
	}
	return fmt.Sprintf("%d → %d", o, n)
}

// belowZeroMeters names each meter of a net-negative usage row with its
// amount below zero: by the meter's name, which rows written before it was
// recorded lack, so those show the key they have.
func belowZeroMeters(meta map[string]any) []string {
	var out []string
	ms, _ := meta["meters"].([]any)
	for _, m := range ms {
		mm, _ := m.(map[string]any)
		name := metaString(mm, "meter_name")
		if name == "" {
			name = metaString(mm, "meter_key")
		}
		if qty := metaString(mm, "quantity"); name != "" && qty != "" {
			out = append(out, name+" "+qty)
		}
	}
	return out
}

func failedTriesText(n int64) string {
	if n == 1 {
		return "After 1 failed try"
	}
	return fmt.Sprintf("After %d failed tries", n)
}

// metaString reads a string from audit metadata.
func metaString(meta map[string]any, key string) string {
	s, _ := meta[key].(string)
	return s
}

// metaInt reads a whole number from audit metadata: float64 after the audit
// store's JSON round trip, an integer type straight from a writer's map.
func metaInt(meta map[string]any, key string) (int64, bool) {
	switch v := meta[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// metaTime reads an instant from audit metadata: an RFC 3339 string after
// the JSON round trip, a time.Time straight from a writer's map.
func metaTime(meta map[string]any, key string) (time.Time, bool) {
	switch v := meta[key].(type) {
	case time.Time:
		return v, !v.IsZero()
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		return t, err == nil && !t.IsZero()
	}
	return time.Time{}, false
}

// legacyStatus is the line's tone in the words the timeline's `status` field
// carries, which the Activity card's dot reads.
func legacyStatus(t activity.Tone) string {
	switch t {
	case activity.ToneSuccess:
		return "succeeded"
	case activity.ToneFailure:
		return "failed"
	case activity.ToneWarning:
		return "warning"
	}
	return "neutral"
}

// legacyDetail is the meta line in the older `detail` and `detail_timestamp`
// fields, without the actor, which the card shows from actor_name. The card
// prints detail_timestamp after detail, in the account's timezone, so a time
// part in the last segment becomes detail_timestamp with the words before it
// as the end of detail ("Trial now ends" + the instant). A time part's
// closing ")" has no place there and is left out.
func legacyDetail(l activity.Line) (string, string) {
	var segs []string
	ts := ""
	for i, s := range l.Detail {
		last := i == len(l.Detail)-1
		at := -1
		if last {
			for j, p := range s {
				if p.Time != nil {
					at = j
					break
				}
			}
		}
		if at < 0 {
			if txt := activity.SegmentText(s); txt != "" {
				segs = append(segs, txt)
			}
			continue
		}
		ts = s[at].Time.UTC().Format(time.RFC3339)
		if before := strings.TrimRight(activity.SegmentText(s[:at]), " ("); before != "" {
			segs = append(segs, before)
		}
	}
	return strings.Join(segs, " · "), ts
}
