package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedClockDueRuns makes three active dunning runs due on one test clock,
// each on its own failed invoice, and returns the clock time.
func seedClockDueRuns(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, clockID string) time.Time {
	t.Helper()
	frozen := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'Clock Due Held', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode) VALUES ($1, $2, 'held', $3, 'advancing', false)`, clockID, tenantID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ('cus_due_held', $1, 'due_held', 'Held', '', $2, $3, $3)`, tenantID, clockID, frozen)
	mustExec(`INSERT INTO dunning_policies (id, tenant_id, name, enabled, is_default, retry_days, final_subscription_action, final_invoice_action, livemode)
		VALUES ('dpol_due_held', $1, 'P', true, true, '[3, 3, 3]', 'pause', 'none', false)`, tenantID)
	for _, n := range []string{"1", "2", "3"} {
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, is_simulated, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, tax_status, billing_period_start, billing_period_end, created_at, updated_at)
			VALUES ($1, $2, 'cus_due_held', $1, 'finalized', 'failed', true, 'USD', 2500, 2500, 2500, 'ok', $3, $3, $3, $3)`,
			"inv_due_held_"+n, tenantID, frozen.Add(-24*time.Hour))
		mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
			VALUES ($1, $2, $3, 'cus_due_held', 'dpol_due_held', 'active', 'payment_failed', 0, $4, false, $5, $5, false)`,
			"run_due_held_"+n, tenantID, "inv_due_held_"+n, frozen.Add(-time.Hour), frozen.Add(-24*time.Hour))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	return frozen
}

// The clock's due list returns a run whose invoice another transaction holds,
// a settle for example. A clock has one runner, so its dunning job reaches the
// run and its own conditional writes decide; a skipped run waited for a later
// pass, if one came. The list takes no lock, so it never locks a run before
// its invoice, the order every settle uses the other way round.
func TestListDueRunsForClock_ReturnsARunWhoseInvoiceIsHeld(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := "tnt_clock_due_held"
	const clockID = "hc_tclk_due_held"
	frozen := seedClockDueRuns(t, db, ctx, tenantID, clockID)

	holder, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer postgres.Rollback(holder)
	if _, err := holder.ExecContext(ctx, `SELECT id FROM invoices WHERE id = 'inv_due_held_2' FOR UPDATE`); err != nil {
		t.Fatalf("hold invoice: %v", err)
	}

	due, err := dunning.NewPostgresStore(db).ListDueRunsForClock(ctx, tenantID, clockID, frozen, 50)
	if err != nil {
		t.Fatalf("ListDueRunsForClock: %v", err)
	}
	if len(due) != 3 {
		t.Errorf("due runs = %d, want 3: the run whose invoice is held was skipped", len(due))
	}
}

// noAnswerRetrier gets no definite answer for any retry, so dunning rewinds
// the attempt and the run stays due.
type noAnswerRetrier struct{ calls int }

func (r *noAnswerRetrier) RetryPayment(context.Context, string, string, string) error {
	r.calls++
	return dunning.ErrTransientSkip
}

// A run whose retry gets no definite answer stays due and stays in the list.
// The clock's dunning job tries each such run once and ends; it does not
// retry them again and again within one pass.
func TestProcessDueRunsForClock_RunsThatStayListed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := "tnt_clock_due_stay"
	const clockID = "hc_tclk_due_stay"
	frozen := seedClockDueRuns(t, db, ctx, tenantID, clockID)

	retrier := &noAnswerRetrier{}
	svc := dunning.NewService(dunning.NewPostgresStore(db), retrier, clock.Real())
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = svc.ProcessDueRunsForClock(runCtx, tenantID, clockID, frozen, 50)
		close(done)
	}()
	select {
	case <-done:
	case <-runCtx.Done():
		t.Fatal("the job did not end: it retried the runs that stay due again")
	}
	if retrier.calls != 3 {
		t.Errorf("retries = %d, want 3: each run once", retrier.calls)
	}
}
