package payment

import (
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestReconcile_PanickingRow_RotatesAndTheRestGoesOn: the unknown and
// processing sweeps list the least recently checked rows first, and stamp a
// row only once its Stripe read is handled. A row whose check panicked (the
// real trigger: a nil dereference on one unusual row, as in flexprice #1615)
// was never stamped, so it came back first on every tick, and the panic ended
// Reconciler.Run there: the rest of that sweep and every sweep after it
// stopped for every tenant until a hotfix. Every charge claim refuses an
// 'unknown' invoice, so this sweep (or a webhook) is its only way out.
//
// Now each row runs under its own guard: a row that panics is stamped, so it
// rotates behind the others, the other rows and the later sweeps run, and the
// first panic is raised again after the last sweep. The panic drops Run's
// errors, which only the caller logs, so Run logs them first.
//
//   - U1: 'unknown', the oldest, so first; its read panics.
//   - E:  'unknown'; Stripe has no such PaymentIntent, a read error.
//   - U2: 'unknown'; its PaymentIntent succeeded.
//   - P:  'processing'; its PaymentIntent succeeded and no webhook came.
//
// A parked invoice (no PaymentIntent id) is checked by the stale-hold sweep,
// whose panicking row is pinned by TestStaleHold_PanickingCheck_RestOfTheRunGoesOn.
func TestReconcile_PanickingRow_RotatesAndTheRestGoesOn(t *testing.T) {
	r := newSHRig(t, "Reconcile Panicking Row")
	logs := captureLogs(t)

	set := func(inv domain.Invoice, status, pi, age string) {
		r.exec(`UPDATE invoices SET payment_status = $2, stripe_payment_intent_id = NULLIF($3, ''), updated_at = now() - $4::interval WHERE id = $1`,
			inv.ID, status, pi, age)
	}
	succeededPI := func(inv domain.Invoice) string {
		return r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue).id
	}

	u1 := r.seed("INV-RP-U1")
	u1PI := succeededPI(u1)
	set(u1, "unknown", u1PI, "3 hours")
	r.fake.panicOnRead(u1PI)

	e := r.seed("INV-RP-E")
	set(e, "unknown", "pi_not_at_stripe", "170 minutes")

	u2 := r.seed("INV-RP-U2")
	u2PI := succeededPI(u2)
	set(u2, "unknown", u2PI, "2 hours")

	p := r.seed("INV-RP-P")
	pPI := succeededPI(p)
	set(p, "processing", pPI, "2 hours")

	recovered := func() (p any) {
		defer func() { p = recover() }()
		_ = r.run()
		return nil
	}()
	if recovered != "reconcile: nil dereference on "+u1PI {
		t.Fatalf("recovered %v, want U1's panic raised again after the last sweep", recovered)
	}

	for _, c := range []struct {
		name, id, pi string
	}{
		{"U2 (unknown, after the panicking row)", u2.ID, u2PI},
		{"P (processing, a later sweep)", p.ID, pPI},
	} {
		if got := r.get(c.id); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != c.pi {
			t.Errorf("%s: %s/%s with PI %q, want paid with %q", c.name, got.Status, got.PaymentStatus, got.StripePaymentIntentID, c.pi)
		}
	}
	if got := r.get(u1.ID); got.ProviderSyncedAt == nil {
		t.Error("U1: the row whose check panicked was not stamped, so it stays first in the list on every tick")
	}

	logged := false
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="reconciler error"`) && strings.Contains(line, e.ID) {
			logged = true
		}
	}
	if !logged {
		t.Error("E's read error was not logged: the panic drops Run's errors, so Run must log them before it raises the panic again")
	}
}
