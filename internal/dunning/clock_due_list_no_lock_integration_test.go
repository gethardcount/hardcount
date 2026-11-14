package dunning_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
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
	// Inserted in reverse id order: a list that does not break the tie by id
	// would hand them back in insert order, not id order.
	for _, n := range []string{"3", "2", "1"} {
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

// The clock's due list returns a run whose invoice or run row another
// transaction holds (a settle locks the invoice, then writes the run), and
// returns at once. A clock has one runner, so its dunning job reaches the run
// and the run's own conditional writes decide; a skipped run waited for a
// later pass, if one came, and a list that waited would stall the advance
// behind the settle. The list takes no lock, so it also never locks a run
// before its invoice, the order every settle uses the other way round.
func TestListDueRunsForClock_ReturnsARunWhoseInvoiceIsHeld(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := "tnt_clock_due_held"
	const clockID = "hc_tclk_due_held"
	frozen := seedClockDueRuns(t, db, ctx, tenantID, clockID)

	for _, held := range []struct{ name, lock string }{
		{"invoice", `SELECT id FROM invoices WHERE id = 'inv_due_held_2' FOR UPDATE`},
		{"run", `SELECT id FROM invoice_dunning_runs WHERE id = 'run_due_held_3' FOR UPDATE`},
	} {
		holder, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin holder: %v", err)
		}
		if _, err := holder.ExecContext(ctx, held.lock); err != nil {
			t.Fatalf("hold the %s: %v", held.name, err)
		}
		listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		due, err := dunning.NewPostgresStore(db).ListDueRunsForClock(listCtx, tenantID, clockID, frozen, walk.Key{}, 50)
		cancel()
		postgres.Rollback(holder)
		if err != nil {
			t.Fatalf("%s held: ListDueRunsForClock: %v (a list that waits for the held row takes a lock)", held.name, err)
		}
		if len(due) != 3 {
			t.Errorf("%s held: due runs = %d, want 3: the run was skipped", held.name, len(due))
		}
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
	var errs []error
	go func() {
		_, errs = svc.ProcessDueRunsForClock(runCtx, tenantID, clockID, frozen, 50)
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
	// Each is left for the next run, so the advance fails.
	if len(errs) != 3 {
		t.Fatalf("errors = %v, want 3", errs)
	}
	for _, err := range errs {
		if !errors.Is(err, domain.ErrLeftForNextRun) {
			t.Errorf("error %v: want ErrLeftForNextRun", err)
		}
	}
}

// The clock's due list pages by (next_action_at, id): one run a page reaches
// each run once, in that order, though all three are due at the same instant.
func TestListDueRunsForClock_PagesByNextActionAtThenID(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := "tnt_clock_due_pages"
	const clockID = "hc_tclk_due_pages"
	frozen := seedClockDueRuns(t, db, ctx, tenantID, clockID)
	store := dunning.NewPostgresStore(db)

	list := func(ctx context.Context, after walk.Key, limit int) ([]dunning.DueRun, error) {
		return store.ListDueRunsForClock(ctx, tenantID, clockID, frozen, after, limit)
	}
	key := func(r dunning.DueRun) walk.Key { return walk.Key{At: *r.NextActionAt, ID: r.ID} }
	var got []string
	for run, err := range walk.Walk(ctx, "ListDueRunsForClock", list, key, 1) {
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		got = append(got, run.ID)
	}
	if want := []string{"run_due_held_1", "run_due_held_2", "run_due_held_3"}; !slices.Equal(got, want) {
		t.Errorf("walked %v, want %v", got, want)
	}
}
