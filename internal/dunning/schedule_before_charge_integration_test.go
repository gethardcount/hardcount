package dunning_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// failNthRunWrite is the real store with its n-th UpdateRunIfActive failing,
// as a write does when its connection drops.
type failNthRunWrite struct {
	dunning.Store
	calls, failOn int
}

func (s *failNthRunWrite) UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, then func(tx *sql.Tx) error) (bool, error) {
	s.calls++
	if s.calls == s.failOn {
		return false, errors.New("write: connection reset by peer")
	}
	return s.Store.UpdateRunIfActive(ctx, tenantID, run, expectedAttempts, then)
}

// countingDecline declines every retry and counts them.
type countingDecline struct{ calls int }

func (r *countingDecline) RetryPayment(context.Context, string, string, string) error {
	r.calls++
	return errors.New("card_declined: your card was declined")
}

// On real Postgres: the card is declined and the write after it fails. The
// row must carry the attempt and the policy's wait, and the next pass, through
// the real due-run query, must not charge the card.
func TestProcessDueRuns_DeclineThenWriteFails_NoSecondChargeOnTheNextPass(t *testing.T) {
	db := testutil.SetupTestDB(t)
	retry := &countingDecline{}
	// The pre-charge write lands; the reschedule after the decline fails.
	wrap := func(s dunning.Store) dunning.Store { return &failNthRunWrite{Store: s, failOn: 2} }
	f := newExhaustFixture(t, db, "Schedule Before Charge", policyActions[1], 0, wrap, retry)
	now := f.clk.Now(f.ctx)

	if _, errs := f.svc.ProcessDueRuns(f.ctx, f.tenantID, 50); len(errs) == 0 {
		t.Fatal("setup: want the failed write after the decline reported")
	}
	if retry.calls != 1 {
		t.Fatalf("setup: the card was charged %d times on the first pass, want 1", retry.calls)
	}
	got, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	wait := 3 * 24 * time.Hour // the fixture's policy waits three days before retry 2
	if got.AttemptCount != 1 || got.NextActionAt == nil || got.NextActionAt.Before(now.Add(wait-time.Minute)) || got.NextActionAt.After(now.Add(wait+time.Minute)) {
		t.Fatalf("row after the first pass: attempt_count=%d next_action_at=%v, want 1 and about %s: left due, the next pass charges the card again",
			got.AttemptCount, got.NextActionAt, now.Add(wait).Format(time.RFC3339))
	}

	// The next pass, an hour later.
	f.clk.Advance(time.Hour)
	f.svc.ProcessDueRuns(f.ctx, f.tenantID, 50)
	if retry.calls != 1 {
		t.Fatalf("the card was charged %d times across two passes, want 1: the policy waits three days", retry.calls)
	}
}

// cancellingSkip stands for a charge the pass stopped waiting for: the tick's
// context is cancelled (a shutdown, a lost leader lease) while Stripe is being
// asked, and the charge reports no definite answer.
type cancellingSkip struct{ cancel context.CancelFunc }

func (r *cancellingSkip) RetryPayment(context.Context, string, string, string) error {
	r.cancel()
	return dunning.ErrTransientSkip
}

// On real Postgres: a retry skipped because the pass itself was stopping. The
// rewind runs after the tick's context is cancelled and must still land
// through the real driver, so the row reads as it was listed: the attempt is
// not used, and the run is due on the next pass.
func TestProcessDueRuns_SkipOnACancelledPass_RewindLands(t *testing.T) {
	db := testutil.SetupTestDB(t)
	retry := &cancellingSkip{}
	f := newExhaustFixture(t, db, "Rewind On Cancelled Pass", policyActions[1], 0, nil, retry)
	listed, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}

	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	retry.cancel = cancel
	f.svc.ProcessDueRuns(ctx, f.tenantID, 50)

	got, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.AttemptCount != listed.AttemptCount || !sameTime(got.NextActionAt, listed.NextActionAt) || !sameTime(got.LastAttemptAt, listed.LastAttemptAt) {
		t.Fatalf("row after the stopped pass: attempt_count=%d next_action_at=%v last_attempt_at=%v, want it as listed (%d, %v, %v): a pass that stopped must not use up an attempt",
			got.AttemptCount, got.NextActionAt, got.LastAttemptAt, listed.AttemptCount, listed.NextActionAt, listed.LastAttemptAt)
	}
}
