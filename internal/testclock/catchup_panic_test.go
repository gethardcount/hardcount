package testclock

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestCatchupProcess_RecoversPanic covers the medium-severity audit finding:
// the catchup worker goroutine had no panic recovery, so a panic in the runner
// (a nil-deref in some billing path) would unwind the goroutine and crash the
// whole process, taking down the API for every tenant. process() now recovers,
// logs, and returns so the drain loop continues.
func TestCatchupProcess_RecoversPanic(t *testing.T) {
	w := NewCatchupWorker(NewCatchupQueue(1), func(_ context.Context, _ CatchupJob) error {
		panic("boom: nil deref in billing path")
	})

	// If process did not recover, the panic would propagate and fail the test.
	// Reaching the line after the call proves recovery.
	w.process(CatchupJob{TenantID: "t1", ClockID: "c1"})
}

// panickingRunner blows up inside the billing phase — the worker's (or
// chi's) recover keeps the process alive, but pre-fix nothing flipped
// the clock out of 'advancing', a state with no operator exit (Advance
// needs 'ready', Retry advance needs 'internal_failure').
type panickingRunner struct{ stubRunner }

func (r *panickingRunner) RunCycleForClock(_ context.Context, _, _ string, _ int) (int, []error) {
	panic("boom: nil deref in a billing phase")
}

// TestRunCatchup_PanicMarksClockFailed locks the Tier A fix from the
// test-clock design audit: a panic mid-catchup must land the clock in
// internal_failure (operator can Retry advance or delete) instead of
// stranding it at 'advancing' forever, and the failure reason must not
// leak the panic string into the dashboard banner.
func TestRunCatchup_PanicMarksClockFailed(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: start,
	}
	sub := domain.Subscription{ID: "s1", TenantID: "t1", TestClockID: "c1", Status: domain.SubscriptionActive}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	store.subsOnClock["c1"] = []domain.Subscription{sub}

	s := NewService(store)
	s.SetBillingRunner(&panickingRunner{})

	err := s.RunCatchup(context.Background(), CatchupJob{TenantID: "t1", ClockID: "c1"})
	if err == nil {
		t.Fatal("expected RunCatchup to surface the panic as an error")
	}

	got := store.clocks["c1"]
	if got.Status != domain.TestClockStatusInternalFailed {
		t.Errorf("status: got %q, want internal_failure (panic must not strand the clock at 'advancing')", got.Status)
	}
	if got.LastFailureReason == "" {
		t.Error("last_failure_reason should be populated")
	}
	if strings.Contains(got.LastFailureReason, "boom") || strings.Contains(got.LastFailureReason, "panic") {
		t.Errorf("operator-facing reason must not leak panic internals, got %q", got.LastFailureReason)
	}
}
