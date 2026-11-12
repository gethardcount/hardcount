package testclock

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// ctxCheckingStore embeds the unit mockStore but makes MarkFailed fail when its
// context is already canceled/expired — modelling the real store, where a write
// on a dead ctx can't land. This is the condition the detached failure-flip
// addresses: the pass's ctx hits its deadline, so reusing it for the
// failure-flip would also fail and leave the clock stuck in 'advancing'.
type ctxCheckingStore struct {
	*mockStore
	markFailedSawLiveCtx bool
}

func (s *ctxCheckingStore) MarkFailed(ctx context.Context, tenantID, id, reason string, sum domain.AdvanceSummary) (domain.TestClock, error) {
	if ctx.Err() != nil {
		return domain.TestClock{}, ctx.Err()
	}
	s.markFailedSawLiveCtx = true
	return s.mockStore.MarkFailed(ctx, tenantID, id, reason, sum)
}

// blockingRunner's billing phase runs until its ctx ends, like a pass that
// would take longer than its deadline.
type blockingRunner struct{ stubRunner }

func (r *blockingRunner) RunCycleForClock(ctx context.Context, _, _ string, _ int) (int, []error) {
	<-ctx.Done()
	return 0, []error{ctx.Err()}
}

// TestRunCatchup_PassDeadlineMarksFailedOnALiveCtx: a pass that overruns its
// deadline fails the advance with a reason that names the clock's time, and
// the failure-flip runs on a ctx detached from the pass, so it lands: on the
// expired ctx it could not, and the clock would stay 'advancing'.
func TestRunCatchup_PassDeadlineMarksFailedOnALiveCtx(t *testing.T) {
	prev := catchupPassDeadline
	catchupPassDeadline = 50 * time.Millisecond
	t.Cleanup(func() { catchupPassDeadline = prev })

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := newMockStore()
	base.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: at,
	}
	store := &ctxCheckingStore{mockStore: base}
	s := NewService(store, &blockingRunner{})

	if err := s.RunCatchup(context.Background(), "t1", "c1"); err == nil {
		t.Fatal("expected RunCatchup to return the overrun as an error")
	}
	if !store.markFailedSawLiveCtx {
		t.Error("MarkFailed was not called with a live (detached) ctx — the failure-flip reused the expired pass ctx")
	}
	got := base.clocks["c1"]
	if got.Status != domain.TestClockStatusInternalFailed {
		t.Errorf("status: got %q, want internal_failure (clock must not be left stuck in 'advancing')", got.Status)
	}
	if !strings.Contains(got.LastFailureReason, "the catch-up pass at 2026-01-01T00:00:00Z did not finish within 50ms") {
		t.Errorf("last_failure_reason should name the clock's time and the deadline, got %q", got.LastFailureReason)
	}
}

// TestRunCatchup_EndedCtxLeavesTheClockAdvancing: when the tick's own ctx
// ends mid-pass — a shutdown, or the testclock_catchup lease lost — the pass
// stops and the clock stays advancing, unmarked, for the next leader to run
// the pass again.
func TestRunCatchup_EndedCtxLeavesTheClockAdvancing(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := newMockStore()
	base.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing, FrozenTime: at,
	}
	store := &ctxCheckingStore{mockStore: base}
	s := NewService(store, &stubRunner{store: base, clockID: "c1", subID: "s1", err: errors.New("context canceled")})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RunCatchup(ctx, "t1", "c1"); err != nil {
		t.Fatalf("RunCatchup on an ended ctx: %v, want nil (the next leader runs the pass)", err)
	}
	if store.markFailedSawLiveCtx {
		t.Error("MarkFailed ran: an ended tick must leave the clock for the next leader, not fail it")
	}
	if got := base.clocks["c1"].Status; got != domain.TestClockStatusAdvancing {
		t.Errorf("status: got %q, want advancing", got)
	}
}
