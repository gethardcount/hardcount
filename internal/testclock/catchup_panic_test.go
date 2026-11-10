package testclock

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestRunAdvancing_APanickingPassDoesNotStopTheTick: a panic in one clock's
// pass (a nil-deref in some billing path) is the clock's alone. runClock
// recovers it and fails that clock, and the tick goes on to the next clock.
// Without the recover, the panic would unwind the tick: the scheduler's
// recover keeps the process alive, but every later clock would wait for the
// next tick, which would panic on the same clock first.
func TestRunAdvancing_APanickingPassDoesNotStopTheTick(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMockStore()
	for _, id := range []string{"c1", "c2"} {
		store.clocks[id] = domain.TestClock{ID: id, TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: at}
	}
	s := NewService(store)
	s.SetBillingRunner(&panickingRunner{})

	worked, err := s.RunAdvancing(context.Background())
	if err != nil {
		t.Fatalf("RunAdvancing: %v", err)
	}
	if worked != 2 {
		t.Errorf("clocks run: got %d, want 2", worked)
	}
	for _, id := range []string{"c1", "c2"} {
		if got := store.clocks[id].Status; got != domain.TestClockStatusInternalFailed {
			t.Errorf("%s: status %q, want internal_failure", id, got)
		}
	}
}

// panickingRunner blows up inside the billing phase — the scheduler's
// recover keeps the process alive, but pre-fix nothing flipped
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
