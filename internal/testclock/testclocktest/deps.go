package testclocktest

import (
	"context"
	"database/sql"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/testclock"
)

// Deps returns the clock service's dependencies with billing as given, every
// other job and reader doing nothing, an audit writer that writes nothing,
// and the production pass deadline. A test sets the fields it exercises.
func Deps(billing testclock.BillingRunner) testclock.Deps {
	n := noJobs{}
	return testclock.Deps{
		Billing: billing, Customers: n, Subs: n, Trials: n, Pauses: n,
		Tax: n, Relief: n, Credits: n, Dunning: n, Audit: n,
		PassDeadline: testclock.CatchupPassDeadline,
	}
}

// noJobs is every clock job and reader with nothing to do, and an audit
// writer that writes nothing.
type noJobs struct{}

func (noJobs) LogInTx(context.Context, *sql.Tx, audit.Entry) error { return nil }

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
