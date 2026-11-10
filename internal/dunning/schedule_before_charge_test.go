package dunning

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// ctxStore is the in-memory store with an UpdateRunIfActive that behaves like
// the database: it fails on a cancelled context, and failOn makes the n-th
// call fail as a dropped connection does.
type ctxStore struct {
	*memStore
	calls, failOn int
}

func (c *ctxStore) UpdateRunIfActive(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expectedAttempts int, then func(tx *sql.Tx) error) (bool, error) {
	c.calls++
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if c.calls == c.failOn {
		return false, errors.New("write: connection reset by peer")
	}
	return c.memStore.UpdateRunIfActive(ctx, tenantID, run, expectedAttempts, then)
}

// The retry's schedule is recorded with its attempt, before the charge. Here
// the charge is declined and the write that follows it fails (a deploy, a lost
// lease, a database blip land in the same place). The run must then wait the
// policy's interval. Recorded only after the charge, the run stayed due with
// the attempt counted, so the next tick, an hour later, charged the card again
// and the customer reached the final action days early.
func TestProcessRun_DeclineThenWriteFails_RunWaitsThePolicysInterval(t *testing.T) {
	mem := newMemStore()
	store := &ctxStore{memStore: mem, failOn: 2} // the pre-charge write lands; the post-charge one fails
	charges := 0
	svc := NewService(store, funcRetrier{fn: func() error {
		charges++
		return errors.New("payment declined")
	}}, nil)
	run := dueRunAt(t, mem, svc, 0)
	policy := mem.policies[mem.defaultID]
	wait, ok := policy.WaitBeforeRetry(2)
	if !ok || wait < time.Hour {
		t.Fatalf("setup: the default policy's wait before retry 2 is %v (ok=%v), want days", wait, ok)
	}

	if _, errs := svc.ProcessDueRuns(context.Background(), "t1", 20); len(errs) == 0 {
		t.Fatal("setup: want the failed post-charge write surfaced as an error")
	}
	got := mem.runs[run.ID]
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1: the card was charged", got.AttemptCount)
	}
	if got.NextActionAt == nil || got.NextActionAt.Before(time.Now().Add(wait-time.Hour)) {
		t.Fatalf("next_action_at = %v, want about %v from now: with the attempt counted and the run still due, the next tick charges the card again",
			got.NextActionAt, wait)
	}

	// The next tick, an hour later, must leave the card alone.
	svc.ProcessDueRuns(context.Background(), "t1", 20)
	if charges != 1 {
		t.Fatalf("the card was charged %d times across two ticks, want 1: the policy's wait before retry 2 is %v", charges, wait)
	}
}

// resolveFailStore fails the write that resolves a run, once.
type resolveFailStore struct {
	*memStore
	failures int
}

func (r *resolveFailStore) ResolveRun(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, fromActivity bool) (bool, error) {
	if r.failures > 0 {
		r.failures--
		return false, errors.New("write: connection reset by peer")
	}
	return r.memStore.ResolveRun(ctx, tenantID, run, fromActivity)
}

// A retry that paid the invoice, whose resolve then fails, leaves the run due:
// the next tick's paid pre-check ends it. With the wait before the next retry
// already recorded before the charge, the run would otherwise stay active for
// days on a paid invoice, showing a retry that will never be needed.
func TestProcessRun_RetryPaidButResolveFails_RunIsDueAgain(t *testing.T) {
	mem := newMemStore()
	store := &resolveFailStore{memStore: mem, failures: 1}
	svc := NewService(store, &noopRetrier{}, nil)
	paid := false
	svc.SetSubscriptionPauser(&recordingPauser{}, invGetFunc(func() domain.Invoice {
		if paid {
			return paidInv()
		}
		inv := paidInv()
		inv.Status, inv.PaymentStatus = domain.InvoiceFinalized, domain.PaymentFailed
		return inv
	}))
	run := dueRunAt(t, mem, svc, 0)

	if _, errs := svc.ProcessDueRuns(context.Background(), "t1", 20); len(errs) == 0 {
		t.Fatal("setup: want the failed resolve surfaced as an error")
	}
	paid = true // the retry collected the invoice
	got := mem.runs[run.ID]
	if got.State != domain.DunningActive {
		t.Fatalf("setup: state = %q, want active (the resolve failed)", got.State)
	}
	if got.NextActionAt == nil || got.NextActionAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("next_action_at = %v, want due now: the run must not wait days on a paid invoice", got.NextActionAt)
	}

	svc.ProcessDueRuns(context.Background(), "t1", 20)
	if got := mem.runs[run.ID]; got.State != domain.DunningResolved || got.Resolution != domain.ResolutionPaymentRecovered {
		t.Fatalf("after the next tick: state=%q resolution=%q, want resolved as payment_recovered", got.State, got.Resolution)
	}
}

// invGetFunc is an invoice reader whose answer the test changes between ticks.
type invGetFunc func() domain.Invoice

func (f invGetFunc) Get(context.Context, string, string) (domain.Invoice, error) { return f(), nil }

// A retry that got no definite answer because the tick itself was stopping
// (a deploy, a lost lease) arrives with its context cancelled. The rewind of
// the attempt recorded before the charge must still land, or every shutdown
// during a retry burns one of the customer's attempts.
func TestProcessRun_TransientSkipOnACancelledTick_StillRewinds(t *testing.T) {
	mem := newMemStore()
	store := &ctxStore{memStore: mem}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := NewService(store, funcRetrier{fn: func() error {
		cancel() // the tick is stopping while Stripe is being asked
		return ErrTransientSkip
	}}, nil)
	run := dueRunAt(t, mem, svc, 1)
	before := mem.runs[run.ID]

	svc.ProcessDueRuns(ctx, "t1", 20)

	got := mem.runs[run.ID]
	if got.AttemptCount != before.AttemptCount {
		t.Fatalf("attempt_count = %d, want %d: a shutdown during the retry must not burn an attempt", got.AttemptCount, before.AttemptCount)
	}
	if got.NextActionAt == nil || !got.NextActionAt.Equal(*before.NextActionAt) {
		t.Fatalf("next_action_at = %v, want it restored to %v so the next tick retries", got.NextActionAt, before.NextActionAt)
	}
}
