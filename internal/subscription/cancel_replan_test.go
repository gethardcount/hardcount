package subscription

import (
	"context"
	"errors"
	"sync"
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

// A trial that the activation scan converts while the cancel is priced: the
// row the first plan priced is trialing and never activated, so it bills
// nothing (a trial is free, ADR-069). The conversion changes only the status
// and activated_at, and the compare-and-set's status no longer matches, so
// the cancel plans again from the active row. The memory store keeps no
// billing intervals, so here the status is the only proof. The real-Postgres
// case is "trial activated while planned" in
// internal/billing/cancel_plans_before_tx_integration_test.go.
//
// Mutation (drop the status from the memory store's snapshotMatches, the
// mirror of `AND status = $3`) → one plan, from the trialing row.
func TestCancel_TrialActivatedWhilePriced_PlansAgainFromTheActiveRow(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	s := store.subs[id]
	trialEnd := *s.CurrentBillingPeriodStart
	s.Status, s.TrialEndAt, s.ActivatedAt = domain.SubscriptionTrialing, &trialEnd, nil
	store.subs[id] = s
	fb.onPlan = func() {
		fb.onPlan = nil
		if _, err := store.ActivateAfterTrialWithBill(context.Background(), "t1", id, trialEnd, nil); err != nil {
			t.Errorf("activate the trial while the cancel was priced: %v", err)
		}
	}

	canceled, _, err := svc.Cancel(context.Background(), "t1", id)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled || canceled.ActivatedAt == nil {
		t.Fatalf("status=%s activated_at=%v, want canceled after the activation", canceled.Status, canceled.ActivatedAt)
	}
	if len(fb.plannedFrom) != 2 || fb.draftTxCalls != 1 {
		t.Fatalf("plans=%d writes=%d, want 2 plans and 1 write", len(fb.plannedFrom), fb.draftTxCalls)
	}
	if first, second := fb.plannedFrom[0], fb.plannedFrom[1]; first.Status != domain.SubscriptionTrialing ||
		second.Status != domain.SubscriptionActive || second.ActivatedAt == nil {
		t.Errorf("planned from %s then %s (activated_at %v), want trialing, then active and activated", first.Status, second.Status, second.ActivatedAt)
	}
}

// steppingResolver resolves a wall-clock subscription as the engine does
// (Engine.SimForSubscription): on a context with no bound instant, to the
// clock's now, here a new instant on each call, as a later moment; on a bound
// context, to the bound instant.
type steppingResolver struct {
	mu   sync.Mutex
	base time.Time
	n    int
}

func (r *steppingResolver) sim(ctx context.Context) clock.Sim {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return clock.Sim{At: at}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return clock.Sim{At: r.base.Add(time.Duration(r.n) * time.Second)}
}

func (r *steppingResolver) SimForSubscription(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx), nil
}

func (r *steppingResolver) SimForCustomer(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx), nil
}

func (r *steppingResolver) SimForInvoice(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx), nil
}

// Each attempt binds its instant from the caller's context. On the API's
// unbound context the second plan is at a new instant; binding again on the
// first attempt's context would give the first instant back.
//
// Mutation M7 (bind once, outside the attempts) → both plans at one instant.
func TestCancel_EachAttemptBindsFromTheCallersContext(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	svc.SetResolver(&steppingResolver{base: time.Now().UTC().Add(time.Hour)})
	fb.writeMoved = 1

	if _, _, err := svc.Cancel(context.Background(), "t1", id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(fb.planAts) != 2 || !fb.planAts[1].After(fb.planAts[0]) {
		t.Errorf("plan instants = %v, want two, the second later: each attempt binds anew", fb.planAts)
	}
}

// An active row whose period starts after the cancel instant is not planned:
// priced from it, the final invoice would bill nothing and the relief would
// skip the new period's prebill, while the compare-and-set passes. Both
// attempts refuse it (the instant is bound, as dunning's is), and the cancel
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
	// A trialing subscription has no interval; its conversion opens one per
	// item. A plan that read the rows before the conversion sees it here.
	if sameIntervals(nil, base) {
		t.Error("opened by a trial's conversion: sameIntervals = true, want false")
	}
}
