package dunning

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// The clock's dunning job (ProcessDueRunsForClock) works in rounds: each walks
// every due run and takes each one step, and the job ends when nothing is due.
// Work it leaves undone fails the advance; a real-time tick logs the same work
// and goes on.

// noAnswerCounter answers every retry with no definite answer.
type noAnswerCounter struct{ calls int }

func (r *noAnswerCounter) RetryPayment(context.Context, string, string, string) error {
	r.calls++
	return ErrTransientSkip
}

// dueRuns starts n runs, each due an hour ago.
func dueRuns(t *testing.T, store *memStore, svc *Service, n int) []string {
	t.Helper()
	var ids []string
	past := time.Now().UTC().Add(-time.Hour)
	for i := range n {
		run, err := svc.StartDunning(context.Background(), "t1", fmt.Sprintf("inv_%d", i), "cus_1", time.Now(), domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("start dunning: %v", err)
		}
		run.NextActionAt = &past
		store.runs[run.ID] = run
		ids = append(ids, run.ID)
	}
	return ids
}

// A retry with no definite answer is not counted (the attempt is rewound). On
// a clock it is work left for the next run: the job reaches every due run,
// pages past the first, and returns one such error for each. In real time the
// tick logs it and returns nothing.
func TestProcessDueRuns_NoAnswerIsLeftForTheNextRun(t *testing.T) {
	t.Run("on a clock", func(t *testing.T) {
		store := newMemStore()
		retrier := &noAnswerCounter{}
		svc := NewService(store, retrier, nil)
		ids := dueRuns(t, store, svc, 5)

		_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 2)
		if retrier.calls != 5 {
			t.Errorf("retries = %d, want 5: one for each due run, across pages of 2", retrier.calls)
		}
		if len(errs) != 5 {
			t.Fatalf("errors = %v, want 5", errs)
		}
		for _, err := range errs {
			if !errors.Is(err, domain.ErrLeftForNextRun) || !errors.Is(err, ErrTransientSkip) {
				t.Errorf("error %v: want ErrLeftForNextRun, wrapping ErrTransientSkip", err)
			}
		}
		for _, id := range ids {
			if got := store.runs[id].AttemptCount; got != 0 {
				t.Errorf("run %s attempt count = %d, want 0: a retry with no answer is not counted", id, got)
			}
		}
	})

	t.Run("in real time", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, &noAnswerCounter{}, nil)
		ids := dueRuns(t, store, svc, 2)

		if _, errs := svc.ProcessDueRuns(context.Background(), "t1", 20); len(errs) != 0 {
			t.Fatalf("errors = %v, want none: the tick logs it", errs)
		}
		for _, id := range ids {
			if got := store.runs[id].AttemptCount; got != 0 {
				t.Errorf("run %s attempt count = %d, want 0", id, got)
			}
		}
	})
}

// claimAlwaysLost is a store whose exhaustion claim always goes to another
// processor, so an exhausted run's step leaves it as it was.
type claimAlwaysLost struct{ *memStore }

func (claimAlwaysLost) ClaimExhaustion(context.Context, string, string, int, time.Time, time.Time) (bool, error) {
	return false, nil
}

// A round that finds only runs its last step left as they were makes no
// progress. On a clock that is an error, not a quiet exit with the run still
// due.
func TestProcessDueRunsForClock_NoProgressIsAnError(t *testing.T) {
	mem := newMemStore()
	svc := NewService(claimAlwaysLost{mem}, &noopRetrier{}, nil)
	run := exhaustedRun(t, mem, svc)

	_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20)
	if len(errs) != 1 || !errors.Is(errs[0], domain.ErrLeftForNextRun) {
		t.Fatalf("errors = %v, want one ErrLeftForNextRun: the run is still due", errs)
	}
	if got := mem.runs[run.ID]; got.State != domain.DunningActive || got.AttemptCount != run.AttemptCount {
		t.Errorf("run = %s at attempt %d, want it as it was", got.State, got.AttemptCount)
	}
}

// One round takes a run one step, so a run with more steps due than the job
// has rounds is still due when the rounds run out. That is an error. The
// policy validator allows at most 15 retries; the store here takes 60.
func TestProcessDueRunsForClock_StillDueAfterTheLastRoundIsAnError(t *testing.T) {
	store := newMemStore()
	p := store.policies[store.defaultID]
	p.RetryDays = make([]int, 60)
	for i := range p.RetryDays {
		p.RetryDays[i] = 1
	}
	store.policies[store.defaultID] = p
	svc := NewService(store, &failingRetrier{}, nil)
	ids := dueRuns(t, store, svc, 1)

	_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(100*24*time.Hour), 20)
	if len(errs) != 1 || !errors.Is(errs[0], domain.ErrLeftForNextRun) {
		t.Fatalf("errors = %v, want one ErrLeftForNextRun: the run is still due", errs)
	}
	if got := store.runs[ids[0]].AttemptCount; got != maxDunningCatchupIters {
		t.Errorf("attempt count = %d, want %d: one retry a round", got, maxDunningCatchupIters)
	}
}

// graceThenAct waits for the invoice grace on its first call and acts on the
// later ones.
type graceThenAct struct {
	until time.Time
	calls int
}

func (a *graceThenAct) act() error {
	a.calls++
	if a.calls == 1 {
		return &SubscriptionActionNotYet{Until: a.until}
	}
	return nil
}

func (a *graceThenAct) Cancel(context.Context, string, string) error { return a.act() }

// A final action that waits for the invoice grace moves the run's
// next_action_at and keeps its attempt count. When the grace ends inside the
// advance, the same advance runs the final action: a moved next_action_at is
// progress.
func TestProcessDueRunsForClock_GraceEndingInTheAdvanceRunsTheFinalAction(t *testing.T) {
	h := newGraceWaitHarness(domain.SubActionCancel)
	act := &graceThenAct{until: h.action.until}
	h.svc.SetSubscriptionCanceler(act)
	run := exhaustedRun(t, h.store, h.svc)

	if _, errs := h.svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", act.until.Add(time.Hour), 20); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if act.calls != 2 {
		t.Errorf("final action calls = %d, want 2: the wait, then the action once the grace ended", act.calls)
	}
	if got := h.store.runs[run.ID].State; got != domain.DunningEscalated {
		t.Errorf("run state = %s, want escalated", got)
	}
}
