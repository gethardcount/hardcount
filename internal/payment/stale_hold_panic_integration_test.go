package payment

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestStaleHold_PanickingCheck_RestOfTheRunGoesOn: a stale-hold check that
// panics on one row (the real trigger: a nil dereference on one unusual row;
// #1299 had to add a nil-client guard "instead of panicking the tick") used to
// end Reconciler.Run, and with it the whole billing tick. The list puts
// never-checked open invoices first, so that row came back first on every
// tick and pinned every stale hold behind it; the sweep is the only way out of
// a stale hold, so those invoices could not be charged at all. The unknown,
// processing, parked and hosted sweeps after it never ran either.
//
// Now the panicking check counts as a failed check and its row is stamped (so
// it rotates behind the others), the other holds are checked, the later
// sweeps run, and the first panic is raised again once Run is done, so the
// tick still ends as panicked and pages.
//
//   - H1: open, never checked, the oldest hold, so first in the list; its
//     check panics.
//   - H2: open; Stripe has nothing for it.
//   - H3: 'unknown' with a stale hold; its check panics. The store does not
//     stamp an 'unknown' invoice's check (the unknown sweep's rotation key),
//     so a re-raise at the end of the stale-hold sweep would stop the later
//     sweeps on every tick.
//   - U: 'unknown' with a PaymentIntent that succeeded; the unknown sweep
//     settles it.
func TestStaleHold_PanickingCheck_RestOfTheRunGoesOn(t *testing.T) {
	r := newSHRig(t, "SH Panicking Check")
	h1 := r.staleHolds("INV-SHP-H1", 1, 13*time.Minute)[0]
	h2 := r.staleHolds("INV-SHP-H2", 1)[0]
	h3 := r.staleHolds("INV-SHP-H3", 1)[0]
	r.exec(`UPDATE invoices SET payment_status = 'unknown', updated_at = now() - interval '2 hours' WHERE id = $1`, h3)
	for _, id := range []string{h1, h3} {
		r.fake.setCustomer(stripeCustomerOf(r.get(id)), "panic")
	}
	h2Seq := r.get(h2).ChargeAttemptSeq

	u := r.seed("INV-SHP-U")
	paid := r.fake.addPI(stripeCustomerOf(u), u.ID, "", "succeeded", shDue)
	r.exec(`UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = $2, updated_at = now() - interval '2 hours' WHERE id = $1`, u.ID, paid.id)

	recovered := func() (p any) {
		defer func() { p = recover() }()
		_ = r.run()
		return nil
	}()

	if recovered != "stale charge check: nil dereference on "+stripeCustomerOf(r.get(h1)) {
		t.Fatalf("recovered %v, want the first check's panic (H1) raised again after the run", recovered)
	}
	if got := r.get(h2); r.hold(h2) != nil || got.ChargeAttemptSeq != h2Seq+1 {
		t.Errorf("H2: hold %v, charge_attempt_seq %d, want the hold cleared and the counter moved to %d", r.hold(h2), got.ChargeAttemptSeq, h2Seq+1)
	}
	if got := r.get(h1); got.ProviderSyncedAt == nil {
		t.Error("H1: the panicking check was not stamped, so it stays first in the list on every tick")
	}
	if got := r.get(u.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != paid.id {
		t.Errorf("U: %s/%s with PI %q, want paid with %q by the unknown sweep after the panicking checks",
			got.Status, got.PaymentStatus, got.StripePaymentIntentID, paid.id)
	}
}
