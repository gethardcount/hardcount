package dunning_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A panic in a dunning retry (the real trigger: a nil dereference on one
// unusual row, as in flexprice #1615) is not a card decline. The attempt is
// recorded before the charge, so a panic used to skip both the rewind and the
// reschedule: the run stayed due, every rerun spent one attempt (and, after a
// decline had released the hold, charged the card again), and within a few
// reruns exhaust-on-entry fired the final action (cancel, pause or write-off)
// on a customer whose card was never declined that many times. It also ended
// the tick for every run after it.
//
// Now a panic goes through the transient-skip rewind, so the run looks as it
// was listed, the other due runs are processed, and the panic is raised again
// at the end of the batch.

// panickingRetry declines every retry except the one for the panicking
// invoice, which panics. before, when set, runs just before that panic.
type panickingRetry struct {
	f      **exhaustFixture
	before func(ctx context.Context, f *exhaustFixture)
	calls  map[string]int
}

func (r *panickingRetry) RetryPayment(ctx context.Context, _, invoiceID, _ string) error {
	r.calls[invoiceID]++
	f := *r.f
	if invoiceID == f.invoiceID {
		if r.before != nil {
			r.before(ctx, f)
		}
		panic("retry: nil payment method on an unusual row")
	}
	return errors.New("card_declined: your card was declined")
}

// addDueRun adds a second failed invoice for the fixture's customer, with an
// active run that has used no attempts and is due at next.
func addDueRun(t *testing.T, f *exhaustFixture, number string, next time.Time) (invoiceID, runID string) {
	t.Helper()
	x, err := f.invoices.Get(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	xRun, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	inv, err := f.invoices.Create(f.ctx, f.tenantID, domain.Invoice{
		CustomerID: x.CustomerID, InvoiceNumber: number,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 1500, TotalAmountCents: 1500, AmountDueCents: 1500,
		BillingPeriodStart: x.BillingPeriodStart, BillingPeriodEnd: x.BillingPeriodEnd, IssuedAt: x.IssuedAt,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	runID = "run_" + inv.ID
	tx, err := f.db.BeginTx(f.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(f.ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("pin livemode GUC: %v", err)
	}
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason,
			attempt_count, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', 0, $6, false, $7, $7, false)`,
		runID, f.tenantID, inv.ID, x.CustomerID, xRun.PolicyID, next, next.Add(-time.Hour)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit run: %v", err)
	}
	return inv.ID, runID
}

// tickPanics runs one dunning tick for the fixture's tenant and returns what
// it panicked with (nil when it did not).
func tickPanics(f *exhaustFixture) (r any) {
	defer func() { r = recover() }()
	f.svc.ProcessDueRuns(f.ctx, f.tenantID, 50)
	return nil
}

// TestProcessDueRuns_RetryPanic_RewindsAndRunsTheRest: run X is due first and
// its retry panics; run Y is due after it. Each tick re-raises the panic, Y is
// attempted and rescheduled on the first, and X stays exactly as it was
// listed on every tick, past the final attempt's exhaustion lease too, so it
// is never exhausted and no final action fires. The policy allows two
// attempts and X has used one, so its next attempt is the final one.
func TestProcessDueRuns_RetryPanic_RewindsAndRunsTheRest(t *testing.T) {
	db := testutil.SetupTestDB(t)
	var f *exhaustFixture
	retry := &panickingRetry{f: &f, calls: map[string]int{}}
	f = newExhaustFixture(t, db, "Retry Panic", policyActions[0], 1, nil, retry)
	yInvoice, yRun := addDueRun(t, f, "INV-PANIC-Y", f.clk.Now(f.ctx).Add(-30*time.Minute))
	listed, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}

	assertAsListed := func(tick string) {
		t.Helper()
		got, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		if got.AttemptCount != listed.AttemptCount || !sameTime(got.LastAttemptAt, listed.LastAttemptAt) || !sameTime(got.NextActionAt, listed.NextActionAt) {
			t.Fatalf("%s: run X attempt_count=%d last_attempt_at=%v next_action_at=%v, want it as listed (%d, %v, %v): a panic is not an attempt",
				tick, got.AttemptCount, got.LastAttemptAt, got.NextActionAt, listed.AttemptCount, listed.LastAttemptAt, listed.NextActionAt)
		}
	}

	// MaxRetryAttempts+1 ticks: without the rewind, X would be out of
	// attempts and exhausted by now.
	for i := 1; i <= 3; i++ {
		if r := tickPanics(f); r != "retry: nil payment method on an unusual row" {
			t.Fatalf("tick %d recovered %v, want the retry's panic raised again", i, r)
		}
		if i == 1 {
			y, err := f.runs.GetRun(f.ctx, f.tenantID, yRun)
			if err != nil {
				t.Fatalf("read run Y: %v", err)
			}
			if retry.calls[yInvoice] != 1 || y.AttemptCount != 1 || y.NextActionAt == nil || !y.NextActionAt.After(f.clk.Now(f.ctx)) {
				t.Fatalf("run Y after the first tick: retries=%d attempt_count=%d next_action_at=%v, want it attempted once and rescheduled after now",
					retry.calls[yInvoice], y.AttemptCount, y.NextActionAt)
			}
		}
		assertAsListed(fmt.Sprintf("tick %d", i))
	}

	// Past the final attempt's exhaustion lease.
	f.clk.Advance(16 * time.Minute)
	if r := tickPanics(f); r == nil {
		t.Fatal("the tick past the lease did not re-raise the retry's panic")
	}
	assertAsListed("past the lease")
	f.assertNothingFired(t)
	if retry.calls[f.invoiceID] != 4 {
		t.Errorf("run X was retried %d times, want once per tick (4)", retry.calls[f.invoiceID])
	}
}

// TestProcessDueRuns_RetryPanic_AfterAResolveKeepsTheRunResolved (collision):
// the payment succeeds and its webhook resolves the run during the retry, then
// the retry panics. The rewind must not reopen the resolved run.
func TestProcessDueRuns_RetryPanic_AfterAResolveKeepsTheRunResolved(t *testing.T) {
	db := testutil.SetupTestDB(t)
	var f *exhaustFixture
	retry := &panickingRetry{f: &f, calls: map[string]int{}, before: func(ctx context.Context, f *exhaustFixture) {
		if err := f.svc.ResolveByInvoice(ctx, f.tenantID, f.invoiceID, domain.ResolutionPaymentRecovered); err != nil {
			t.Errorf("resolve during the retry: %v", err)
		}
	}}
	f = newExhaustFixture(t, db, "Retry Panic Resolved", policyActions[0], 1, nil, retry)

	if r := tickPanics(f); r == nil {
		t.Fatal("the retry's panic was not raised again")
	}
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
		t.Fatalf("run state=%s resolution=%q, want resolved / payment_recovered: the rewind reopened a run the webhook resolved",
			run.State, run.Resolution)
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
