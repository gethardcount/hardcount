package testclock_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The catch-up is the testclock_catchup leader's (ADR-140). These tests pin
// that every write it makes to a clock row proves the lease, and what a write
// that misses reports.

var (
	leaseFrom   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	leaseTarget = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
)

// advancingClock creates a clock at leaseFrom and advances it to leaseTarget.
func advancingClock(t *testing.T, store *testclock.PostgresStore, ctx context.Context, tenantID, name string) domain.TestClock {
	t.Helper()
	clk, err := store.Create(ctx, tenantID, domain.TestClock{Name: name, FrozenTime: leaseFrom})
	if err != nil {
		t.Fatalf("create clock %s: %v", name, err)
	}
	clk, err = store.MarkAdvancing(ctx, tenantID, clk.ID, leaseTarget)
	if err != nil {
		t.Fatalf("mark %s advancing: %v", name, err)
	}
	return clk
}

// TestCatchupWrites_FencedOnTheLease: a run that no longer holds the lease
// (another replica took it over) changes nothing on the clock — not the
// summary, not the status, not the failure reason — and its writes say so
// with ErrCatchupFenceLost. A ctx that never led is refused before any query.
// The current leader's write goes through.
func TestCatchupWrites_FencedOnTheLease(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Catchup Fence")
	clk := advancingClock(t, store, base, tenantID, "fenced")

	deposed := leadertest.Token(t, admin, base, leader.RoleTestClockCatchup)
	current := leadertest.Token(t, admin, base, leader.RoleTestClockCatchup) // takes the role over

	if err := store.SaveAdvanceSummary(deposed, tenantID, clk.ID, []byte(`{"invoices_generated": 9}`)); !errors.Is(err, testclock.ErrCatchupFenceLost) {
		t.Errorf("deposed SaveAdvanceSummary: %v, want ErrCatchupFenceLost", err)
	}
	if _, err := store.MarkFailed(deposed, tenantID, clk.ID, "deposed"); !errors.Is(err, testclock.ErrCatchupFenceLost) {
		t.Errorf("deposed MarkFailed: %v, want ErrCatchupFenceLost", err)
	}
	if _, err := store.CompleteAdvance(deposed, tenantID, clk.ID); !errors.Is(err, testclock.ErrCatchupFenceLost) {
		t.Errorf("deposed CompleteAdvance: %v, want ErrCatchupFenceLost", err)
	}
	if _, err := store.CompleteAdvance(base, tenantID, clk.ID); !errors.Is(err, leader.ErrNoToken) {
		t.Errorf("CompleteAdvance with no lease: %v, want leader.ErrNoToken", err)
	}

	got, err := store.Get(base, tenantID, clk.ID)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if got.Status != domain.TestClockStatusAdvancing || got.LastFailureReason != "" {
		t.Errorf("after the deposed writes: status %q reason %q, want advancing and no reason", got.Status, got.LastFailureReason)
	}
	if got.LastAdvanceSummary == nil || got.LastAdvanceSummary.InvoicesGenerated != 0 || !got.LastAdvanceSummary.AdvancedFrom.Equal(leaseFrom) {
		t.Errorf("summary after the deposed write: %+v, want MarkAdvancing's (advanced_from %s, no counts)", got.LastAdvanceSummary, leaseFrom)
	}

	done, err := store.CompleteAdvance(current, tenantID, clk.ID)
	if err != nil || done.Status != domain.TestClockStatusReady {
		t.Fatalf("current leader's CompleteAdvance: %v status %q, want ready", err, done.Status)
	}
}

// TestCatchupWrites_ClockNoLongerAdvancing: a fenced write on a clock that is
// out of advancing — completed by another run, or deleted by its tenant — is
// not a lost lease and not a failure: it reports ErrClockNoLongerAdvancing,
// and nothing changes.
func TestCatchupWrites_ClockNoLongerAdvancing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	ctx := leadertest.Token(t, testutil.AdminPool(t), base, leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Catchup Moved")

	t.Run("completed", func(t *testing.T) {
		clk := advancingClock(t, store, base, tenantID, "completed")
		if _, err := store.CompleteAdvance(ctx, tenantID, clk.ID); err != nil {
			t.Fatalf("complete: %v", err)
		}
		if _, err := store.CompleteAdvance(ctx, tenantID, clk.ID); !errors.Is(err, testclock.ErrClockNoLongerAdvancing) {
			t.Errorf("second CompleteAdvance: %v, want ErrClockNoLongerAdvancing", err)
		}
		if _, err := store.MarkFailed(ctx, tenantID, clk.ID, "late"); !errors.Is(err, testclock.ErrClockNoLongerAdvancing) {
			t.Errorf("MarkFailed on a ready clock: %v, want ErrClockNoLongerAdvancing", err)
		}
		if err := store.SaveAdvanceSummary(ctx, tenantID, clk.ID, []byte(`{"invoices_generated": 9}`)); !errors.Is(err, testclock.ErrClockNoLongerAdvancing) {
			t.Errorf("SaveAdvanceSummary on a ready clock: %v, want ErrClockNoLongerAdvancing", err)
		}
		got, err := store.Get(base, tenantID, clk.ID)
		if err != nil || got.Status != domain.TestClockStatusReady || got.LastFailureReason != "" || got.LastAdvanceSummary.InvoicesGenerated != 0 {
			t.Errorf("clock after the late writes: %+v (%v), want ready, unchanged", got, err)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		clk := advancingClock(t, store, base, tenantID, "deleted")
		if err := store.Delete(base, tenantID, clk.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := store.CompleteAdvance(ctx, tenantID, clk.ID); !errors.Is(err, testclock.ErrClockNoLongerAdvancing) {
			t.Errorf("CompleteAdvance on a deleted clock: %v, want ErrClockNoLongerAdvancing", err)
		}
	})
}

// TestListAdvancingForCatchup_FencedAcrossTenants: the role's claim list
// spans every tenant, holds only advancing clocks, the one touched longest
// ago first, and is empty for a deposed leader.
func TestListAdvancingForCatchup_FencedAcrossTenants(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	tenantA := testutil.CreateTestTenant(t, db, "Catchup List A")
	tenantB := testutil.CreateTestTenant(t, db, "Catchup List B")

	first := advancingClock(t, store, base, tenantA, "first")
	second := advancingClock(t, store, base, tenantB, "second")
	ready, err := store.Create(base, tenantA, domain.TestClock{Name: "ready", FrozenTime: leaseFrom})
	if err != nil {
		t.Fatalf("create ready clock: %v", err)
	}

	deposed := leadertest.Token(t, admin, base, leader.RoleTestClockCatchup)
	current := leadertest.Token(t, admin, base, leader.RoleTestClockCatchup)

	listed, err := store.ListAdvancingForCatchup(current, 1000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var mine []string
	for _, c := range listed {
		switch c.ID {
		case first.ID, second.ID, ready.ID:
			mine = append(mine, c.ID)
		}
	}
	if len(mine) != 2 || mine[0] != first.ID || mine[1] != second.ID {
		t.Errorf("listed: %v, want [%s %s] (both tenants, advancing only, oldest first)", mine, first.ID, second.ID)
	}

	stale, err := store.ListAdvancingForCatchup(deposed, 1000)
	if err != nil {
		t.Fatalf("deposed list: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("deposed leader listed %d clocks, want 0", len(stale))
	}
}

// failingRunner fails its billing phase.
type failingRunner struct{}

func (failingRunner) RunCycleForClock(context.Context, string, string, int) (int, []error) {
	return 0, []error{errors.New("phase failed")}
}
func (failingRunner) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (failingRunner) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}

// TestRunCatchup_DeposedLeaderChangesNothing: two catch-up leaders at once
// on one clock, as during a takeover. The deposed one's pass ends without
// marking the clock failed or complete; the current one completes it once.
func TestRunCatchup_DeposedLeaderChangesNothing(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Catchup Deposed")
	clk := advancingClock(t, store, base, tenantID, "two leaders")
	job := testclock.CatchupJob{TenantID: tenantID, ClockID: clk.ID}

	deposed := leadertest.Token(t, admin, base, leader.RoleTestClockCatchup)
	current := leadertest.Token(t, admin, base, leader.RoleTestClockCatchup)

	failing := testclock.NewService(store)
	failing.SetBillingRunner(failingRunner{})
	if err := failing.RunCatchup(deposed, job); err != nil {
		t.Errorf("deposed failing pass: %v, want nil (its MarkFailed misses the fence)", err)
	}
	completing := testclock.NewService(store) // no billing: the pass is just CompleteAdvance
	if err := completing.RunCatchup(deposed, job); err != nil {
		t.Errorf("deposed completing pass: %v, want nil (its CompleteAdvance misses the fence)", err)
	}
	if got, _ := store.Get(base, tenantID, clk.ID); got.Status != domain.TestClockStatusAdvancing || got.LastFailureReason != "" {
		t.Fatalf("after the deposed passes: status %q reason %q, want advancing and no reason", got.Status, got.LastFailureReason)
	}

	if err := completing.RunCatchup(current, job); err != nil {
		t.Fatalf("current leader's pass: %v", err)
	}
	if got, _ := store.Get(base, tenantID, clk.ID); got.Status != domain.TestClockStatusReady {
		t.Errorf("after the current leader's pass: status %q, want ready", got.Status)
	}
}

// readyUnderPassRunner's billing phase flips the clock to ready with an
// unfenced write, as a replica still on the previous version does with its
// own CompleteAdvance during a rolling deploy.
type readyUnderPassRunner struct {
	admin   *sql.DB
	clockID string
}

func (r readyUnderPassRunner) RunCycleForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	if _, err := r.admin.ExecContext(ctx, `UPDATE test_clocks SET status = 'ready', updated_at = now() WHERE id = $1`, r.clockID); err != nil {
		return 0, []error{err}
	}
	return 1, nil
}
func (readyUnderPassRunner) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (readyUnderPassRunner) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}

// TestRunCatchup_ClockTurnsReadyUnderThePass: the upgrade overlap. While the
// leader's pass runs, the previous version's worker completes the same clock
// with its unfenced write. The pass then finds the clock out of advancing:
// it ends without error and changes nothing — no summary of its own, no
// failure — and the clock stays ready.
func TestRunCatchup_ClockTurnsReadyUnderThePass(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	ctx := leadertest.Token(t, admin, base, leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Catchup Overlap")
	clk := advancingClock(t, store, base, tenantID, "overlap")

	svc := testclock.NewService(store)
	svc.SetBillingRunner(readyUnderPassRunner{admin: admin, clockID: clk.ID})
	if err := svc.RunCatchup(ctx, testclock.CatchupJob{TenantID: tenantID, ClockID: clk.ID}); err != nil {
		t.Fatalf("RunCatchup: %v, want nil (the clock was completed under it)", err)
	}
	got, err := store.Get(base, tenantID, clk.ID)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if got.Status != domain.TestClockStatusReady || got.LastFailureReason != "" {
		t.Errorf("status %q reason %q, want ready and no reason", got.Status, got.LastFailureReason)
	}
	if got.LastAdvanceSummary == nil || got.LastAdvanceSummary.InvoicesGenerated != 0 {
		t.Errorf("summary %+v: the pass wrote its counts onto a clock it no longer ran", got.LastAdvanceSummary)
	}
}

// countingClockRunner reports two invoices from its billing phase.
type countingClockRunner struct{}

func (countingClockRunner) RunCycleForClock(context.Context, string, string, int) (int, []error) {
	return 2, nil
}
func (countingClockRunner) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (countingClockRunner) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}

// TestRunAdvancing_RetryKeepsAdvancedFrom: an advance that failed and is
// retried, then run by the tick, still reports the span from the time before
// the advance — not the target (what the queue job carried on a Retry) and
// not zero (what it carried after a restart).
func TestRunAdvancing_RetryKeepsAdvancedFrom(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	ctx := leadertest.Token(t, testutil.AdminPool(t), base, leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Catchup Retry")
	clk := advancingClock(t, store, base, tenantID, "retried")

	if _, err := store.MarkFailed(ctx, tenantID, clk.ID, "first try failed"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	svc := testclock.NewService(store)
	svc.SetBillingRunner(countingClockRunner{})
	if _, err := svc.RetryAdvance(base, tenantID, clk.ID); err != nil {
		t.Fatalf("retry advance: %v", err)
	}
	if _, err := svc.RunAdvancing(ctx); err != nil {
		t.Fatalf("RunAdvancing: %v", err)
	}
	got, err := store.Get(base, tenantID, clk.ID)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if got.Status != domain.TestClockStatusReady {
		t.Fatalf("status %q, want ready", got.Status)
	}
	sum := got.LastAdvanceSummary
	if sum == nil || !sum.AdvancedFrom.Equal(leaseFrom) || !sum.AdvancedTo.Equal(leaseTarget) || sum.InvoicesGenerated != 2 {
		t.Errorf("summary %+v, want %s → %s with 2 invoices", sum, leaseFrom, leaseTarget)
	}
}
