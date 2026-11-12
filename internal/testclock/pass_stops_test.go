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

// order is the jobs of one pass, as runPhases runs them and as
// docs/concepts/test-clocks.md lists them. Relief credit notes run twice.
var order = []string{
	"trial ends", "billing period closes", "pause ends", "spend thresholds", "tax retries",
	"relief credit notes", "card charges", "relief credit notes", "payment retries", "credit expiry",
}

// jobSpy is every catch-up job. It records each call by the job's name and
// reports one row done per call. The failCall-th call of the job named fail
// also returns an error for one row.
type jobSpy struct {
	calls    []string
	fail     string
	failCall int
}

func (j *jobSpy) run(name string) (int, []error) {
	j.calls = append(j.calls, name)
	if name == j.fail {
		n := 0
		for _, c := range j.calls {
			if c == name {
				n++
			}
		}
		if n == j.failCall {
			return 1, []error{errors.New("row sub_spy could not be done")}
		}
	}
	return 1, nil
}

func (j *jobSpy) ProcessExpiredTrialsForClock(context.Context, string, string, time.Time) (int, []error) {
	return j.run("trial ends")
}
func (j *jobSpy) RunCycleForClock(context.Context, string, string, int) (int, []error) {
	return j.run("billing period closes")
}
func (j *jobSpy) ProcessExpiredPauseCollectionsForClock(context.Context, string, string, time.Time) (int, []error) {
	return j.run("pause ends")
}
func (j *jobSpy) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return j.run("spend thresholds")
}
func (j *jobSpy) RetryPendingTaxForClock(context.Context, string, string, int) (int, []error) {
	return j.run("tax retries")
}
func (j *jobSpy) RetryPendingClawbackIssueForClock(context.Context, string, string, time.Time, int) (int, []error) {
	return j.run("relief credit notes")
}
func (j *jobSpy) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return j.run("card charges")
}
func (j *jobSpy) ProcessDueRunsForClock(context.Context, string, string, time.Time, int) (int, []error) {
	return j.run("payment retries")
}
func (j *jobSpy) ExpireCreditsForClock(context.Context, string, string, time.Time) (int, []error) {
	return j.run("credit expiry")
}

// counted is the summary count each job's rows add to.
func counted(sum *domain.AdvanceSummary, job string) int {
	switch job {
	case "trial ends":
		return sum.TrialsActivated
	case "billing period closes":
		return sum.InvoicesGenerated
	case "pause ends":
		return sum.PausesResumed
	case "spend thresholds":
		return sum.ThresholdsFired
	case "tax retries":
		return sum.TaxRetried
	case "relief credit notes":
		return sum.ClawbacksIssued
	case "card charges":
		return sum.ChargesRetried
	case "payment retries":
		return sum.DunningAdvanced
	case "credit expiry":
		return sum.CreditsExpired
	}
	return -1
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

// TestPass_StopsAtTheFailedJob: the pass stops at the first job that returns
// an error, at each of the ten, so no later job acts on state the failed one
// did not reach. A relief credit note that fails stops the card charge that
// would take the full amount; the second relief run fails on its own. The
// failed job's count is kept in the summary, and the clock's reason names
// the job and the row. With no failure every job runs and the clock is ready.
func TestPass_StopsAtTheFailedJob(t *testing.T) {
	for i, failing := range order {
		nth := 0
		for _, j := range order[:i+1] {
			if j == failing {
				nth++
			}
		}
		t.Run(failing+"/"+string(rune('0'+nth)), func(t *testing.T) {
			spy := &jobSpy{fail: failing, failCall: nth}
			s, store := spyService(spy)
			if err := runPass(context.Background(), s, "t1", "c1"); err == nil {
				t.Fatal("expected the pass to return the job's error")
			}
			if want := order[:i+1]; !slices.Equal(spy.calls, want) {
				t.Errorf("jobs run = %v, want %v", spy.calls, want)
			}
			got := store.clocks["c1"]
			if got.Status != domain.TestClockStatusInternalFailed {
				t.Fatalf("status %q, want internal_failure", got.Status)
			}
			if !strings.HasPrefix(got.LastFailureReason, failing+": ") || !strings.Contains(got.LastFailureReason, "sub_spy") {
				t.Errorf("reason %q, want it to name the job (%q) and the row", got.LastFailureReason, failing)
			}
			if sum := got.LastAdvanceSummary; sum == nil || counted(sum, failing) != nth || !sum.HadErrors {
				t.Errorf("summary %+v: want %q counted %d and had_errors", sum, failing, nth)
			}
		})
	}

	t.Run("no failure", func(t *testing.T) {
		spy := &jobSpy{}
		s, store := spyService(spy)
		if err := runPass(context.Background(), s, "t1", "c1"); err != nil {
			t.Fatalf("pass: %v", err)
		}
		if !slices.Equal(spy.calls, order) {
			t.Errorf("jobs run = %v, want %v", spy.calls, order)
		}
		if got := store.clocks["c1"]; got.Status != domain.TestClockStatusReady {
			t.Errorf("status %q, want ready", got.Status)
		}
	})
}
