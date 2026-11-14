package dunning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
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
		var safe interface{ OperatorSafeMessage() string }
		if !errors.As(errs[0], &safe) {
			t.Fatalf("error %v carries no operator message", errs[0])
		}
		const said = ": a payment retry got no definite answer from Stripe, so it is not counted and is tried again (on a test clock, by Retry advance)."
		if got := safe.OperatorSafeMessage(); !strings.HasPrefix(got, "Invoice inv_") || !strings.HasSuffix(got, said) {
			t.Errorf("the operator reads %q, want \"Invoice inv_<n>%s\"", got, said)
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

	_, runErrs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20)
	if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
		t.Fatalf("errors = %v, want one ErrLeftForNextRun: the run is still due", runErrs)
	}
	if got, want := errs.SummarizeForOperator(runErrs, ""), "1 payment retries due in this advance did not move: each is as its last step found it. Retry advance tries them again."; got != want {
		t.Errorf("the operator reads %q, want %q", got, want)
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

	_, runErrs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(100*24*time.Hour), 20)
	if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
		t.Fatalf("errors = %v, want one ErrLeftForNextRun: the run is still due", runErrs)
	}
	if got, want := errs.SummarizeForOperator(runErrs, ""), "1 payment retries are still due after 50 rounds of this advance. Retry advance goes on with them."; got != want {
		t.Errorf("the operator reads %q, want %q", got, want)
	}
	if got := store.runs[ids[0]].AttemptCount; got != maxDunningCatchupRounds {
		t.Errorf("attempt count = %d, want %d: one retry a round", got, maxDunningCatchupRounds)
	}
}

// claimLeftByAnotherWorker is a store whose first exhaustion claim goes to
// another worker that takes its lease and stops: the run's next_action_at
// moves to that lease, its attempt count stays. Later claims are the store's.
type claimLeftByAnotherWorker struct {
	*memStore
	taken bool
}

func (c *claimLeftByAnotherWorker) ClaimExhaustion(ctx context.Context, tenantID, runID string, attempts int, next, lease time.Time) (bool, error) {
	if !c.taken {
		c.taken = true
		run := c.runs[runID]
		other := next.Add(exhaustLease)
		run.NextActionAt = &other
		c.runs[runID] = run
		return false, nil
	}
	return c.memStore.ClaimExhaustion(ctx, tenantID, runID, attempts, next, lease)
}

// A run whose exhaustion another worker claimed and left moves only its
// next_action_at, to that worker's lease. When the lease ends inside the
// advance, the next round takes the run again and runs its final action: a
// moved next_action_at is progress, with the same attempt count.
func TestProcessDueRunsForClock_ALeaseLeftByAnotherWorkerIsProgress(t *testing.T) {
	mem := newMemStore()
	markUncollectiblePolicy(mem)
	store := &claimLeftByAnotherWorker{memStore: mem}
	svc := NewService(store, &noopRetrier{}, nil)
	mover := &capturingUncollect{}
	svc.SetInvoiceUncollectibleMarker(mover)
	run := exhaustedRun(t, mem, svc)

	if _, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20); len(errs) != 0 {
		t.Fatalf("errors = %v, want none", errs)
	}
	if len(mover.calls) != 1 || mem.runs[run.ID].State != domain.DunningEscalated {
		t.Errorf("final actions = %d, run %s; want 1 and escalated", len(mover.calls), mem.runs[run.ID].State)
	}
}

// A final action that fails keeps the run open (action_failed, due a day
// later). On a clock the job returns it, once, with one attempt at the
// action, so the advance fails and Retry advance tries it again; it does not
// try the action once per simulated day inside the advance. In real time the
// tick logs it and returns nothing.
func TestProcessDueRunsForClock_AFailedFinalActionFailsTheAdvance(t *testing.T) {
	t.Run("on a clock", func(t *testing.T) {
		store := newMemStore()
		markUncollectiblePolicy(store)
		svc := NewService(store, &noopRetrier{}, nil)
		mover := &capturingUncollect{err: errors.New("stripe blip")}
		svc.SetInvoiceUncollectibleMarker(mover)
		run := exhaustedRun(t, store, svc)

		_, runErrs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(5*24*time.Hour), 20)
		if len(runErrs) != 1 || !errors.Is(runErrs[0], domain.ErrLeftForNextRun) {
			t.Fatalf("errors = %v, want one ErrLeftForNextRun", runErrs)
		}
		const want = "Invoice inv_1: payment recovery's final action did not finish (its invoice could not be marked uncollectible), so its dunning run stays open. Retry advance tries the action again."
		if got := errs.SummarizeForOperator(runErrs, ""); got != want {
			t.Errorf("the operator reads:\n  %q\nwant\n  %q", got, want)
		}
		if len(mover.calls) != 1 {
			t.Errorf("final action attempts = %d, want 1", len(mover.calls))
		}
		if got := store.runs[run.ID]; got.State != domain.DunningActive || got.Resolution != domain.ResolutionActionFailed {
			t.Errorf("run = %s / %s, want active / action_failed", got.State, got.Resolution)
		}
	})
	t.Run("in real time", func(t *testing.T) {
		store := newMemStore()
		markUncollectiblePolicy(store)
		svc := NewService(store, &noopRetrier{}, nil)
		svc.SetInvoiceUncollectibleMarker(&capturingUncollect{err: errors.New("stripe blip")})
		exhaustedRun(t, store, svc)
		// processed counts a step that returned nil, as today: the tick
		// retries the action a day later and logs no error.
		if n, runErrs := svc.ProcessDueRuns(context.Background(), "t1", 20); n != 1 || len(runErrs) != 0 {
			t.Fatalf("processed %d, errors %v; want 1 and none: the tick retries it a day later", n, runErrs)
		}
	})
}
