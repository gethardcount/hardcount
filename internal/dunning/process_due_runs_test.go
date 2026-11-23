package dunning

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

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

// A retry with no definite answer is not counted (the attempt is rewound),
// and the tick logs it and returns nothing.
func TestProcessDueRuns_NoAnswerIsLeftForTheNextRun(t *testing.T) {
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
}

// A final action that fails keeps the run open (action_failed, due a day
// later), and the tick logs it and returns nothing.
func TestProcessDueRuns_AFailedFinalActionIsNotAnError(t *testing.T) {
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
}
