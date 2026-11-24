package payment

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/scheduler"
)

// StaleChargeHoldStore is what the stale-hold sweep needs from the invoice
// store (ADR-127, ADR-137). Part of ReconcileInvoiceStore, so the sweep cannot
// be left unwired: without it a stale hold, which every claim and the
// irreversible actions refuse, would never clear.
type StaleChargeHoldStore interface {
	ListStaleChargeHolds(ctx context.Context, limit int, skip []string) ([]domain.StaleChargeHold, error)
	ClearStaleChargeHold(ctx context.Context, tenantID, id string, hold time.Time, attemptSeq int64, moveSeq bool) (bool, error)
	MarkStaleHoldChecked(ctx context.Context, tenantID, id string) error
}

// Stale-hold results, the label of
// hardcount_stale_charge_hold_resolutions_total.
const (
	staleHoldReleased = "released"  // the held charge sent no confirm; hold cleared and counter moved, so the invoice is charged again
	staleHoldCleared  = "cleared"   // a leftover hold on an invoice that is in flight, settled, voided or written off; cleared
	staleHoldFailed   = "failed"    // the clear could not be written (or the check panicked); hold kept for the next tick
	staleHoldLostRace = "lost_race" // another writer changed the invoice first; the next tick looks again
)

// sweepStaleChargeHolds clears charge holds whose charger is gone (a crash, a
// shutdown, a cancelled caller). Every claim, the hosted page, void, offline
// payment and dunning's final action refuse while a hold is set.
//
// It asks Stripe nothing. A charge puts its PaymentIntent on the invoice as an
// unknown outcome before it confirms (sendCharge, StampPendingConfirm), in a
// compare-and-set on the invoice row. So a stale hold on an invoice still open
// for a charge (pending or failed) is a charge that never confirmed: no money
// moved, and the hold is cleared with the counter moved, so the next charge
// sends a new idempotency key. On any other invoice the hold is a leftover:
// the invoice is in flight on its PaymentIntent, which the unknown and
// processing sweeps read by id, or it is paid, voided or written off. The
// clear is a compare-and-set on the same row, so a charger that stalled past
// its hold either stamped first (the clear then moves no counter) or finds
// the hold gone and confirms nothing.
//
// It runs first in Run, so an invoice it releases is charged by the same
// tick's auto-charge sweep. It drains page after page of limit holds, open
// invoices first, each hold checked once, until none is left: a hold costs one
// database write. A check that panics counts as failed: its
// row is stamped so it rotates behind the others, and panics keeps the panic
// for Run to raise again after its last sweep.
func (r *Reconciler) sweepStaleChargeHolds(ctx context.Context, limit int, panics *scheduler.Panics) (int, []error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		checked  []string
		errs     []error
		resolved int
	)
	for {
		holds, err := r.invoices.ListStaleChargeHolds(ctx, limit, checked)
		if err != nil {
			return resolved, append(errs, fmt.Errorf("list stale charge holds: %w", err))
		}
		for _, h := range holds {
			if ctx.Err() != nil {
				// Shutdown or a lost lease: the rest wait for the next tick.
				return resolved, errs
			}
			checked = append(checked, h.Invoice.ID)
			var result string
			var cerr error
			if guardRow(panics, func() { result, cerr = r.checkStaleHold(ctx, h) },
				"step", "stale_charge_check", "invoice_id", h.Invoice.ID, "tenant_id", h.Invoice.TenantID) {
				r.markChecked(ctx, h.Invoice)
				result, cerr = staleHoldFailed, nil
			}
			recordStaleChargeHoldResolution(result)
			if cerr != nil {
				errs = append(errs, fmt.Errorf("invoice %s: %w", h.Invoice.ID, cerr))
			}
			if result == staleHoldReleased || result == staleHoldCleared {
				resolved++
			}
		}
		if len(holds) < limit {
			return resolved, errs
		}
	}
}

// checkStaleHold clears one stale hold. On an invoice still open for a charge
// its charger never confirmed, so the counter moves and the next charge uses
// a fresh idempotency key (ADR-105 amendment): the old one could be handed a
// 500 Stripe saved under the create. On any other invoice it only clears. It
// returns the result label, and an error only for a failure worth surfacing
// on the reconciler's error log.
func (r *Reconciler) checkStaleHold(ctx context.Context, h domain.StaleChargeHold) (string, error) {
	inv := h.Invoice
	ctx = auth.WithTenantID(ctx, inv.TenantID)
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
		slog.InfoContext(ctx, "stale charge check: the interrupted charge never confirmed a payment; the invoice can be charged again",
			"invoice_id", inv.ID, "charge_attempt_seq", inv.ChargeAttemptSeq+1)
		return staleHoldReleased, nil
	}
	return staleHoldCleared, nil
}

// markChecked stamps a row whose hold could not be cleared, so it rotates.
// Best-effort: a lost stamp only means the row is looked at again sooner. The
// store leaves an 'unknown' invoice unstamped: the unknown sweep rotates on
// the same column and stamps it on its own checks.
func (r *Reconciler) markChecked(ctx context.Context, inv domain.Invoice) {
	if err := r.invoices.MarkStaleHoldChecked(ctx, inv.TenantID, inv.ID); err != nil {
		slog.WarnContext(ctx, "stale charge check: could not stamp the check", "invoice_id", inv.ID, "error", err)
	}
}

// openForCharge is the state a charge claim admits: open for payment
// (finalized, or an unpaid top-up) and pending or failed. Only there does a
// cleared hold move the counter.
func openForCharge(inv domain.Invoice) bool {
	return inv.OpenForPayment() &&
		(inv.PaymentStatus == domain.PaymentPending || inv.PaymentStatus == domain.PaymentFailed)
}
