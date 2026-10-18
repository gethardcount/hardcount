package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
)

// The interrupted-charge check (ADR-127, #976).
//
// Every charge writes a record on its invoice before it calls Stripe and
// clears it once its result is saved (chargeInvoice). A record that is still
// there 10 minutes later belongs to a charge that was interrupted — a crash,
// a deploy, the caller's deadline ending during the call — or that got an
// unclear answer. While it is there no path charges the invoice. This check
// asks Stripe what happened to it and ends it the way Stripe says:
//
//   - found, succeeded:       record the payment (SettleSucceeded)
//   - found, declined:        record the decline (SettleFailed), dunning starts
//   - found, still running:   stamp it processing; the processing sweep and the
//     webhook take over. If the invoice moved on meanwhile, cancel a
//     requires_* PaymentIntent (it never ends on its own) or wait for a
//     processing one (it always ends).
//   - not found:              Stripe never got the request; release the
//     invoice and move its counter, so it is charged normally under a new key
//   - Stripe unreadable for 24 hours: park the invoice (ADR-107)
//
// Why "not found" is final. Four facts hold together: List is a consistent
// read (unlike Search); the customer was just shown to exist in this Stripe
// account, and the key belongs to this attempt alone; the record is at least
// interruptedChargeMinAge old, and no request for it can start or be retried
// more than chargeSendBudget after it was written; and Stripe saves a result
// for a key only once execution has begun.

const (
	// chargeSendBudget bounds a charge from its record write to its last
	// Stripe request: stripe-go's three requests of at most 30 s fit in it.
	chargeSendBudget = 2 * time.Minute
	// interruptedChargeMinAge is how old a charge record must be before the
	// check asks Stripe about it. It is more than three send budgets
	// (TestChargeRecord_TimeBudgets), so nothing can still be on its way to
	// Stripe for the record when the check looks.
	interruptedChargeMinAge = 10 * time.Minute
	// interruptedChargeGiveUpAge is how long a record waits for a successful
	// read of Stripe before the invoice parks.
	interruptedChargeGiveUpAge = 24 * time.Hour
	// interruptedChargeListMargin widens the list below the record's start,
	// so a skew between the database's clock and Stripe's cannot drop the
	// PaymentIntent from it.
	interruptedChargeListMargin = time.Hour
	// chargeRecordEndTimeout bounds clearing a record on a detached context.
	chargeRecordEndTimeout = 10 * time.Second
)

// Results for velox_interrupted_charge_resolutions_total.
const (
	interruptedSucceeded  = "succeeded"
	interruptedDeclined   = "declined"
	interruptedProcessing = "processing"
	interruptedCanceled   = "canceled"
	interruptedNotFound   = "not_found"
	interruptedParked     = "parked"
)

// sweepInterruptedCharges is the check's sweep, first in Run: a release lets
// the same tick's auto-charge sweep charge the invoice under its new key.
func (r *Reconciler) sweepInterruptedCharges(ctx context.Context, limit int) (int, []error) {
	rows, err := r.invoices.ListInterruptedCharges(ctx, interruptedChargeMinAge, interruptedChargeGiveUpAge, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list interrupted charges: %w", err)}
	}
	var errs []error
	resolved := 0
	for _, row := range rows {
		done, err := r.checkInterruptedCharge(ctx, row)
		if err != nil {
			errs = append(errs, fmt.Errorf("interrupted charge on invoice %s: %w", row.InvoiceID, err))
		}
		if done {
			resolved++
		}
	}
	return resolved, errs
}

// checkInterruptedCharge ends one record. done reports whether it did. A
// panic is turned into an error for this record alone: the record stays (the
// next run checks it again), and the other records and sweeps still run.
func (r *Reconciler) checkInterruptedCharge(ctx context.Context, row domain.InterruptedCharge) (done bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "interrupted-charge check panicked; the charge record stays and the next run checks it again",
				"invoice_id", row.InvoiceID, "tenant_id", row.TenantID, "panic", p)
			done, err = false, fmt.Errorf("panic: %v", p)
		}
	}()
	// Per-tenant Stripe credentials, as in the other sweeps: the background
	// context carries no tenant.
	ctx = auth.WithTenantID(ctx, row.TenantID)

	results, rerr := r.listForRecord(ctx, row)
	if rerr != nil {
		return r.failedRead(ctx, row, rerr)
	}
	pi := matchAttempt(ctx, row, results)
	if pi == nil {
		return r.releaseNotFound(ctx, row)
	}
	switch pi.Status {
	case "succeeded":
		return r.settleFoundSucceeded(ctx, row, *pi)
	case "requires_payment_method", "canceled":
		return r.settleFoundDeclined(ctx, row, *pi)
	case "processing", "requires_action", "requires_confirmation", "requires_capture":
		return r.stampFoundRunning(ctx, row, *pi)
	default:
		r.recordSync(ctx, domain.Invoice{ID: row.InvoiceID, TenantID: row.TenantID}, pi.Status)
		return false, fmt.Errorf("unexpected PaymentIntent status %q for key %s", pi.Status, row.Key)
	}
}

// listForRecord reads the record's Stripe customer's PaymentIntents, behind
// the breaker. An open breaker is a failed read like any other.
func (r *Reconciler) listForRecord(ctx context.Context, row domain.InterruptedCharge) ([]PaymentIntentResult, error) {
	since := row.StartedAt.Add(-interruptedChargeListMargin)
	if r.breaker == nil {
		return r.client.ListPaymentIntentsForCustomer(ctx, row.StripeCustomerID, since)
	}
	out, err := r.breaker.Execute(ctx, func(ctx context.Context) (any, error) {
		return r.client.ListPaymentIntentsForCustomer(ctx, row.StripeCustomerID, since)
	})
	if err != nil {
		return nil, err
	}
	results, _ := out.([]PaymentIntentResult)
	return results, nil
}

// matchAttempt returns the PaymentIntent the record's charge created: the one
// whose velox_attempt_key is the record's key. The key names the invoice, the
// attempt, the charge kind and the card, so another attempt's PaymentIntent
// for the same invoice never matches. One key gives at most one
// PaymentIntent; if Stripe shows more, that is logged, and a succeeded one
// wins over one still running, which wins over a failed one.
func matchAttempt(ctx context.Context, row domain.InterruptedCharge, results []PaymentIntentResult) *PaymentIntentResult {
	var best *PaymentIntentResult
	n := 0
	for i := range results {
		if results[i].AttemptKey != row.Key {
			continue
		}
		n++
		if best == nil || attemptRank(results[i].Status) > attemptRank(best.Status) {
			best = &results[i]
		}
	}
	if n > 1 {
		slog.ErrorContext(ctx, "more than one PaymentIntent carries one charge's idempotency key; acting on the most advanced",
			"invoice_id", row.InvoiceID, "idempotency_key", row.Key, "matches", n, "chosen_payment_intent_id", best.ID)
	}
	return best
}

func attemptRank(status string) int {
	switch status {
	case "succeeded":
		return 3
	case "processing", "requires_action", "requires_confirmation", "requires_capture":
		return 2
	default:
		return 1
	}
}

// failedRead keeps the record: an error is never "no such charge". It stamps
// the check time, so the record waits interruptedChargeMinAge before the next
// read, and after interruptedChargeGiveUpAge it parks the invoice instead. A
// disconnected Stripe (ErrStripeNotConfigured) is a failed read too, or the
// invoice would stay blocked for good.
func (r *Reconciler) failedRead(ctx context.Context, row domain.InterruptedCharge, rerr error) (bool, error) {
	if row.PastGiveUp {
		return r.giveUp(ctx, row, rerr)
	}
	r.recordSync(ctx, domain.Invoice{ID: row.InvoiceID, TenantID: row.TenantID}, "attempt_check_error")
	if !errors.Is(rerr, breaker.ErrOpen) {
		slog.WarnContext(ctx, "interrupted-charge check could not read Stripe; the charge record stays and the next run asks again",
			"invoice_id", row.InvoiceID, "tenant_id", row.TenantID, "idempotency_key", row.Key,
			"stripe_customer_id", row.StripeCustomerID, "error", rerr)
	}
	return false, nil
}

// giveUp parks an invoice whose record Stripe could not be asked about for
// interruptedChargeGiveUpAge (ADR-107): the park and the clear are one
// statement, with the park's existing CRITICAL. If the invoice moved on
// meanwhile, only the record clears, at ERROR, naming the key.
func (r *Reconciler) giveUp(ctx context.Context, row domain.InterruptedCharge, rerr error) (bool, error) {
	reason := fmt.Sprintf("Stripe could not be read about this charge for %s: %v", interruptedChargeGiveUpAge, rerr)
	parked, cleared, err := r.invoices.ParkInterruptedCharge(ctx, row.TenantID, row.InvoiceID, row.Seq, row.StartedAt, reason)
	if err != nil {
		return false, fmt.Errorf("park interrupted charge: %w", err)
	}
	switch {
	case parked:
		inv, gerr := r.invoices.Get(ctx, row.TenantID, row.InvoiceID)
		if gerr != nil {
			inv = domain.Invoice{ID: row.InvoiceID, TenantID: row.TenantID}
		}
		logParked(ctx, row.TenantID, inv, row.Key, reason)
		mw.RecordInterruptedChargeResolution(interruptedParked)
		return true, nil
	case cleared:
		slog.ErrorContext(ctx, "an interrupted charge could not be checked with Stripe for 24 hours, and its invoice has moved on; its record is cleared. If Stripe took this charge, find it in the Stripe dashboard by idempotency key",
			"invoice_id", row.InvoiceID, "tenant_id", row.TenantID, "idempotency_key", row.Key,
			"stripe_customer_id", row.StripeCustomerID, "error", rerr)
		return true, nil
	default:
		return false, nil // the record changed since it was listed
	}
}

// releaseNotFound: Stripe has no PaymentIntent for the key, so the request
// never reached it. Clear the record and move the counter, and the invoice is
// charged normally under a new key.
func (r *Reconciler) releaseNotFound(ctx context.Context, row domain.InterruptedCharge) (bool, error) {
	released, seqMoved, err := r.invoices.ReleaseInterruptedCharge(ctx, row.TenantID, row.InvoiceID, row.Seq, row.StartedAt)
	if err != nil {
		return false, fmt.Errorf("release interrupted charge: %w", err)
	}
	if !released {
		return false, nil // the record changed since it was listed
	}
	slog.WarnContext(ctx, "interrupted charge never reached Stripe; the invoice will be charged normally",
		"invoice_id", row.InvoiceID, "tenant_id", row.TenantID, "idempotency_key", row.Key, "seq_moved", seqMoved)
	mw.RecordInterruptedChargeResolution(interruptedNotFound)
	return true, nil
}

// settleFoundSucceeded records a payment Stripe took. It calls
// SettleSucceeded directly, not r.settle: r.settle skips an invoice already
// paid, and a second payment on a paid invoice is a duplicate the operator
// must see (payment.duplicate_charge), which SettleSucceeded raises.
func (r *Reconciler) settleFoundSucceeded(ctx context.Context, row domain.InterruptedCharge, pi PaymentIntentResult) (bool, error) {
	if r.settler == nil {
		return false, errors.New("settler not wired: the interrupted-charge check records results through it")
	}
	inv, err := r.invoices.Get(ctx, row.TenantID, row.InvoiceID)
	if err != nil {
		return false, err
	}
	r.recordFoundAttempt(ctx, row, inv, pi, domain.ChargeAttemptPending)
	if _, err := r.settle(withFoundAnchor(ctx, pi), inv, pi.ID, pi.AmountReceivedCents, pi.Purpose, "", terminalSucceeded, pi.Status, "", ""); err != nil {
		return false, fmt.Errorf("settle the payment Stripe took (%s): %w", pi.ID, err)
	}
	slog.InfoContext(ctx, "interrupted charge found at Stripe, succeeded; payment recorded",
		"invoice_id", row.InvoiceID, "stripe_payment_intent_id", pi.ID, "idempotency_key", row.Key)
	mw.RecordInterruptedChargeResolution(interruptedSucceeded)
	return r.endRecord(ctx, row)
}

// settleFoundDeclined records a decline through SettleFailed, as the webhook
// would, with the charge's seq as the CAS (it moves the invoice only if no
// other outcome was saved since) and the decline code and card (#966), so a
// hard decline found here stops dunning's retries on that card too.
func (r *Reconciler) settleFoundDeclined(ctx context.Context, row domain.InterruptedCharge, pi PaymentIntentResult) (bool, error) {
	if r.settler == nil {
		return false, errors.New("settler not wired: the interrupted-charge check records results through it")
	}
	inv, err := r.invoices.Get(ctx, row.TenantID, row.InvoiceID)
	if err != nil {
		return false, err
	}
	r.recordFoundAttempt(ctx, row, inv, pi, domain.ChargeAttemptFailed)
	msg := pi.FailureMessage
	if msg == "" {
		msg = "the payment was " + pi.Status
	}
	seq := row.Seq
	if err := r.settler.SettleFailed(withFoundAnchor(ctx, pi), row.TenantID, inv, PaymentFailure{
		PaymentIntentID: pi.ID, Message: msg, Purpose: pi.Purpose, ChargedAtSeq: &seq,
		DeclineCode: pi.DeclineCode, PaymentMethodID: pi.PaymentMethodID,
	}, SourceReconciler); err != nil {
		return false, fmt.Errorf("record the decline Stripe gave (%s): %w", pi.ID, err)
	}
	slog.InfoContext(ctx, "interrupted charge found at Stripe, declined; decline recorded",
		"invoice_id", row.InvoiceID, "stripe_payment_intent_id", pi.ID, "stripe_status", pi.Status)
	mw.RecordInterruptedChargeResolution(interruptedDeclined)
	return r.endRecord(ctx, row)
}

// stampFoundRunning saves a PaymentIntent that has not ended: processing, at
// the record's seq, so the processing sweep and the webhook take over. If the
// invoice moved on meanwhile, a requires_* PaymentIntent is canceled at
// Stripe (off-session it never ends on its own, so the record would never
// clear, and canceling stops a capture nothing would record), and a
// processing one is waited for: it always ends.
func (r *Reconciler) stampFoundRunning(ctx context.Context, row domain.InterruptedCharge, pi PaymentIntentResult) (bool, error) {
	inv, err := r.invoices.Get(ctx, row.TenantID, row.InvoiceID)
	if err != nil {
		return false, err
	}
	r.recordFoundAttempt(ctx, row, inv, pi, domain.ChargeAttemptPending)
	current, applied, err := r.invoices.StampChargeOutcome(ctx, row.TenantID, row.InvoiceID, row.Seq, domain.PaymentProcessing, pi.ID, "")
	if err != nil {
		return false, fmt.Errorf("stamp the running PaymentIntent %s: %w", pi.ID, err)
	}
	if applied || current.StripePaymentIntentID == pi.ID {
		slog.InfoContext(ctx, "interrupted charge found at Stripe, still running; recorded as processing",
			"invoice_id", row.InvoiceID, "stripe_payment_intent_id", pi.ID, "stripe_status", pi.Status)
		mw.RecordInterruptedChargeResolution(interruptedProcessing)
		return r.endRecord(ctx, row)
	}
	if pi.Status == "processing" {
		r.recordSync(ctx, current, pi.Status)
		slog.InfoContext(ctx, "interrupted charge found at Stripe still processing, and the invoice moved on; waiting for it to end",
			"invoice_id", row.InvoiceID, "stripe_payment_intent_id", pi.ID)
		return false, nil
	}
	if err := r.client.CancelPaymentIntent(ctx, pi.ID); err != nil {
		return false, fmt.Errorf("cancel the orphaned PaymentIntent %s: %w", pi.ID, err)
	}
	slog.WarnContext(ctx, "interrupted charge found at Stripe waiting, and its invoice moved on; the PaymentIntent is canceled",
		"invoice_id", row.InvoiceID, "stripe_payment_intent_id", pi.ID, "stripe_status", pi.Status,
		"recorded_payment_status", current.PaymentStatus, "recorded_payment_intent_id", current.StripePaymentIntentID)
	mw.RecordInterruptedChargeResolution(interruptedCanceled)
	return r.endRecord(ctx, row)
}

// recordFoundAttempt writes the ADR-102 attempt row for the PaymentIntent the
// check found, with the trigger the charge would have written (from the
// PaymentIntent's velox_purpose) and its anchor: a crash or a timeout left
// none, and a settle that finds no row inserts an 'external' one, which reads
// as an attempt Velox did not make. Best-effort, like every attempt row.
func (r *Reconciler) recordFoundAttempt(ctx context.Context, row domain.InterruptedCharge, inv domain.Invoice, pi PaymentIntentResult, outcome domain.ChargeAttemptOutcome) {
	trigger := domain.ChargeTriggerAutoCharge
	if pi.Purpose == piPurposeDunningRetry {
		trigger = domain.ChargeTriggerDunningRetry
	}
	var simAt *time.Time
	if !pi.AnchorAt.IsZero() {
		at := pi.AnchorAt
		simAt = &at
	}
	if err := r.invoices.RecordChargeAttempt(ctx, row.TenantID, domain.InvoiceChargeAttempt{
		InvoiceID:             row.InvoiceID,
		StripePaymentIntentID: pi.ID,
		Trigger:               trigger,
		Outcome:               outcome,
		DeclineCode:           pi.DeclineCode,
		StripePaymentMethodID: pi.PaymentMethodID,
		AmountCents:           inv.AmountDueCents,
		OccurredAt:            time.Now().UTC(),
		SimEffectiveAt:        simAt,
	}); err != nil {
		slog.WarnContext(ctx, "record charge attempt failed — timeline falls back to the webhook row",
			"invoice_id", row.InvoiceID, "payment_intent_id", pi.ID, "error", err)
	}
}

// withFoundAnchor carries the charge's contracted instant (velox_anchor_at)
// into the settle, as the webhook and the other sweeps do.
func withFoundAnchor(ctx context.Context, pi PaymentIntentResult) context.Context {
	if pi.AnchorAt.IsZero() {
		return ctx
	}
	return withSettleAnchor(ctx, pi.AnchorAt)
}

// endRecord clears the record once the check has saved what Stripe said. An
// error keeps it; the next run finds the result already saved and clears it.
func (r *Reconciler) endRecord(ctx context.Context, row domain.InterruptedCharge) (bool, error) {
	if _, err := r.invoices.EndChargeAttempt(ctx, row.TenantID, row.InvoiceID, row.Seq); err != nil {
		return false, fmt.Errorf("clear the charge record: %w", err)
	}
	return true, nil
}
