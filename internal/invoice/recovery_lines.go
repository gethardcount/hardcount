package invoice

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/activity"
)

// Payment-recovery (dunning) rows are told on two pages: the invoice's
// Activity card and the Dunning page's recovery timeline. Both are built
// here, by the same code from the same sources, so an event reads the same
// on both (ADR-128 rule 1, §3.3). The pages differ only in the invoice
// page's folds.

// recoveryEmails splits an invoice's emails into the ones the invoice page
// shows as rows of their own and the payment-recovery emails, which are
// clauses on the row they belong to. A reminder pairs with its retry by the
// payload's attempt_number = run.AttemptCount at send time (an exact key);
// the payload carries no run id, so across several runs on one invoice a
// repeated attempt number keeps the newest email.
func recoveryEmails(list []EmailEventRow) (own []EmailEventRow, reminders map[int]EmailEventRow, escalation *EmailEventRow) {
	reminders = map[int]EmailEventRow{}
	for _, evt := range list {
		switch evt.EmailType {
		case "dunning_warning":
			if evt.AttemptNumber > 0 {
				reminders[evt.AttemptNumber] = evt
			}
		case "dunning_escalation":
			e := evt
			escalation = &e
		default:
			own = append(own, evt)
		}
	}
	return own, reminders, escalation
}

// resolvedByIn is the operator who resolved a run, from the run's audit
// rows; nil when no resolve row names one.
func resolvedByIn(entries []domain.AuditEntry) *activity.Actor {
	for _, e := range entries {
		if e.Action == domain.AuditActionUpdate && e.Metadata["action"] == "resolved" {
			ec := e
			return actorFrom(&ec)
		}
	}
	return nil
}

// recoveryRowsInput is what the payment-recovery rows of one invoice need.
type recoveryRowsInput struct {
	inv        domain.Invoice
	runs       []dunningRunEvents
	reminders  map[int]EmailEventRow
	escalation *EmailEventRow
	// sub links the subscription an escalation paused or canceled.
	sub *activity.Part
	// resolvedBy reads who resolved a run; called only for a resolved row
	// that is shown.
	resolvedBy func(runID string) *activity.Actor
	// fold is the invoice page's: a row the invoice's own lifecycle row
	// already tells is left out ("Payment recovery ended" beside "Invoice
	// paid", the recovering retry inside "Invoice paid · After N retries"),
	// and "Payment retries ran out" leaves out the write-off that "Invoice
	// marked uncollectible" says. The Dunning page has no lifecycle rows,
	// so it folds nothing.
	fold bool
}

// recoveryRows builds one row per payment-recovery event, in run order.
func recoveryRows(in recoveryRowsInput) []TimelineEvent {
	inv := in.inv
	var out []TimelineEvent
	for _, re := range in.runs {
		for _, evt := range re.events {
			if in.fold {
				// Gated on the invoice's own transition FIELDS — never the
				// event's reason string, which each resolver spells
				// differently. A run that closed while its invoice never
				// transitioned (such as retries_exhausted with a pause) sets
				// none of the fields, so its row stays as the only record.
				if evt.EventType == domain.DunningEventResolved &&
					(inv.PaidAt != nil || inv.UncollectibleAt != nil || inv.VoidedAt != nil) {
					continue
				}
				if evt.EventType == domain.DunningEventRetryAttempted && evt.Reason == "succeeded" && inv.PaidAt != nil {
					continue
				}
			}
			dc := dunningContext{
				run: re.run, writeOffShown: in.fold && inv.UncollectibleAt != nil, sub: in.sub,
				invoicePaid: inv.PaidAt != nil,
			}
			switch evt.EventType {
			case domain.DunningEventRetryAttempted:
				if em, ok := in.reminders[evt.AttemptCount]; ok {
					dc.email = &em
				}
			case domain.DunningEventEscalated:
				dc.email = in.escalation
			case domain.DunningEventResolved:
				if in.resolvedBy != nil {
					dc.resolvedBy = in.resolvedBy(re.run.ID)
				}
			}
			var recorded time.Time
			if inv.IsSimulated && evt.RecordedAt != nil {
				recorded = *evt.RecordedAt
			}
			out = append(out, TimelineEvent{
				ID:           "dunning:" + evt.ID,
				Timestamp:    evt.CreatedAt.Format(time.RFC3339),
				sortAt:       evt.CreatedAt,
				tieRank:      dunningEventRank(evt.EventType),
				Source:       "dunning",
				EventType:    string(evt.EventType),
				AttemptCount: evt.AttemptCount,
				IsSimulated:  inv.IsSimulated,
				RecordedAt:   rfc3339OrEmpty(recorded),
				recordedSort: recorded,
				// Exact-attribution key written by the dunning retry path
				// (RetryError); empty on legacy rows.
				PaymentIntentID: piFromEventMetadata(evt.Metadata),
				Line:            dunningLine(evt, dc),
				dunningEventID:  evt.ID,
			})
		}
	}
	return out
}

// recoveryRowsByPI indexes the payment-recovery rows by the PaymentIntent
// their charge made, the exact key a charge attempt is matched by.
func recoveryRowsByPI(events []TimelineEvent) map[string]int {
	idx := map[string]int{}
	for i, e := range events {
		if e.Source == "dunning" && e.PaymentIntentID != "" {
			if _, dup := idx[e.PaymentIntentID]; !dup {
				idx[e.PaymentIntentID] = i
			}
		}
	}
	return idx
}

// liftAttempt hands a payment-recovery row the facts of the charge it made.
// The attempt owns payment truth (ADR-103): its decline code, mapped to
// words about the cardholder, or else its verbatim provider reason,
// replaces the dunning event's internally-wrapped copy ("payment failed:
// Your card was declined."), and its amount is the row's.
func liftAttempt(d *TimelineEvent, a domain.InvoiceChargeAttempt, inv domain.Invoice) {
	if d.Line.Tone == activity.ToneFailure {
		if p := activity.DeclineReason(a.DeclineCode, a.ProviderReason); p != "" {
			d.Line.Problem = p
		}
	}
	if d.Line.Amount == nil {
		d.Line.Amount = activity.Amount(a.AmountCents, inv.Currency)
	}
}

// RecoveryRunLines words one payment-recovery run's events for the Dunning
// page's recovery timeline: the lines the invoice's Activity card shows for
// them, from the same sources (the run's reminder and final-notice emails,
// its charge attempts, who resolved it), without the invoice page's folds.
// So the run's end shows, a retry that recovered the invoice shows as its
// own row, and "Payment retries ran out" names a write-off.
//
// lines is keyed by event id. degraded names each side lane that could not
// be read, the way the invoice timeline discloses it; a line missing a
// lane says less, and the lane is named. err is set only when the run's
// invoice cannot be read, and then no line is built.
func (h *Handler) RecoveryRunLines(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, events []domain.InvoiceDunningEvent) (map[string]activity.Line, []string, error) {
	inv, err := h.svc.Get(ctx, tenantID, run.InvoiceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the run's invoice: %w", err)
	}
	degraded := []string{}
	degrade := func(lane string, err error) {
		slog.ErrorContext(ctx, "recovery timeline: lane degraded", "lane", lane, "run_id", run.ID, "invoice_id", inv.ID, "error", err)
		for _, l := range degraded {
			if l == lane {
				return
			}
		}
		degraded = append(degraded, lane)
	}

	reminders := map[int]EmailEventRow{}
	var escalation *EmailEventRow
	if h.emailEvents != nil {
		if list, err := h.emailEvents.ListByInvoice(ctx, tenantID, inv.InvoiceNumber); err != nil {
			degrade("emails", err)
		} else {
			_, reminders, escalation = recoveryEmails(list)
		}
	}
	resolvedBy := func(runID string) *activity.Actor {
		if h.auditStamps == nil {
			return nil
		}
		entries, err := h.auditStamps.ListByResource(ctx, tenantID, "dunning_run", runID)
		if err != nil {
			degrade("audit", err)
			return nil
		}
		return resolvedByIn(entries)
	}

	rows := recoveryRows(recoveryRowsInput{
		inv: inv, runs: []dunningRunEvents{{run: run, events: events}}, reminders: reminders, escalation: escalation,
		sub: h.subscriptionLink(ctx, tenantID, inv), resolvedBy: resolvedBy, fold: false,
	})
	if attempts, err := h.svc.ListChargeAttempts(ctx, tenantID, inv.ID); err != nil {
		degrade("charge_attempts", err)
	} else {
		byPI := recoveryRowsByPI(rows)
		for _, a := range attempts {
			// A pending attempt is still in flight and lends nothing.
			if a.Outcome == domain.ChargeAttemptPending || a.StripePaymentIntentID == "" {
				continue
			}
			if i, ok := byPI[a.StripePaymentIntentID]; ok {
				liftAttempt(&rows[i], a, inv)
			}
		}
	}

	lines := make(map[string]activity.Line, len(rows))
	for i := range rows {
		rows[i].fillFromLine()
		lines[rows[i].dunningEventID] = rows[i].Line
	}
	return lines, degraded, nil
}
