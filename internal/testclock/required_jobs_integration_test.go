package testclock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testclock/testclocktest"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// flakyCreditExpirer fails its first call and succeeds after.
type flakyCreditExpirer struct{ calls int }

func (f *flakyCreditExpirer) ExpireCreditsForClock(context.Context, string, string, time.Time) (int, []error) {
	f.calls++
	if f.calls == 1 {
		return 0, []error{errors.New("credit expiry could not finish")}
	}
	return 0, nil
}

// TestAdvance_FailedJobThenRetryCompletes: a job that fails leaves the clock
// failed with a reason and a summary that says it failed. Retry advance runs
// the pass again, and the clock reaches its target.
func TestAdvance_FailedJobThenRetryCompletes(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	ctx := leadertest.Token(t, testutil.AdminPool(t), base, leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Failed Job Then Retry")
	clk := advancingClock(t, store, base, tenantID, "failed job")

	credits := &flakyCreditExpirer{}
	clockDeps := testclocktest.Deps(countingClockRunner{})
	clockDeps.Credits = credits
	svc := testclock.NewService(store, clockDeps)

	if _, err := svc.RunAdvancing(ctx); err != nil {
		t.Fatalf("RunAdvancing: %v", err)
	}
	got, err := store.Get(base, tenantID, clk.ID)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if got.Status != domain.TestClockStatusInternalFailed || got.LastFailureReason == "" {
		t.Fatalf("after the failed job: status %q reason %q, want internal_failure with a reason", got.Status, got.LastFailureReason)
	}
	if sum := got.LastAdvanceSummary; sum == nil || !sum.HadErrors || !sum.AdvancedTo.Equal(leaseTarget) {
		t.Errorf("summary after the failed job = %+v, want advanced_to %s and had_errors", sum, leaseTarget)
	}

	if _, err := svc.RetryAdvance(base, tenantID, clk.ID); err != nil {
		t.Fatalf("retry advance: %v", err)
	}
	if _, err := svc.RunAdvancing(ctx); err != nil {
		t.Fatalf("RunAdvancing after retry: %v", err)
	}
	got, err = store.Get(base, tenantID, clk.ID)
	if err != nil {
		t.Fatalf("get clock: %v", err)
	}
	if got.Status != domain.TestClockStatusReady || !got.FrozenTime.Equal(leaseTarget) {
		t.Errorf("after the retry: status %q at %s, want ready at %s", got.Status, got.FrozenTime, leaseTarget)
	}
	if credits.calls != 2 {
		t.Errorf("credit expiry ran %d times, want 2 (the failed pass and the retry)", credits.calls)
	}
}

// tickEndingRunner ends the tick on its first pass and counts its passes.
type tickEndingRunner struct {
	countingClockRunner
	endTick context.CancelFunc
	passes  *int
}

func (r tickEndingRunner) RunCycleForClock(ctx context.Context, tenantID, clockID string, n int) (int, []error) {
	*r.passes++
	r.endTick()
	return r.countingClockRunner.RunCycleForClock(ctx, tenantID, clockID, n)
}

// TestRunAdvancing_TickEndsDuringTheFirstClock: when the tick ends during a
// pass, the clocks after it are not started. They stay advancing for the next
// tick, and are not counted as worked.
func TestRunAdvancing_TickEndsDuringTheFirstClock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := testclock.NewPostgresStore(db)
	base := postgres.WithLivemode(context.Background(), false)
	leased := leadertest.Token(t, testutil.AdminPool(t), base, leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Tick Ends")
	first := advancingClock(t, store, base, tenantID, "first")
	second := advancingClock(t, store, base, tenantID, "second")

	tick, endTick := context.WithCancel(leased)
	defer endTick()
	passes := 0
	svc := testclock.NewService(store, testclocktest.Deps(tickEndingRunner{endTick: endTick, passes: &passes}))

	worked, err := svc.RunAdvancing(tick)
	if err != nil {
		t.Fatalf("RunAdvancing: %v", err)
	}
	if worked != 1 || passes != 1 {
		t.Errorf("worked %d with %d passes, want 1 and 1: the tick ended during the first clock", worked, passes)
	}
	for _, c := range []domain.TestClock{first, second} {
		got, err := store.Get(base, tenantID, c.ID)
		if err != nil {
			t.Fatalf("get clock: %v", err)
		}
		if got.Status != domain.TestClockStatusAdvancing {
			t.Errorf("clock %s status %q, want advancing for the next tick", c.Name, got.Status)
		}
	}
}
