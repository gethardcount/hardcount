package subscription

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A row the job leaves as it was stays in the list. The job's cursor moves
// past it, so each row is tried once and the job ends; without the cursor it
// would read the same page again and never end.
func TestProcessExpiredPauseCollectionsForClock_TriesEachRowOnce(t *testing.T) {
	m := newMemStore()
	m.clearPauseOverride = errors.New("resume refused")
	frozen := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	resumes := frozen.Add(-time.Hour)
	next := frozen.Add(30 * 24 * time.Hour)
	const n = clockJobPage + clockJobPage/2
	for i := range n {
		id := fmt.Sprintf("sub_%04d", i)
		m.subs[id] = domain.Subscription{
			ID: id, TenantID: "t1", TestClockID: "clk", Status: domain.SubscriptionActive, NextBillingAt: &next,
			PauseCollection: &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumes},
		}
	}
	svc := NewService(m, clock.Real())
	done := make(chan []error, 1)
	go func() {
		_, errs := svc.ProcessExpiredPauseCollectionsForClock(context.Background(), "t1", "clk", frozen)
		done <- errs
	}()
	select {
	case errs := <-done:
		if len(errs) != n {
			t.Errorf("errors = %d, want %d: one per row, each row tried once", len(errs), n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not end: it read the same rows again")
	}
}
