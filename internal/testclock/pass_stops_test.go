package testclock

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// jobSpy is every catch-up job. It records each call by the job's name, and
// the job named in fail returns an error for one row.
type jobSpy struct {
	calls []string
	fail  string
}

func (j *jobSpy) run(name string) (int, []error) {
	j.calls = append(j.calls, name)
	if name == j.fail {
		return 0, []error{errors.New("row sub_spy could not be done")}
	}
	return 0, nil
}

func (j *jobSpy) RunCycleForClock(context.Context, string, string, int) (int, []error) {
	return j.run("period closes")
}
func (j *jobSpy) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return j.run("spend thresholds")
}
func (j *jobSpy) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return j.run("collection")
}
func (j *jobSpy) ProcessExpiredTrialsForClock(context.Context, string, string, time.Time) (int, []error) {
	return j.run("trial ends")
}
func (j *jobSpy) ProcessExpiredPauseCollectionsForClock(context.Context, string, string, time.Time) (int, []error) {
	return j.run("pause ends")
}
func (j *jobSpy) RetryPendingTaxForClock(context.Context, string, string, int) (int, []error) {
	return j.run("tax retries")
}
func (j *jobSpy) RetryPendingClawbackIssueForClock(context.Context, string, string, time.Time, int) (int, []error) {
	return j.run("relief credit notes")
}
func (j *jobSpy) ExpireCreditsForClock(context.Context, string, string, time.Time) (int, []error) {
	return j.run("credit expiry")
}
func (j *jobSpy) ProcessDueRunsForClock(context.Context, string, string, time.Time, int) (int, []error) {
	return j.run("payment retries")
}

func spyService(spy *jobSpy) (*Service, *mockStore) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{
		ID: "c1", TenantID: "t1", Status: domain.TestClockStatusAdvancing,
		FrozenTime: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	d := testDeps(spy)
	d.Trials, d.Pauses, d.Tax, d.Relief, d.Credits, d.Dunning = spy, spy, spy, spy, spy, spy
	return NewService(store, d), store
}

// TestPass_StopsAtTheFirstFailedJob: a job runs all its rows, and the pass
// stops at the first job that returned an error, so no later job acts on
// state the failed one did not reach. A failed relief credit note stops the
// collection that would charge the full amount. The reason names the job and
// the row.
func TestPass_StopsAtTheFirstFailedJob(t *testing.T) {
	for _, failing := range []string{"trial ends", "period closes", "relief credit notes", "payment retries"} {
		t.Run(failing, func(t *testing.T) {
			spy := &jobSpy{fail: failing}
			s, store := spyService(spy)
			if err := runPass(context.Background(), s, "t1", "c1"); err == nil {
				t.Fatal("expected the pass to return the job's error")
			}
			if last := spy.calls[len(spy.calls)-1]; last != failing {
				t.Errorf("jobs run = %v, want the pass to stop after %q", spy.calls, failing)
			}
			if failing == "relief credit notes" && slices.Contains(spy.calls, "collection") {
				t.Errorf("collection ran after the relief credit notes failed: %v", spy.calls)
			}
			got := store.clocks["c1"]
			if got.Status != domain.TestClockStatusInternalFailed {
				t.Fatalf("status %q, want internal_failure", got.Status)
			}
			if !strings.HasPrefix(got.LastFailureReason, failing+": ") || !strings.Contains(got.LastFailureReason, "sub_spy") {
				t.Errorf("reason %q, want it to name the job (%q) and the row", got.LastFailureReason, failing)
			}
		})
	}
}

// TestPass_RetryAfterAFailedJobRunsEveryJob: Retry advance runs the pass again
// from the top, and once the job succeeds every job runs and the clock is
// ready.
func TestPass_RetryAfterAFailedJobRunsEveryJob(t *testing.T) {
	spy := &jobSpy{fail: "tax retries"}
	s, store := spyService(spy)
	_ = runPass(context.Background(), s, "t1", "c1")
	if _, err := s.RetryAdvance(context.Background(), "t1", "c1"); err != nil {
		t.Fatalf("retry advance: %v", err)
	}
	spy.fail, spy.calls = "", nil
	if err := runPass(context.Background(), s, "t1", "c1"); err != nil {
		t.Fatalf("pass after retry: %v", err)
	}
	want := []string{"trial ends", "period closes", "pause ends", "spend thresholds", "tax retries",
		"relief credit notes", "collection", "relief credit notes", "payment retries", "credit expiry"}
	if !slices.Equal(spy.calls, want) {
		t.Errorf("jobs run after the retry = %v, want %v", spy.calls, want)
	}
	if got := store.clocks["c1"]; got.Status != domain.TestClockStatusReady {
		t.Errorf("status %q, want ready", got.Status)
	}
}
