package payment

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// panicOnClear is the invoice store with a stale-hold clear that panics for
// one invoice: a nil dereference on one unusual row.
type panicOnClear struct {
	ReconcileInvoiceStore
	id string
}

func (p panicOnClear) ClearStaleChargeHold(ctx context.Context, tenantID, id string, hold time.Time, attemptSeq int64, moveSeq bool) (bool, error) {
	if id == p.id {
		panic("stale charge check: nil dereference on " + id)
	}
	return p.ReconcileInvoiceStore.ClearStaleChargeHold(ctx, tenantID, id, hold, attemptSeq, moveSeq)
}

// TestStaleHold_PanickingCheck_RestOfTheRunGoesOn: a stale-hold check that
// panics on one row (#1299 had to add a nil-client guard "instead of
// panicking the tick") used to end Reconciler.Run, and with it the whole
// billing tick. The list puts never-checked open invoices first, so that row
// came back first on every tick and pinned every stale hold behind it; the
// sweep is the only way out of a stale hold, so those invoices could not be
// charged at all. The unknown, processing and hosted sweeps after it never ran
// either.
//
// Now the panicking check counts as a failed check and its row is stamped (so
// it rotates behind the others), the other holds are cleared, the later
// sweeps run, and the first panic is raised again once Run is done, so the
// tick still ends as panicked and pages.
//
//   - H1: open, never checked, the oldest hold, so first in the list; its
//     check panics.
//   - H2: open; its charge never confirmed, so it is released.
//   - U: 'unknown' with a PaymentIntent that succeeded; the unknown sweep
//     settles it.
func TestStaleHold_PanickingCheck_RestOfTheRunGoesOn(t *testing.T) {
	r := newSHRig(t, "SH Panicking Check")
	h1 := r.staleHolds("INV-SHP-H1", 1, 13*time.Minute)[0]
	h2 := r.staleHolds("INV-SHP-H2", 1)[0]
	h2Seq := r.get(h2).ChargeAttemptSeq

	u := r.seed("INV-SHP-U")
	paid := r.fake.addPI(stripeCustomerOf(u), u.ID, "", "succeeded", shDue)
	r.exec(`UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = $2, updated_at = now() - interval '2 hours' WHERE id = $1`, u.ID, paid.id)

	rec := NewReconciler(r.fake, panicOnClear{ReconcileInvoiceStore: r.invoices, id: h1}, time.Minute)
	rec.SetSettler(r.stripe)
	recovered := func() (p any) {
		defer func() { p = recover() }()
		_, _ = rec.Run(r.ctx, 50)
		return nil
	}()

	if recovered != "stale charge check: nil dereference on "+h1 {
		t.Fatalf("recovered %v, want the first check's panic (H1) raised again after the run", recovered)
	}
	if got := r.get(h2); r.hold(h2) != nil || got.ChargeAttemptSeq != h2Seq+1 {
		t.Errorf("H2: hold %v, charge_attempt_seq %d, want the hold cleared and the counter moved to %d", r.hold(h2), got.ChargeAttemptSeq, h2Seq+1)
	}
	if got := r.get(h1); got.ProviderSyncedAt == nil || r.hold(h1) == nil {
		t.Errorf("H1: stamped %v, hold %v; want the panicking check stamped (so it rotates) and its hold kept", got.ProviderSyncedAt != nil, r.hold(h1))
	}
	if got := r.get(u.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != paid.id {
		t.Errorf("U: %s/%s with PI %q, want paid with %q by the unknown sweep after the panicking check",
			got.Status, got.PaymentStatus, got.StripePaymentIntentID, paid.id)
	}
}
