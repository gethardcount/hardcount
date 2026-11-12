package testclock

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// testDeps is testclocktest.Deps for this package's own tests, which cannot
// import testclocktest: billing as given, every other job doing nothing.
func testDeps(billing BillingRunner) Deps {
	n := noJobs{}
	return Deps{
		Billing: billing, Customers: n, Subs: n, Trials: n, Pauses: n,
		Tax: n, Relief: n, Credits: n, Dunning: n,
		PassDeadline: CatchupPassDeadline,
	}
}

// noJobs is every clock job and reader with nothing to do.
type noJobs struct{}

func (noJobs) RunCycleForClock(context.Context, string, string, int) (int, []error) { return 0, nil }
func (noJobs) RetryPendingChargesForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (noJobs) ScanThresholdsForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (noJobs) ListByTestClockID(context.Context, string, string, int, int) ([]domain.Customer, int, error) {
	return nil, 0, nil
}
func (noJobs) ListByTestClock(context.Context, string, string, int, int) ([]domain.Subscription, int, error) {
	return nil, 0, nil
}
func (noJobs) ProcessExpiredTrialsForClock(context.Context, string, string, time.Time) (int, []error) {
	return 0, nil
}
func (noJobs) ProcessExpiredPauseCollectionsForClock(context.Context, string, string, time.Time) (int, []error) {
	return 0, nil
}
func (noJobs) RetryPendingTaxForClock(context.Context, string, string, int) (int, []error) {
	return 0, nil
}
func (noJobs) RetryPendingClawbackIssueForClock(context.Context, string, string, time.Time, int) (int, []error) {
	return 0, nil
}
func (noJobs) ExpireCreditsForClock(context.Context, string, string, time.Time) (int, []error) {
	return 0, nil
}
func (noJobs) ProcessDueRunsForClock(context.Context, string, string, time.Time, int) (int, []error) {
	return 0, nil
}

// TestNewService_RefusesAMissingDependency: each missing dependency stops the
// constructor with its name, so a wiring mistake stops the server at boot
// instead of skipping a job on every advance.
func TestNewService_RefusesAMissingDependency(t *testing.T) {
	cases := map[string]func(*Deps){
		"Deps.Billing":      func(d *Deps) { d.Billing = nil },
		"Deps.Customers":    func(d *Deps) { d.Customers = nil },
		"Deps.Subs":         func(d *Deps) { d.Subs = nil },
		"Deps.Trials":       func(d *Deps) { d.Trials = nil },
		"Deps.Pauses":       func(d *Deps) { d.Pauses = nil },
		"Deps.Tax":          func(d *Deps) { d.Tax = nil },
		"Deps.Relief":       func(d *Deps) { d.Relief = nil },
		"Deps.Credits":      func(d *Deps) { d.Credits = nil },
		"Deps.Dunning":      func(d *Deps) { d.Dunning = nil },
		"Deps.PassDeadline": func(d *Deps) { d.PassDeadline = 0 },
	}
	for name, unset := range cases {
		t.Run(name, func(t *testing.T) {
			d := testDeps(noJobs{})
			unset(&d)
			defer func() {
				r := recover()
				if msg, _ := r.(string); !strings.Contains(msg, name) {
					t.Errorf("panic = %v, want one naming %s", r, name)
				}
			}()
			NewService(newMockStore(), d)
		})
	}
	if s := NewService(newMockStore(), testDeps(noJobs{})); s == nil {
		t.Fatal("NewService with every dependency returned nil")
	}
}

// runPass runs one catch-up pass on one clock, as the tick does for each
// clock it lists. On the real store ctx must hold the testclock_catchup lease.
func runPass(ctx context.Context, s *Service, tenantID, clockID string) error {
	clk, err := s.store.Get(ctx, tenantID, clockID)
	if err != nil {
		return err
	}
	return s.runClock(ctx, clk)
}
