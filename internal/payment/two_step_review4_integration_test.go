package payment

import (
	"os"
	"strings"
	"testing"
	"time"
)

// On the real database (the stale-hold rig): the upgrade check run as the
// migration runs it.

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
