package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// The cancel's attempts (ADR-139), with the memory store and a fake biller
// whose writer can answer ErrWatermarkMoved as a change that committed while
// the cancel was priced would. The real-Postgres races are in
// internal/billing/cancel_plans_before_tx_integration_test.go.

// One moved snapshot: the cancel plans again and commits once.
//
// Mutation M6 (one attempt) → the cancel fails.
func TestCancel_SnapshotMovedOnce_PlansAgainAndCommits(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	fb.writeMoved = 1

	canceled, _, err := svc.Cancel(context.Background(), "t1", id)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled {
		t.Fatalf("status = %s, want canceled", canceled.Status)
	}
	if fb.finalCalls != 2 || fb.draftTxCalls != 1 || fb.afterCommitCalls != 1 {
		t.Errorf("plans=%d writes=%d after-commit=%d, want 2 plans, 1 write, 1 after-commit", fb.finalCalls, fb.draftTxCalls, fb.afterCommitCalls)
	}
}

// Two moved snapshots in a row: the cancel returns ErrWatermarkMoved and the
// subscription stays as it was. The handler answers it 409; dunning sees the
// error.
func TestCancel_SnapshotMovedTwice_ReturnsErrWatermarkMoved(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	fb.writeMoved = 2

	if _, _, err := svc.Cancel(context.Background(), "t1", id); !errors.Is(err, ErrWatermarkMoved) {
		t.Fatalf("Cancel: %v, want ErrWatermarkMoved", err)
	}
	if fb.finalCalls != 2 || fb.draftTxCalls != 0 || fb.finishCalls != 0 || fb.afterCommitCalls != 0 {
		t.Errorf("plans=%d writes=%d finish=%d after-commit=%d, want 2 plans and nothing written", fb.finalCalls, fb.draftTxCalls, fb.finishCalls, fb.afterCommitCalls)
	}
	if sub, err := store.Get(context.Background(), "t1", id); err != nil || sub.Status != domain.SubscriptionActive {
		t.Errorf("subscription: %v status=%s, want active", err, sub.Status)
	}
}

// Each attempt runs on the caller's instant (a request's or dunning's), so a
// second attempt plans at the same instant as the first.
//
// Mutation: plan each attempt on a fresh read of the server's clock, ignoring
// the caller's instant → neither plan is at the caller's instant.
func TestCancel_ASecondAttemptPlansAtTheCallersInstant(t *testing.T) {
	// Both instants fall on one day: the subscription's period starts at that
	// day's midnight, before the cancel, whatever the hour the test runs.
	noon := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	server := &subSteppingClock{at: noon.Add(time.Hour)}
	svc := NewService(newMemStore(), server)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	fb.writeMoved = 1
	server.reads = 0

	at := noon.Add(time.Minute)
	if _, _, err := svc.Cancel(clock.WithEffectiveNow(context.Background(), at), "t1", id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(fb.planAts) != 2 || !fb.planAts[0].Equal(at) || !fb.planAts[1].Equal(at) || server.reads != 0 {
		t.Errorf("plan instants = %v, server clock reads %d; want two, both %v, none", fb.planAts, server.reads, at)
	}
}

// An active row whose period starts after the cancel instant is not planned:
// priced from it, the final invoice would bill nothing and the relief would
// skip the new period's prebill, while the compare-and-set passes. Both
// attempts refuse it (each runs on the caller's instant), and the cancel
// returns ErrWatermarkMoved with nothing planned or written.
//
// Mutation M8 (drop the period-start rule) → the cancel commits at an instant
// before its period.
func TestCancel_PeriodStartsAfterTheInstant_NotPlanned(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	sub, err := store.Get(context.Background(), "t1", id)
	if err != nil || sub.CurrentBillingPeriodStart == nil {
		t.Fatalf("read the seeded subscription: %v", err)
	}
	ctx := clock.WithEffectiveNow(context.Background(), sub.CurrentBillingPeriodStart.Add(-time.Minute))

	if _, _, err := svc.Cancel(ctx, "t1", id); !errors.Is(err, ErrWatermarkMoved) {
		t.Fatalf("Cancel: %v, want ErrWatermarkMoved", err)
	}
	if fb.finalCalls != 0 {
		t.Errorf("plans = %d, want 0: a row whose period starts after the instant is not priced", fb.finalCalls)
	}
	if got, err := store.Get(context.Background(), "t1", id); err != nil || got.Status != domain.SubscriptionActive {
		t.Errorf("subscription: %v status=%s, want active", err, got.Status)
	}
}

// A row the cancel reads as canceled is answered as already terminated before
// anything is priced.
func TestCancel_AlreadyCanceled_NotPriced(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	if _, _, err := svc.Cancel(context.Background(), "t1", id); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	plans := fb.finalCalls
	if _, _, err := svc.Cancel(context.Background(), "t1", id); err == nil || errors.Is(err, ErrWatermarkMoved) {
		t.Fatalf("second Cancel: %v, want already terminated", err)
	}
	if fb.finalCalls != plans {
		t.Errorf("the second cancel priced %d time(s), want 0", fb.finalCalls-plans)
	}
}

// sameIntervals compares rows on item, plan, quantity and start, and ignores
// ends_at, which the cancel's own seal writes before it compares.
func TestSameIntervals(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(48 * time.Hour)
	row := func(item, plan string, qty int64, start time.Time, end *time.Time) domain.ItemInterval {
		return domain.ItemInterval{SubscriptionItemID: item, PlanID: plan, Quantity: qty, StartsAt: start, EndsAt: end}
	}
	base := []domain.ItemInterval{row("si_1", "pln_1", 1, t0, nil)}
	for _, c := range []struct {
		name  string
		other []domain.ItemInterval
		same  bool
	}{
		{"identical", []domain.ItemInterval{row("si_1", "pln_1", 1, t0, nil)}, true},
		{"sealed by the cancel", []domain.ItemInterval{row("si_1", "pln_1", 1, t0, &t1)}, true},
		{"quantity changed and back", []domain.ItemInterval{row("si_1", "pln_1", 1, t0, &t1), row("si_1", "pln_1", 2, t1, &t1), row("si_1", "pln_1", 1, t1, nil)}, false},
		{"item added then removed", []domain.ItemInterval{row("si_1", "pln_1", 1, t0, nil), row("si_2", "pln_2", 1, t1, &t1)}, false},
		{"plan changed", []domain.ItemInterval{row("si_1", "pln_2", 1, t0, nil)}, false},
		{"start moved", []domain.ItemInterval{row("si_1", "pln_1", 1, t1, nil)}, false},
	} {
		if got := sameIntervals(base, c.other); got != c.same {
			t.Errorf("%s: sameIntervals = %v, want %v", c.name, got, c.same)
		}
	}
	// Rows opened after a plan that read none are a change too.
	if sameIntervals(nil, base) {
		t.Error("opened after the plan read none: sameIntervals = true, want false")
	}
}
