package testclock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// ctxCheckingStore embeds the unit mockStore but makes MarkFailed fail when its
// context is already canceled/expired — modelling the real store, where a write
// on a dead ctx can't land. This is the condition the fix addresses: the
// catchup ctx hits CatchupTimeout, so reusing it for the failure-flip would
// also fail and leave the clock stuck in 'advancing'.
type ctxCheckingStore struct {
	*mockStore
	markFailedSawLiveCtx bool
}

func (s *ctxCheckingStore) MarkFailed(ctx context.Context, tenantID, id, reason string) (domain.TestClock, error) {
	if ctx.Err() != nil {
		return domain.TestClock{}, ctx.Err()
	}
	s.markFailedSawLiveCtx = true
	return s.mockStore.MarkFailed(ctx, tenantID, id, reason)
}

// TestRunCatchup_MarksFailedOnExpiredCtx covers the medium-severity audit
// finding: when catchup timed out, MarkFailed ran on the already-expired
// catchup ctx and failed, leaving the clock stuck in 'advancing' forever. The
// failure-flip now runs on a ctx detached from the catchup deadline.
func TestRunCatchup_MarksFailedOnExpiredCtx(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := newMockStore()
	base.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: start,
	}
	sub := domain.Subscription{ID: "s1", TenantID: "t1", TestClockID: "c1", Status: domain.SubscriptionActive}
	nextBilling := start.Add(1 * time.Hour)
	sub.NextBillingAt = &nextBilling
	base.subsOnClock["c1"] = []domain.Subscription{sub}

	store := &ctxCheckingStore{mockStore: base}
	runner := &stubRunner{store: base, clockID: "c1", subID: "s1", err: errors.New("catchup deadline exceeded")}
	s := NewService(store)
	s.SetBillingRunner(runner)

	// Simulate the catchup ctx having hit CatchupTimeout: already canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.RunCatchup(ctx, CatchupJob{TenantID: "t1", ClockID: "c1"})
	if err == nil {
		t.Fatal("expected RunCatchup to return the catchup error")
	}

	if !store.markFailedSawLiveCtx {
		t.Error("MarkFailed was not called with a live (detached) ctx — failure-flip reused the expired catchup ctx")
	}
	got := store.clocks["c1"]
	if got.Status != domain.TestClockStatusInternalFailed {
		t.Errorf("status: got %q, want internal_failure (clock must not be left stuck in 'advancing')", got.Status)
	}
	if got.LastFailureReason == "" {
		t.Error("last_failure_reason should be populated")
	}
}
