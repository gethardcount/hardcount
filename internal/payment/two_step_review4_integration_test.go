package payment

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// From review 4 (2026-11-10), on the real database (the stale-hold rig): an
// unclear charge on a test clock ahead of real time, and the upgrade check run
// as the migration runs it.

// timePasses moves every wall-clock mark on the invoice back by d, which is
// what d of real time passing does to "now minus the mark": the hold and its
// token (written on the database's clock) and the invoice's last write. It
// shifts updated_at RELATIVELY; the author's ageUpdated sets it to now()-age,
// which hides where updated_at was written.
func (r *shRig) timePasses(invoiceID string, d time.Duration) {
	r.t.Helper()
	r.exec(`UPDATE invoices SET
			updated_at = updated_at - make_interval(secs => $2),
			charge_hold_token = charge_hold_token - make_interval(secs => $2),
			auto_charge_claimed_until = auto_charge_claimed_until - make_interval(secs => $2)
		WHERE id = $1`, invoiceID, d.Seconds())
}

// A confirm whose answer never arrived on a test-clock invoice whose clock is
// ahead of real time (the ordinary use of a test clock: advance a month).
// The charge runs with the clock bound on ctx (Collect binds it with
// bindForInvoice, catch-up with WithSim), so StampPendingConfirm writes
// updated_at = the simulated instant. The unknown sweep's cool-off compares
// updated_at with the real clock, so the invoice is not listed until real
// time reaches the simulated instant: it stays unknown, refusing every
// charge, void, offline payment and the hosted page, with write-off the only
// exit. The one-call design resolved a lost charge from its hold, which is
// written on the database's clock.
//
// Correct: once the hold has ended, the sweep reads the PaymentIntent, cancels
// it and frees the invoice, exactly as for the wall-clock invoice beside it.
func TestTwoStep_UnclearConfirm_OnAdvancedTestClock_IsResolved(t *testing.T) {
	r := newTwoStepRig(t, "R4 Sim Clock")
	wall := r.seed("INV-R4-WALL")
	sim := r.seed("INV-R4-SIM")

	r.fake.queueConfirm(fakeMode{kind: fakeConfirmNeverArrived}, fakeMode{kind: fakeConfirmNeverArrived})
	if _, err := r.chargeAs(r.ctx, autoCharge, wall.ID, "pm_card_1"); err == nil {
		t.Fatal("wall-clock charge: want the unclear error")
	}
	simCtx := clock.WithSim(r.ctx, clock.Sim{At: time.Now().UTC().Add(30 * 24 * time.Hour), TestClockID: "clk_review4"})
	if _, err := r.chargeAs(simCtx, autoCharge, sim.ID, "pm_card_1"); err == nil {
		t.Fatal("test-clock charge: want the unclear error")
	}
	for _, inv := range []domain.Invoice{wall, sim} {
		if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID == "" {
			t.Fatalf("setup %s: %s on %q, want unknown on its PaymentIntent", inv.InvoiceNumber, got.PaymentStatus, got.StripePaymentIntentID)
		}
	}

	// Sixteen minutes of real time: both holds ended eleven minutes ago.
	r.timePasses(wall.ID, 16*time.Minute)
	r.timePasses(sim.ID, 16*time.Minute)
	r.runClean()

	if got := r.get(wall.ID); got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("control (wall clock): %s, want pending after the sweep canceled the never-confirmed PaymentIntent", got.PaymentStatus)
	}
	got := r.get(sim.ID)
	if got.PaymentStatus != domain.PaymentPending {
		t.Errorf("test-clock invoice: still %s on %s (updated_at %s, %s ahead of real time); want pending. The unknown sweep never lists it, so nothing reads or cancels its PaymentIntent and no charge can ever start",
			got.PaymentStatus, got.StripePaymentIntentID, got.UpdatedAt.Format(time.RFC3339), time.Until(got.UpdatedAt).Round(time.Hour))
	}
	if st := r.fake.pi(got.StripePaymentIntentID).status; st != "canceled" {
		t.Errorf("the test-clock invoice's PaymentIntent is %s at Stripe, want canceled", st)
	}
}

// The same miss when the money DID move: the confirm charged the card and the
// answer was lost. With no webhook (a test clock in local development has no
// forwarder; in production the webhook may be the thing that was lost), the
// sweep is the only reader. Correct: the payment is recorded.
func TestTwoStep_LostAnswer_OnAdvancedTestClock_PaymentIsRecorded(t *testing.T) {
	r := newTwoStepRig(t, "R4 Sim Clock Paid")
	sim := r.seed("INV-R4-SIMPAID")
	r.fake.queueConfirm(fakeMode{kind: fakeSave500})
	simCtx := clock.WithSim(r.ctx, clock.Sim{At: time.Now().UTC().Add(30 * 24 * time.Hour), TestClockID: "clk_review4"})
	if _, err := r.chargeAs(simCtx, autoCharge, sim.ID, "pm_card_1"); err == nil {
		t.Fatal("want the unclear error")
	}
	if st := r.fake.onlyPI().status; st != "succeeded" {
		t.Fatalf("setup: the PaymentIntent is %s at Stripe, want succeeded (money moved)", st)
	}
	r.timePasses(sim.ID, 16*time.Minute)
	r.runClean()
	if got := r.get(sim.ID); got.Status != domain.InvoicePaid {
		t.Errorf("invoice %s/%s; want paid: Stripe took the money and nothing recorded it", got.Status, got.PaymentStatus)
	}
}

// Migration 00225's upgrade check, run the way the migration runs it: in its
// own transaction, with nothing set. invoices has FORCE ROW LEVEL SECURITY, so
// for a migration user that is the database owner but not a superuser (the
// documented minimum for DATABASE_URL, docs/ops/postgres-requirements.md) the
// count sees no rows unless the file turns the bypass on, as every migration
// that reads invoices does (SET LOCAL app.bypass_rls = 'on'; 00186 says why).
// This test guards that line: without it the check counts nothing and lets
// the upgrade through. The other upgrade-check test runs the block inside a
// TxBypass transaction, which sets the bypass for it and so cannot tell.
func TestUpgradeCheck_RunAsTheMigrationRuns_StopsOnAHold(t *testing.T) {
	r := newTwoStepRig(t, "R4 Upgrade RLS")
	inv := r.seed("INV-R4-UPGRADE")
	c, err := r.claim(autoCharge, inv.ID) // an open invoice with a charge hold
	if err != nil || !c.ok() {
		t.Fatalf("claim: %v ok=%v", err, c.ok())
	}
	t.Cleanup(func() { _ = r.release(c, inv.ID) })

	raw, err := os.ReadFile("../platform/migrate/sql/00225_charge_hold_token.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	src := string(raw)
	up := src[:strings.Index(src, "-- +goose Down")]
	i := strings.Index(up, "-- +goose StatementBegin")
	j := strings.Index(up, "-- +goose StatementEnd")
	if i < 0 || j < i {
		t.Fatal("the migration has no upgrade check block")
	}
	// Everything the Up section runs before the check, then the check: any
	// SET LOCAL the file carries is honoured.
	before, check := up[:i], up[i+len("-- +goose StatementBegin"):j]

	// The app pool's user is not a superuser and has no BYPASSRLS: the same
	// standing as a non-superuser owner under FORCE ROW LEVEL SECURITY.
	tx, err := r.db.Pool.BeginTx(r.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range strings.Split(stripSQLComments(before), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			if _, err := tx.ExecContext(r.ctx, s); err != nil {
				t.Fatalf("statement before the check %q: %v", s, err)
			}
		}
	}
	_, err = tx.ExecContext(r.ctx, check)
	if err == nil || !strings.Contains(err.Error(), "Hardcount upgrade stopped") {
		t.Errorf("upgrade check with an open invoice holding a charge hold: err = %v; want the upgrade stopped. Under row-level security the count sees no invoices, so the check passes over every hold and park", err)
	}
	_ = tx.Rollback()

	// Control: the hold is there, and the same block sees it once the
	// transaction turns the bypass on, as the other migrations do.
	ctl, err := r.db.Pool.BeginTx(r.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ctl.Rollback() }()
	if _, err := ctl.ExecContext(r.ctx, `SET LOCAL app.bypass_rls = 'on'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.ExecContext(r.ctx, check); err == nil || !strings.Contains(err.Error(), "Hardcount upgrade stopped") {
		t.Fatalf("control with the bypass on: err = %v, want the upgrade stopped (the test's setup is wrong)", err)
	}
}

func stripSQLComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
