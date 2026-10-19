package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// StaleChargeHoldStore is what the stale-hold sweep needs from the invoice
// store (ADR-127). Part of ReconcileInvoiceStore, so the sweep cannot be left
// unwired: without it a stale hold, which every claim and the irreversible
// actions refuse, would never clear.
type StaleChargeHoldStore interface {
	ListStaleChargeHolds(ctx context.Context, limit int) ([]domain.StaleChargeHold, error)
	ClearStaleChargeHold(ctx context.Context, tenantID, id string, hold time.Time, attemptSeq int64, moveSeq bool) (bool, error)
	MarkStaleHoldChecked(ctx context.Context, tenantID, id string) error
	StampChargeOutcome(ctx context.Context, tenantID, id string, attemptSeq int64, paymentStatus domain.InvoicePaymentStatus, stripePaymentIntentID, lastPaymentError string) (domain.Invoice, bool, error)
}

// staleHoldListWindow is how far before its hold the sweep lists a
// customer's PaymentIntents. The hold is written when the charge is claimed,
// on the database's wall clock, and the PaymentIntent is created after that,
// so an hour covers clock skew between Postgres and Stripe with a wide margin
// and keeps the list to one page in practice.
const staleHoldListWindow = time.Hour

// Stale-hold check results, the label of
// velox_stale_charge_hold_resolutions_total.
const (
	staleHoldSettled   = "settled"    // a succeeded payment was found and recorded
	staleHoldAdopted   = "adopted"    // a processing payment was found and put on the invoice
	staleHoldCanceled  = "canceled"   // an unfinished off-session payment was canceled at Stripe; hold kept
	staleHoldReleased  = "released"   // Stripe had nothing; hold cleared and the counter moved
	staleHoldCleared   = "cleared"    // nothing to record on an invoice that is settled or in flight; hold cleared
	staleHoldFailed    = "failed"     // Stripe could not be read this time; hold kept
	staleHoldCannotAsk = "cannot_ask" // Stripe will not answer (not connected, key rejected, customer missing); hold kept
	staleHoldLostRace  = "lost_race"  // another writer changed the invoice first; the next tick looks again
)

// sweepStaleChargeHolds is the one place a stale charge hold is resolved
// (ADR-127). A stale hold means a charge may have been sent and its result
// was not saved — a crash, a caller's deadline, a lost answer. Every claim,
// the hosted page, void, offline payment and dunning's final action refuse
// while it is set; this sweep asks Stripe what the lost charge did and then
// records it, or clears the hold.
//
// It runs first in Run, so an invoice it clears is charged by the same tick's
// auto-charge sweep. It calls the Stripe client directly, not through the
// charge breaker: failed checks must never open the breaker that guards
// customer charges.
//
// Why "Stripe has nothing" is final: List is the read-after-write API; no
// request for the attempt can start after its hold ends (the charge's
// re-check and send budget); the sweep waits StaleChargeHoldWait more; every
// Velox PaymentIntent carries velox_invoice_id and the invoice's Stripe
// customer; and that customer id is set once (customer store guard).
func (r *Reconciler) sweepStaleChargeHolds(ctx context.Context, limit int) (int, []error) {
	if r.settler == nil {
		// Unit-test construction only (production wires SetSettler): without
		// the settler a found payment could not be recorded, so do nothing
		// rather than clear a hold that may speak for one.
		return 0, nil
	}
	holds, err := r.invoices.ListStaleChargeHolds(ctx, limit)
	if err != nil {
		return 0, []error{fmt.Errorf("list stale charge holds: %w", err)}
	}
	resolved := 0
	var errs []error
	for _, h := range holds {
		result, err := r.checkStaleHold(ctx, h)
		mw.RecordStaleChargeHoldResolution(result)
		if err != nil {
			errs = append(errs, fmt.Errorf("invoice %s: %w", h.Invoice.ID, err))
		}
		switch result {
		case staleHoldSettled, staleHoldAdopted, staleHoldReleased, staleHoldCleared:
			resolved++
		}
	}
	return resolved, errs
}

// checkStaleHold asks Stripe about one stale hold and acts on the answer. It
// returns the result label and an error only for a failure worth surfacing
// on the reconciler's error log; an expected "try again" returns nil.
func (r *Reconciler) checkStaleHold(ctx context.Context, h domain.StaleChargeHold) (string, error) {
	inv := h.Invoice
	ctx = auth.WithTenantID(ctx, inv.TenantID)

	// No Stripe customer: no charge can have been sent (a charge needs one,
	// and the id is set once), so this is "Stripe has nothing" without a call.
	if h.StripeCustomerID == "" {
		return r.clearStaleHold(ctx, h, inv)
	}

	pis, err := r.client.ListPaymentIntentsForCustomer(ctx, h.StripeCustomerID, h.Hold.Add(-staleHoldListWindow))
	if err != nil {
		return r.staleHoldReadFailed(ctx, h, err)
	}

	var mine []PaymentIntentResult
	for _, pi := range pis {
		if pi.InvoiceID == inv.ID {
			mine = append(mine, pi)
		}
	}
	// Oldest first: the first succeeded payment pays the invoice, and each
	// later one is the duplicate.
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].CreatedAt < mine[j].CreatedAt })

	var succeeded, processing, unfinished []PaymentIntentResult
	for _, pi := range mine {
		switch pi.Status {
		case domain.ProviderPISucceeded:
			if pi.FullyRefunded {
				// Money taken and handed back in full: it does not pay the
				// invoice. Loud, because Velox did not refund it.
				slog.ErrorContext(ctx, "stale charge check: a payment on this invoice was found already fully refunded at Stripe; it is not counted as paying the invoice",
					"invoice_id", inv.ID, "tenant_id", inv.TenantID, "stripe_payment_intent_id", pi.ID)
				continue
			}
			succeeded = append(succeeded, pi)
		case domain.ProviderPIProcessing:
			processing = append(processing, pi)
		case domain.ProviderPIRequiresAction, domain.ProviderPIRequiresConfirmation, domain.ProviderPIRequiresCapture:
			// The hosted page's own PaymentIntent is the customer's to finish
			// (Checkout's cannot be canceled, only its session expired), and
			// the hosted block already keeps charges off while a session may
			// be paid. A PaymentIntent the invoice already records as in
			// flight is the processing sweep's (and follow-up C's), not a lost
			// result.
			if pi.Purpose == PurposeHostedInvoicePay {
				continue
			}
			if inv.PaymentInFlight() && inv.StripePaymentIntentID == pi.ID {
				continue
			}
			unfinished = append(unfinished, pi)
		}
	}

	switch {
	case len(succeeded) > 0:
		return r.settleStaleHold(ctx, h, succeeded)
	case len(processing) > 0:
		return r.adoptStaleHold(ctx, h, processing[len(processing)-1])
	case len(unfinished) > 0:
		return r.cancelUnfinished(ctx, h, unfinished)
	default:
		return r.clearStaleHold(ctx, h, inv)
	}
}

// settleStaleHold records every succeeded payment, oldest first, through the
// settlement primitive: the first settles the invoice (pending, failed,
// unknown or written off), each later one raises payment.duplicate_charge,
// and money on a voided invoice raises payment.received_on_voided_invoice.
// Then the hold is cleared. The counter is not moved: the settle's own stamp
// moved it.
func (r *Reconciler) settleStaleHold(ctx context.Context, h domain.StaleChargeHold, succeeded []PaymentIntentResult) (string, error) {
	inv := h.Invoice
	recorded := false
	for _, pi := range succeeded {
		fresh, err := r.invoices.Get(ctx, inv.TenantID, inv.ID)
		if err != nil {
			r.markChecked(ctx, inv)
			return staleHoldFailed, fmt.Errorf("re-read before settling %s: %w", pi.ID, err)
		}
		if fresh.Status == domain.InvoicePaid && fresh.StripePaymentIntentID == pi.ID {
			// Already the invoice's payment: its webhook or the processing
			// sweep recorded it, and only the hold was left behind.
			continue
		}
		recorded = true
		settleCtx := ctx
		if !pi.AnchorAt.IsZero() {
			settleCtx = withSettleAnchor(ctx, pi.AnchorAt)
		}
		if err := r.settler.SettleSucceeded(settleCtx, inv.TenantID, fresh, pi.ID, pi.AmountReceivedCents, SourceReconciler); err != nil {
			r.markChecked(ctx, inv)
			return staleHoldFailed, fmt.Errorf("settle found payment %s: %w", pi.ID, err)
		}
		slog.InfoContext(ctx, "stale charge check: a payment whose result was lost was found at Stripe and recorded",
			"invoice_id", inv.ID, "stripe_payment_intent_id", pi.ID)
	}
	fresh, err := r.invoices.Get(ctx, inv.TenantID, inv.ID)
	if err != nil {
		r.markChecked(ctx, inv)
		return staleHoldFailed, fmt.Errorf("re-read after settling: %w", err)
	}
	cleared, err := r.invoices.ClearStaleChargeHold(ctx, inv.TenantID, inv.ID, h.Hold, fresh.ChargeAttemptSeq, false)
	if err != nil {
		r.markChecked(ctx, inv)
		return staleHoldFailed, fmt.Errorf("clear hold after settling: %w", err)
	}
	if !cleared {
		return staleHoldLostRace, nil
	}
	if !recorded {
		return staleHoldCleared, nil
	}
	return staleHoldSettled, nil
}

// adoptStaleHold puts a found processing payment on an open invoice at the
// counter the sweep read, so the invoice is processing: the webhook, the
// processing sweep and every PaymentInFlight gate take over. On an invoice
// that is already settled or in flight it only clears the hold.
func (r *Reconciler) adoptStaleHold(ctx context.Context, h domain.StaleChargeHold, pi PaymentIntentResult) (string, error) {
	inv := h.Invoice
	if !openForCharge(inv) {
		return r.clearStaleHold(ctx, h, inv)
	}
	stamped, applied, err := r.invoices.StampChargeOutcome(ctx, inv.TenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentProcessing, pi.ID, "")
	if err != nil {
		r.markChecked(ctx, inv)
		return staleHoldFailed, fmt.Errorf("adopt processing payment %s: %w", pi.ID, err)
	}
	if !applied {
		return staleHoldLostRace, nil
	}
	cleared, err := r.invoices.ClearStaleChargeHold(ctx, inv.TenantID, inv.ID, h.Hold, stamped.ChargeAttemptSeq, false)
	if err != nil {
		r.markChecked(ctx, inv)
		return staleHoldFailed, fmt.Errorf("clear hold after adopting %s: %w", pi.ID, err)
	}
	slog.InfoContext(ctx, "stale charge check: a processing payment whose result was lost was found at Stripe and put on the invoice",
		"invoice_id", inv.ID, "stripe_payment_intent_id", pi.ID)
	if !cleared {
		return staleHoldLostRace, nil
	}
	return staleHoldAdopted, nil
}

// cancelUnfinished cancels Velox's own off-session PaymentIntents that
// stopped at requires_action / requires_confirmation / requires_capture: Velox
// never shows the customer a way to finish them, so they would never end. The
// hold is kept; the next check sees them canceled and decides then.
func (r *Reconciler) cancelUnfinished(ctx context.Context, h domain.StaleChargeHold, unfinished []PaymentIntentResult) (string, error) {
	inv := h.Invoice
	r.markChecked(ctx, inv)
	for _, pi := range unfinished {
		if err := r.client.CancelPaymentIntent(ctx, pi.ID); err != nil {
			// It may have moved on (succeeded) since the list: a failed check,
			// asked again later.
			slog.WarnContext(ctx, "stale charge check: could not cancel an unfinished payment; checking again later",
				"invoice_id", inv.ID, "stripe_payment_intent_id", pi.ID, "stripe_status", pi.Status, "error", err)
			return staleHoldFailed, nil
		}
		slog.InfoContext(ctx, "stale charge check: canceled an unfinished off-session payment whose result was lost",
			"invoice_id", inv.ID, "stripe_payment_intent_id", pi.ID, "stripe_status", pi.Status)
	}
	return staleHoldCanceled, nil
}

// clearStaleHold is "Stripe has nothing for this charge" (or nothing left to
// record). On an invoice still open for a charge it clears the hold and moves
// the counter, so the next charge uses a fresh idempotency key (ADR-105
// amendment). On any other status it only clears: nothing can charge it.
func (r *Reconciler) clearStaleHold(ctx context.Context, h domain.StaleChargeHold, inv domain.Invoice) (string, error) {
	moveSeq := openForCharge(inv)
	cleared, err := r.invoices.ClearStaleChargeHold(ctx, inv.TenantID, inv.ID, h.Hold, inv.ChargeAttemptSeq, moveSeq)
	if err != nil {
		r.markChecked(ctx, inv)
		return staleHoldFailed, fmt.Errorf("clear stale hold: %w", err)
	}
	if !cleared {
		return staleHoldLostRace, nil
	}
	if moveSeq {
		slog.InfoContext(ctx, "stale charge check: Stripe has no payment for this invoice's interrupted charge; the invoice can be charged again",
			"invoice_id", inv.ID, "charge_attempt_seq", inv.ChargeAttemptSeq+1)
		return staleHoldReleased, nil
	}
	return staleHoldCleared, nil
}

// staleHoldReadFailed handles a list that did not answer. A read that may
// succeed later keeps the hold and rotates the row. One that will not
// (Stripe disconnected, the key rejected, the customer gone from the
// account) keeps the hold on an open invoice — charges stay blocked and
// write-off is the exit — and clears it on a settled, voided or written-off
// invoice, which nothing can charge.
func (r *Reconciler) staleHoldReadFailed(ctx context.Context, h domain.StaleChargeHold, err error) (string, error) {
	inv := h.Invoice
	permanent := errors.Is(err, ErrStripeNotConfigured) || errors.Is(err, ErrStripeAuthFailed) || errors.Is(err, ErrStripeCustomerMissing)
	if !permanent {
		r.markChecked(ctx, inv)
		slog.WarnContext(ctx, "stale charge check: could not read the customer's payments at Stripe; the charge hold stays and is checked again later",
			"invoice_id", inv.ID, "tenant_id", inv.TenantID, "error", err)
		return staleHoldFailed, nil
	}
	if inv.Status == domain.InvoiceFinalized {
		r.markChecked(ctx, inv)
		slog.ErrorContext(ctx, "stale charge check: can't confirm an earlier charge with Stripe; write the invoice off or fix the Stripe connection. Charges on this invoice stay blocked until then",
			"invoice_id", inv.ID, "tenant_id", inv.TenantID, "invoice_number", inv.InvoiceNumber,
			"stripe_customer_id", h.StripeCustomerID, "error", err)
		return staleHoldCannotAsk, nil
	}
	slog.ErrorContext(ctx, "stale charge check: can't confirm an earlier charge with Stripe on an invoice that is no longer open; clearing its charge hold. Check the customer's payments in Stripe",
		"invoice_id", inv.ID, "tenant_id", inv.TenantID, "invoice_number", inv.InvoiceNumber, "status", inv.Status,
		"stripe_customer_id", h.StripeCustomerID, "error", err)
	cleared, cerr := r.invoices.ClearStaleChargeHold(ctx, inv.TenantID, inv.ID, h.Hold, inv.ChargeAttemptSeq, false)
	if cerr != nil {
		r.markChecked(ctx, inv)
		return staleHoldFailed, fmt.Errorf("clear stale hold on a closed invoice: %w", cerr)
	}
	if !cleared {
		return staleHoldLostRace, nil
	}
	return staleHoldCleared, nil
}

// markChecked stamps a kept row so it rotates. Best-effort: a lost stamp only
// means the row is asked about again sooner.
func (r *Reconciler) markChecked(ctx context.Context, inv domain.Invoice) {
	if err := r.invoices.MarkStaleHoldChecked(ctx, inv.TenantID, inv.ID); err != nil {
		slog.WarnContext(ctx, "stale charge check: could not stamp the check", "invoice_id", inv.ID, "error", err)
	}
}

// openForCharge is the state a charge claim admits: finalized and pending or
// failed. Only there does a lost charge's "nothing at Stripe" move the counter
// and a found processing payment get adopted.
func openForCharge(inv domain.Invoice) bool {
	return inv.Status == domain.InvoiceFinalized &&
		(inv.PaymentStatus == domain.PaymentPending || inv.PaymentStatus == domain.PaymentFailed)
}
