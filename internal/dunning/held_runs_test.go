package dunning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// countingRetrier makes every retry and counts them; onRetry, when set, runs
// on each.
type countingRetrier struct {
	calls   int
	onRetry func()
}

func (r *countingRetrier) RetryPayment(context.Context, string, string, string) error {
	r.calls++
	if r.onRetry != nil {
		r.onRetry()
	}
	return nil
}

const heldSentence = ": a payment retry is waiting for an earlier payment of this invoice to finish: it cannot be charged twice. " +
	"Hardcount checks it on its next billing run (hourly by default). Retry advance once it is done; to stop waiting, void or write off the invoice."

// heldSays returns what each left-for-next-pass error tells the operator.
func heldSays(t *testing.T, errs []error) []string {
	t.Helper()
	var says []string
	for _, err := range errs {
		var left *leftForNextPass
		if !errors.As(err, &left) || !errors.Is(err, domain.ErrLeftForNextRun) {
			t.Fatalf("error %v is not left for the next pass", err)
		}
		says = append(says, left.OperatorSafeMessage())
	}
	return says
}

// A run held by an earlier payment of its invoice is not stepped, and when the
// round has nothing to step the job fails once for each such run, so the
// advance does not end ready with that retry not made. A run whose customer
// may be paying on the hosted page is neither stepped nor reported.
func TestProcessDueRunsForClock_ReportsEachRunAChargeAttemptHolds(t *testing.T) {
	store := newMemStore()
	retrier := &countingRetrier{}
	svc := NewService(store, retrier, nil)
	ids := dueRuns(t, store, svc, 3)
	store.heldBy = map[string]domain.ChargeClaimState{
		ids[0]: domain.ChargeHeldByAttempt,
		ids[1]: domain.ChargePayingOnHostedPage,
		ids[2]: domain.ChargeHeldByAttempt,
	}
	_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20)
	if retrier.calls != 0 {
		t.Errorf("%d retries made; want none: every run is held", retrier.calls)
	}
	says := heldSays(t, errs)
	want := []string{"Invoice " + store.runs[ids[0]].InvoiceID + heldSentence, "Invoice " + store.runs[ids[2]].InvoiceID + heldSentence}
	if len(says) != 2 || says[0] != want[0] || says[1] != want[1] {
		t.Errorf("the operator reads %q; want %q", says, want)
	}
}

// After a round that stepped a run, a held run is still reported when the
// next round has nothing to step.
func TestProcessDueRunsForClock_ReportsAHeldRunAfterARoundThatDidWork(t *testing.T) {
	store := newMemStore()
	retrier := &countingRetrier{}
	svc := NewService(store, retrier, nil)
	ids := dueRuns(t, store, svc, 2)
	store.heldBy = map[string]domain.ChargeClaimState{ids[1]: domain.ChargeHeldByAttempt}
	n, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20)
	if retrier.calls != 1 || n != 1 {
		t.Errorf("%d retries made, %d processed; want the free run stepped once", retrier.calls, n)
	}
	says := heldSays(t, errs)
	if want := "Invoice " + store.runs[ids[1]].InvoiceID + heldSentence; len(says) != 1 || says[0] != want {
		t.Errorf("the operator reads %q; want %q", says, want)
	}
}

// A round with nothing to step returns both what did not move and what a
// charge attempt holds.
func TestProcessDueRunsForClock_ReturnsUnmovedAndHeldTogether(t *testing.T) {
	mem := newMemStore()
	svc := NewService(claimAlwaysLost{mem}, &noopRetrier{}, nil)
	exhaustedRun(t, mem, svc)
	held, err := svc.StartDunning(context.Background(), "t1", "inv_held", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	held.NextActionAt = &past
	mem.runs[held.ID] = held
	mem.heldBy = map[string]domain.ChargeClaimState{held.ID: domain.ChargeHeldByAttempt}

	_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20)
	says := heldSays(t, errs)
	want := []string{
		"1 payment retries due in this advance did not move: each is as its last step found it. Retry advance tries them again.",
		"Invoice inv_held" + heldSentence,
	}
	if len(says) != 2 || says[0] != want[0] || says[1] != want[1] {
		t.Errorf("the operator reads %q; want %q", says, want)
	}
}

// Only runs whose customer may be paying on the hosted page are due: none is
// stepped, and the job ends with no error.
func TestProcessDueRunsForClock_OnlyHostedPageRunsDueEndsWithNoError(t *testing.T) {
	store := newMemStore()
	retrier := &countingRetrier{}
	svc := NewService(store, retrier, nil)
	ids := dueRuns(t, store, svc, 2)
	store.heldBy = map[string]domain.ChargeClaimState{ids[0]: domain.ChargePayingOnHostedPage, ids[1]: domain.ChargePayingOnHostedPage}
	if _, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20); len(errs) != 0 || retrier.calls != 0 {
		t.Errorf("errors %v, %d retries; want none of either", errs, retrier.calls)
	}
}

// A run whose earlier payment finishes between two rounds is stepped in the
// next round, not reported.
func TestProcessDueRunsForClock_ARunReleasedBetweenRoundsIsStepped(t *testing.T) {
	store := newMemStore()
	retrier := &countingRetrier{}
	svc := NewService(store, retrier, nil)
	ids := dueRuns(t, store, svc, 2)
	store.heldBy = map[string]domain.ChargeClaimState{ids[1]: domain.ChargeHeldByAttempt}
	retrier.onRetry = func() { delete(store.heldBy, ids[1]) } // the free run's retry, in round one
	if _, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 20); len(errs) != 0 || retrier.calls != 2 {
		t.Errorf("errors %v, %d retries; want both runs stepped and no error", errs, retrier.calls)
	}
}

// A held run in the middle of the walk, across page boundaries, is reported,
// and the runs before and after it are stepped.
func TestProcessDueRunsForClock_AHeldRunAcrossPagesIsReported(t *testing.T) {
	store := newMemStore()
	retrier := &countingRetrier{}
	svc := NewService(store, retrier, nil)
	ids := dueRuns(t, store, svc, 3)
	store.heldBy = map[string]domain.ChargeClaimState{ids[1]: domain.ChargeHeldByAttempt}
	_, errs := svc.ProcessDueRunsForClock(context.Background(), "t1", "clk", time.Now().UTC().Add(time.Hour), 1)
	says := heldSays(t, errs)
	if want := "Invoice " + store.runs[ids[1]].InvoiceID + heldSentence; retrier.calls != 2 || len(says) != 1 || says[0] != want {
		t.Errorf("%d retries, the operator reads %q; want 2 retries and %q", retrier.calls, says, want)
	}
}
