package invoice

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
)

// The invoice timeline's activity lines (ADR-128). Each describe function
// turns one fact into an activity.Line: title, amount, problem, meta and
// actor. None takes a clock input; paymentTimeline stamps the times after
// describing, so a test-clock line differs from a real-clock one only by its
// Recorded part.

// legacyStatus is the line's tone in the words the timeline's `status`
// field carries, which the Activity card's dot reads.
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

// actorFrom maps an audit row's actor to a line's actor; nil when there is
// no row.
func actorFrom(e *domain.AuditEntry) *activity.Actor {
	if e == nil {
		return nil
	}
	return activity.ActorFromAudit(e.ActorType, e.ActorName)
}

// whyInvoiceExists is the meta segment that says why an invoice was made,
// from its billing_reason. A manual invoice needs none: its actor says who
// made it.
func whyInvoiceExists(inv domain.Invoice) activity.Segment {
	switch inv.BillingReason {
	case domain.BillingReasonSubscriptionCycle:
		if inv.BillingPeriodDisplay != "" {
			return activity.Text("Renewal for " + inv.BillingPeriodDisplay)
		}
		return activity.Text("Renewal")
	case domain.BillingReasonSubscriptionCreate:
		return activity.Text("First invoice")
	case domain.BillingReasonSubscriptionUpdate:
		switch inv.SourceChangeType {
		case domain.ItemChangeTypePlan:
			return activity.Text("Plan change")
		case domain.ItemChangeTypeQuantity:
			return activity.Text("Quantity change")
		case domain.ItemChangeTypeAdd:
			return activity.Text("Item added")
		}
		return activity.Text("Subscription change")
	case domain.BillingReasonThreshold:
		return activity.Text("Spend threshold crossed")
	case domain.BillingReasonSubscriptionCancel:
		return activity.Text("Final invoice after cancellation")
	}
	return nil
}

// createdLine is I-1. sub is the subscription link part, or nil.
func createdLine(inv domain.Invoice, actor *activity.Actor, sub *activity.Part) activity.Line {
	l := activity.Line{Kind: "invoice.created", Title: "Invoice created", Tone: activity.ToneNeutral, Actor: actor}
	if why := whyInvoiceExists(inv); why != nil {
		l.Detail = append(l.Detail, why)
	}
	if sub != nil {
		l.Detail = append(l.Detail, activity.Segment{*sub})
	}
	return l
}

// finalizedLine is I-2. The amount is what the invoice asked for when it was
// finalized: its total, before any credit applied at collection or credit
// note issued later.
func finalizedLine(inv domain.Invoice, actor *activity.Actor) activity.Line {
	return activity.Line{
		Kind: "invoice.finalized", Title: "Invoice finalized", Tone: activity.ToneNeutral,
		Amount: activity.Amount(inv.TotalAmountCents, inv.Currency), Actor: actor,
	}
}

// voidedLine is I-3: voiding is a choice, not a failure, and it cancels the
// amount still owed.
func voidedLine(inv domain.Invoice, actor *activity.Actor) activity.Line {
	return activity.Line{
		Kind: "invoice.voided", Title: "Invoice voided", Tone: activity.ToneNeutral,
		Amount: activity.Amount(inv.AmountDueCents, inv.Currency), Actor: actor,
	}
}

// uncollectibleLine is I-4. writtenOff is the amount the write-off canceled,
// nil when no record says it. byRecovery is set when payment recovery wrote
// the invoice off as its final action.
func uncollectibleLine(inv domain.Invoice, writtenOff *int64, byRecovery bool, actor *activity.Actor) activity.Line {
	l := activity.Line{
		Kind: "invoice.marked_uncollectible", Title: "Invoice marked uncollectible", Tone: activity.ToneWarning, Actor: actor,
	}
	if writtenOff != nil {
		l.Amount = activity.Amount(*writtenOff, inv.Currency)
	}
	if byRecovery {
		l.Detail = append(l.Detail, activity.Text("Payment retries ran out"))
	}
	return l
}

// cardText is the card that paid, "Visa •••• 4242"; empty when unknown.
func cardText(brand, last4 string) string {
	return strings.TrimPrefix(formatPaymentCardDetail(brand, last4), "via ")
}

// retriesText is "After 1 retry" / "After 3 retries".
func retriesText(n int) string {
	if n == 1 {
		return "After 1 retry"
	}
	return fmt.Sprintf("After %d retries", n)
}

// paidLine is I-5 and I-6. retries is the payment-recovery attempt count when
// recovery was running (0 otherwise). note is the reference an operator typed
// with an offline payment.
func paidLine(inv domain.Invoice, retries int, note string, actor *activity.Actor) activity.Line {
	amount := activity.Amount(inv.AmountPaidCents, inv.Currency)
	if strings.HasPrefix(inv.StripePaymentIntentID, "out_of_band:") {
		// "Recorded" is only the timestamp word (ADR-104 Invariant C), and the
		// actor says who entered it.
		l := activity.Line{Kind: "invoice.offline_payment_received", Title: "Offline payment received", Tone: activity.ToneSuccess, Amount: amount, Actor: actor}
		if note = strings.TrimSpace(note); note != "" {
			l.Detail = append(l.Detail, activity.Text(note))
		}
		return l
	}
	l := activity.Line{Kind: "invoice.paid", Title: "Invoice paid", Tone: activity.ToneSuccess, Amount: amount, Actor: actor}
	if c := cardText(inv.PaymentCardBrand, inv.PaymentCardLast4); c != "" {
		l.Detail = append(l.Detail, activity.Text(c))
	}
	if retries > 0 {
		l.Detail = append(l.Detail, activity.Text(retriesText(retries)))
	}
	if inv.StripePaymentIntentID == "" && inv.CreditsAppliedCents > 0 {
		l.Detail = append(l.Detail, activity.Text("Paid with credits"))
	}
	return l
}

// attemptActor names who made a charge attempt, from the attempt's trigger:
// a dunning retry is always Velox's own. The other triggers cannot yet tell
// an operator's "Charge now" from an automatic charge, or say where the
// customer paid, so they name no one until the attempt records its actor.
func attemptActor(a domain.InvoiceChargeAttempt) *activity.Actor {
	if a.Trigger == domain.ChargeTriggerDunningRetry {
		return activity.Automatic()
	}
	return nil
}

// attemptLine is I-7a/b/c: one charge attempt the timeline shows as its own
// row.
func attemptLine(a domain.InvoiceChargeAttempt, inv domain.Invoice) activity.Line {
	l := activity.Line{Amount: activity.Amount(a.AmountCents, inv.Currency), Actor: attemptActor(a)}
	switch a.Outcome {
	case domain.ChargeAttemptFailed:
		l.Kind, l.Title, l.Tone = "payment.failed", "Payment failed", activity.ToneFailure
		l.Problem = activity.DeclineReason(a.DeclineCode, a.ProviderReason)
	case domain.ChargeAttemptUnknown:
		l.Kind, l.Title, l.Tone = "payment.result_unknown", "Payment result unknown", activity.ToneWarning
		l.Problem = "The payment provider has not confirmed whether this charge went through."
	case domain.ChargeAttemptSucceeded:
		// Only shown on an invoice that is not paid: the row exists for the
		// anomaly, so it says it.
		l.Kind, l.Title, l.Tone = "payment.succeeded", "Payment succeeded", activity.ToneWarning
		l.Problem = "The invoice is not marked paid yet."
	default:
		// pending: in flight. renderChargeAttempts never shows it (the
		// attention banner owns it); the words are here so no outcome
		// borrows another's.
		l.Kind, l.Title, l.Tone = "payment.pending", "Payment in progress", activity.ToneNeutral
	}
	return l
}

// creditNoteLine is I-8: one title for every credit note, its total as the
// amount, and the parts saying where the total went. refundFailure is the
// provider's reason for a failed card refund, from the credit note's audit
// trail; empty when none was recorded.
func creditNoteLine(cn domain.CreditNote, actor *activity.Actor, refundFailure string) activity.Line {
	l := activity.Line{
		Kind: "credit_note.issued", Title: "Credit note issued", Tone: activity.ToneSuccess,
		Amount: activity.Amount(cn.TotalCents, cn.Currency), Actor: actor,
		Detail: []activity.Segment{{activity.L(activity.LinkCreditNote, cn.ID, cn.CreditNoteNumber)}},
	}
	if r := strings.TrimSpace(cn.Reason); r != "" {
		l.Detail = append(l.Detail, activity.Text(activity.CreditNoteReason(r)))
	}
	refunded := cn.RefundAmountCents > 0 && (cn.RefundStatus == domain.RefundSucceeded || cn.RefundStatus == "")
	if cn.RefundAmountCents > 0 && !refunded {
		// The card part names the allocation; it says "refunded" only once the
		// money moved, and the problem line says what happened instead.
		l.Detail = append(l.Detail, activity.Segment{activity.M(cn.RefundAmountCents, cn.Currency), activity.T(" to card")})
		l.Detail = append(l.Detail, activity.RefundParts(0, cn.CreditAmountCents, cn.OutOfBandAmountCents, cn.Currency)...)
	} else {
		l.Detail = append(l.Detail, activity.RefundParts(cn.RefundAmountCents, cn.CreditAmountCents, cn.OutOfBandAmountCents, cn.Currency)...)
	}
	if cn.RefundAmountCents > 0 && !refunded {
		switch cn.RefundStatus {
		case domain.RefundFailed:
			// Stripe's refund failure_reason is a code
			// ("expired_or_canceled_card"); "unknown" says nothing.
			l.Tone = activity.ToneFailure
			l.Problem = "Card refund failed."
			if r := humanizeCode(refundFailure); r != "" && strings.TrimSpace(refundFailure) != "unknown" {
				l.Problem = "Card refund failed: " + strings.ToLower(r[:1]) + r[1:]
			}
		case domain.RefundPending:
			l.Tone, l.Problem = activity.ToneWarning, "The card refund is waiting on the payment provider."
		case domain.RefundUnknown:
			// The customer may already be refunded; "not made" would invite a
			// second refund by hand (#965).
			l.Tone, l.Problem = activity.ToneWarning, "The payment provider did not say whether the card refund went through."
		default: // none with an allocation: the refund leg never ran
			l.Tone, l.Problem = activity.ToneWarning, "No refund was sent to the payment provider."
		}
	}
	return l
}

// emailCause is the setup-link email's cause, from the trigger it was
// enqueued with.
func emailCause(trigger string) string {
	switch trigger {
	case "finalize_no_pm":
		return "No payment method when the invoice was finalized"
	case "auto_charge_retry_no_pm":
		return "A charge found no payment method"
	case "operator_resend":
		// Who resent it is not recorded with the row yet; the actor replaces
		// this once it is.
		return "Resent"
	}
	return ""
}

// emailActor names who sent an email when the email itself proves it:
// receipts, failed-payment notices and payment-recovery emails are only ever
// sent by Velox, and so is a setup email whose trigger is automatic. An
// invoice or credit-note email, or a resend, is an operator's act whose actor
// is not recorded with the row yet.
func emailActor(emailType, trigger string) *activity.Actor {
	switch emailType {
	case "payment_receipt", "payment_failed", "dunning_warning", "dunning_escalation":
		return activity.Automatic()
	case "payment_setup_request":
		if trigger == "finalize_no_pm" || trigger == "auto_charge_retry_no_pm" {
			return activity.Automatic()
		}
	}
	return nil
}

// emailLine is I-9 to I-13: the email's name and its outcome word. cn is the
// credit note a credit-note email names, as a link part, or nil.
func emailLine(evt EmailEventRow, cn *activity.Part) (activity.Line, bool) {
	name, ok := activity.EmailName(evt.EmailType)
	if !ok {
		return activity.Line{}, false
	}
	out := activity.Outcome(evt.Status, evt.DeliveryState, evt.SendFailure)
	l := activity.Line{
		Kind: "email." + evt.EmailType, Title: name + " " + out.Word, Tone: out.Tone, Problem: out.Problem,
		Actor: emailActor(evt.EmailType, evt.Trigger),
	}
	if out.Reason != "" {
		l.Detail = append(l.Detail, activity.Text(out.Reason))
	}
	if evt.EmailType == "payment_setup_request" {
		if c := emailCause(evt.Trigger); c != "" {
			l.Detail = append(l.Detail, activity.Text(c))
		}
	}
	if cn != nil {
		l.Detail = append(l.Detail, activity.Segment{*cn})
	}
	if to := strings.TrimSpace(evt.To); to != "" {
		l.Detail = append(l.Detail, activity.Text("To "+to))
	}
	return l, true
}

// emailClauseOf is a payment-recovery email's one appearance: a clause in the
// meta of the retry or retries-ran-out row it belongs to.
func emailClauseOf(evt EmailEventRow) activity.Segment {
	if c := activity.EmailClause(evt.EmailType, evt.Status, evt.DeliveryState, evt.SendFailure); c != "" {
		return activity.Text(c)
	}
	return nil
}

// isNoPMRetry reports a retry that charged nothing because there was no
// card: the cause vocabulary, or the legacy sentinel our own single writer
// stored before normalization.
func isNoPMRetry(reason string) bool {
	return reason == string(domain.DunningCauseNoPaymentMethod) || reason == domain.ErrNoPaymentMethodOnRetry.Error()
}

// The outcome tokens an escalated event's reason joins with "+"
// (domain.DunningEscalationOutcome.Reason).
const (
	tokenSubscriptionPaused   = "subscription_paused"
	tokenSubscriptionCanceled = "subscription_canceled"
	tokenInvoiceWrittenOff    = "invoice_marked_uncollectible"
)

// escalationTokens splits an escalated event's reason into its outcome
// tokens.
func escalationTokens(reason string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(reason, "+") {
		if t = strings.TrimSpace(t); t != "" {
			out[t] = true
		}
	}
	return out
}

// escalationProblem says what exhaustion did, from the outcome tokens.
// writeOffShown leaves the write-off out when the invoice's own
// "Invoice marked uncollectible" row already says it.
func escalationProblem(reason string, writeOffShown bool) string {
	tokens := escalationTokens(reason)
	if len(tokens) == 0 {
		return "Nothing else changes; the invoice stays open."
	}
	var parts []string
	if tokens[tokenSubscriptionPaused] {
		parts = append(parts, "Collection paused")
	}
	if tokens[tokenSubscriptionCanceled] {
		parts = append(parts, "Subscription canceled")
	}
	if tokens[tokenInvoiceWrittenOff] && !writeOffShown {
		parts = append(parts, "Invoice marked uncollectible")
	}
	for t := range tokens {
		if t != tokenSubscriptionPaused && t != tokenSubscriptionCanceled && t != tokenInvoiceWrittenOff {
			slog.Warn("invoice timeline: escalated event carries an outcome token with no words", "token", t)
		}
	}
	return strings.Join(parts, " · ")
}

// recoveryOutcome is a payment-recovery run's end, from the run's resolution
// column: the enum the 0170 split corrected, and the one the Dunning page's
// badge reads, so the two can't disagree.
func recoveryOutcome(res domain.DunningResolution) (string, activity.Tone, bool) {
	switch res {
	case domain.ResolutionPaymentRecovered:
		return "Invoice paid", activity.ToneSuccess, true
	case domain.ResolutionInvoiceVoided:
		return "Invoice voided", activity.ToneNeutral, true
	case domain.ResolutionInvoiceNotCollectible:
		return "Invoice marked uncollectible", activity.ToneNeutral, true
	case domain.ResolutionManuallyResolved:
		// Legacy, no writer since 0170. The one kind of run that still holds
		// it is one whose invoice never changed state, so neither "voided"
		// nor "marked uncollectible" is true for it (B16).
		return "Closed unpaid", activity.ToneNeutral, true
	}
	return "", activity.ToneNeutral, false
}

// dunningContext is what a payment-recovery row needs besides its event.
type dunningContext struct {
	run domain.InvoiceDunningRun
	// writeOffShown: the invoice has its own "marked uncollectible" row.
	writeOffShown bool
	// email is the payment-recovery email this row carries as a clause.
	email *EmailEventRow
	// sub links the subscription an escalation paused or canceled.
	sub *activity.Part
	// resolvedBy is the actor of an operator's resolve, from the run's audit
	// row; nil when no such row exists.
	resolvedBy *activity.Actor
}

// dunningLine is I-14 to I-17, and I-18 for an event type with no writer.
func dunningLine(evt domain.InvoiceDunningEvent, dc dunningContext) activity.Line {
	l := activity.Line{Tone: activity.ToneNeutral, Actor: activity.Automatic()}
	switch evt.EventType {
	case domain.DunningEventStarted:
		l.Kind, l.Title = "payment_recovery.started", "Payment recovery started"
		if evt.Reason == string(domain.DunningCauseNoPaymentMethod) {
			l.Tone = activity.ToneWarning
			l.Problem = "No payment method on file. Reminders go out until the customer adds one."
		}
		// After a decline, the Payment failed row above it says why.
	case domain.DunningEventRetryAttempted:
		switch {
		case evt.Reason == "succeeded":
			// Folded into "Invoice paid" when the invoice is paid; a recovery
			// on an invoice that is not marked paid is the anomaly I-7c names.
			l.Kind, l.Title, l.Tone = "payment.succeeded", "Payment succeeded", activity.ToneWarning
			l.Problem = "The invoice is not marked paid yet."
		case isNoPMRetry(evt.Reason):
			l.Kind, l.Title, l.Tone = "payment_recovery.retry_not_attempted", fmt.Sprintf("Payment retry %d not attempted", evt.AttemptCount), activity.ToneWarning
			l.Problem = "No payment method on file"
		case evt.Reason == domain.DunningRetryReasonCardRefused:
			l.Kind, l.Title, l.Tone = "payment_recovery.retry_not_attempted", fmt.Sprintf("Payment retry %d not attempted", evt.AttemptCount), activity.ToneWarning
			l.Problem = "The bank refused this card for good. The customer needs to add a different card."
		default:
			// A real decline. The attempt row's decline code, when it is
			// absorbed, replaces this provider text (renderChargeAttempts).
			l.Kind, l.Title, l.Tone = "payment_recovery.retry_failed", fmt.Sprintf("Payment retry %d failed", evt.AttemptCount), activity.ToneFailure
			l.Problem = activity.StripWrapper(evt.Reason)
		}
		if dc.email != nil && evt.Reason != "succeeded" {
			if c := emailClauseOf(*dc.email); c != nil {
				l.Detail = append(l.Detail, c)
			}
		}
	case domain.DunningEventEscalated:
		l.Kind, l.Title, l.Tone = "payment_recovery.retries_ran_out", "Payment retries ran out", activity.ToneWarning
		l.Problem = escalationProblem(evt.Reason, dc.writeOffShown)
		tokens := escalationTokens(evt.Reason)
		if dc.sub != nil && (tokens[tokenSubscriptionPaused] || tokens[tokenSubscriptionCanceled]) {
			l.Detail = append(l.Detail, activity.Segment{*dc.sub})
		}
		if dc.email != nil {
			if c := emailClauseOf(*dc.email); c != nil {
				l.Detail = append(l.Detail, c)
			}
		}
	case domain.DunningEventResolved:
		l.Kind, l.Title = "payment_recovery.ended", "Payment recovery ended"
		// Automatic unless the run's audit trail names an operator's resolve;
		// a legacy run with no such row names no one.
		l.Actor = dc.resolvedBy
		if l.Actor == nil && dc.run.Resolution != domain.ResolutionManuallyResolved {
			l.Actor = activity.Automatic()
		}
		if outcome, tone, ok := recoveryOutcome(dc.run.Resolution); ok {
			l.Detail = append(l.Detail, activity.Text(outcome))
			l.Tone = tone
		} else {
			slog.Warn("invoice timeline: resolved payment-recovery run has a resolution with no words",
				"run_id", dc.run.ID, "resolution", string(dc.run.Resolution))
		}
	default:
		slog.Warn("invoice timeline: payment-recovery event type with no words", "event_type", string(evt.EventType), "event_id", evt.ID)
		l.Kind, l.Title = "payment_recovery.updated", "Payment recovery updated"
	}
	return l
}

// humanizeCode turns a provider's snake_case code into plain words:
// "product_not_received" → "Product not received".
func humanizeCode(code string) string {
	s := strings.TrimSpace(strings.ReplaceAll(code, "_", " "))
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// disputeLines are the dispute rows (#979): "Payment disputed" when Stripe
// opened it, and "Dispute closed" with its outcome once Stripe closed it. A
// dispute is reported by the cardholder's bank, which is not one of the
// actors a line names, so these rows carry none.
func disputeLines(d domain.InvoiceDispute) (opened activity.Line, closed *activity.Line) {
	id := activity.Text("Stripe dispute " + d.StripeDisputeID)
	opened = activity.Line{
		Kind: "dispute.opened", Title: "Payment disputed", Tone: activity.ToneWarning,
		Amount: activity.Amount(d.AmountCents, d.Currency), Detail: []activity.Segment{id},
	}
	if r := humanizeCode(d.Reason); r != "" {
		opened.Detail = append(opened.Detail, activity.Text("Reason: "+r))
	}
	if !d.Status.Closed() && d.EvidenceDueBy != nil {
		opened.Detail = append(opened.Detail, activity.Segment{activity.T("Respond in Stripe by "), activity.At(*d.EvidenceDueBy)})
	}
	if d.ClosedAt == nil {
		return opened, nil
	}
	c := activity.Line{Kind: "dispute.closed", Title: "Dispute closed"}
	switch d.Status {
	case domain.DisputeWon:
		c.Tone, c.Amount = activity.ToneSuccess, activity.Amount(d.AmountCents, d.Currency)
		c.Detail = []activity.Segment{activity.Text("Won"), id}
	case domain.DisputeLost:
		c.Tone, c.Amount = activity.ToneFailure, activity.Amount(d.AmountCents, d.Currency)
		c.Problem = "Lost: the bank took the payment back."
		c.Detail = []activity.Segment{id}
	case domain.DisputeWarningClosed:
		c.Tone = activity.ToneNeutral
		c.Detail = []activity.Segment{activity.Text("Inquiry closed without a chargeback"), id}
	case domain.DisputePrevented:
		c.Tone = activity.ToneNeutral
		c.Detail = []activity.Segment{activity.Text("Prevented before a chargeback"), id}
	default:
		// A closed status with no words: the webhook refuses statuses it
		// does not know, so this needs a new Stripe status added to the
		// domain without words here.
		slog.Warn("invoice timeline: closed dispute status with no words", "status", string(d.Status), "dispute_id", d.ID)
		c.Tone = activity.ToneNeutral
		c.Detail = []activity.Segment{id}
	}
	return opened, &c
}
