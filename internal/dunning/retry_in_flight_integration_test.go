package dunning_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A dunning retry whose payment comes back still processing keeps its run
// active and waiting on that PaymentIntent (#1289), on real Postgres: the
// dunning store, the invoice store and its failure settle, and the email
// outbox. Only the card charge is faked: it stamps the invoice processing as
// the charge path does, and, for the races, commits the payment's outcome
// before the run write (the concurrent-resolver fake, money-path playbook §5
// pattern 3).

// processingRetry is the charge of a retry whose payment Stripe left
// processing: it takes the invoice's charge hold as the adapter's claim does,
// stamps the payment processing, and hands the hold to dunning on the
// in-flight answer, as the adapter does. settle, when set, runs during the
// call, after the processing stamp and before dunning's run write.
type processingRetry struct {
	t      *testing.T
	f      **exhaustFixture
	settle func(t *testing.T, ctx context.Context, f *exhaustFixture, pi string)
	calls  int
}

func (r *processingRetry) RetryPayment(ctx context.Context, _, _, _ string) error {
	r.calls++
	f := *r.f
	pi := fmt.Sprintf("pi_slow_%d", r.calls)
	hold, err := f.invoices.ClaimChargeForDunningRetry(ctx, f.tenantID, f.invoiceID)
	if err != nil || !hold.Held() {
		r.t.Fatalf("claim the retry's hold: held=%v err=%v", hold.Held(), err)
	}
	stamp(domain.PaymentProcessing, pi, "")(r.t, ctx, f)
	if r.settle != nil {
		r.settle(r.t, ctx, f, pi)
	}
	return &dunning.RetryError{PaymentIntentID: pi, Err: dunning.ErrPaymentInFlight, Hold: hold}
}

// newInFlightFixture is the exhaust fixture with a fresh run (no retry used,
// two to come), the email outbox and the hold release wired as router.go
// wires them.
func newInFlightFixture(t *testing.T, db *postgres.DB, name string, retry *processingRetry) *exhaustFixture {
	t.Helper()
	var f *exhaustFixture
	retry.t, retry.f = t, &f
	f = newExhaustFixture(t, db, name, finalActions{name: "none", sub: domain.SubActionNone, inv: domain.InvActionNone}, 0, nil, retry)
	f.svc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(db)))
	f.svc.SetCustomerEmailFetcher(fixedEmailFetcher{email: "slow@retry-in-flight.test", name: "Slow"})
	f.svc.SetRetryHoldReleaser(f.invoices)
	return f
}

func (f *exhaustFixture) retryRows(t *testing.T) []domain.InvoiceDunningEvent {
	t.Helper()
	events, err := f.runs.ListEvents(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var out []domain.InvoiceDunningEvent
	for _, e := range events {
		if e.EventType == domain.DunningEventRetryAttempted {
			out = append(out, e)
		}
	}
	return out
}

func (f *exhaustFixture) eventsOf(t *testing.T, typ domain.DunningEventType) int {
	t.Helper()
	events, err := f.runs.ListEvents(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.EventType == typ {
			n++
		}
	}
	return n
}

func (f *exhaustFixture) getRun(t *testing.T) domain.InvoiceDunningRun {
	t.Helper()
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	return run
}

// setHold sets the invoice's charge hold to a live value, as the retry's own
// claim left it while its payment was processing.
func (f *exhaustFixture) setHold(t *testing.T) {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(f.ctx, `UPDATE invoices SET auto_charge_claimed_until = now() + interval '4 minutes' WHERE id = $1 AND tenant_id = $2`, f.invoiceID, f.tenantID); err != nil {
		t.Fatalf("set hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit hold: %v", err)
	}
}

func (f *exhaustFixture) held(t *testing.T) bool {
	t.Helper()
	state, err := f.invoices.ChargeHoldStateOf(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	return state != domain.ChargeHoldNone
}

// failSettle is the payment's failure as payment.SettleFailed records it: the
// failed stamp through the real store, with dunning's hook on its transaction.
func (f *exhaustFixture) failSettle(t *testing.T, pi, declineCode, cardMessage string) {
	t.Helper()
	inv, err := f.invoices.Get(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	hook := f.svc.RetryPaymentFailed(f.ctx, f.tenantID, inv, pi, declineCode, cardMessage)
	if _, res, err := f.invoices.MarkPaymentFailedReportingTransition(f.ctx, f.tenantID, f.invoiceID,
		domain.PaymentFailureReport{PaymentIntentID: pi, Message: "payment failed", DeclineCode: declineCode}, hook); err != nil || !res.Recorded {
		t.Fatalf("failure settle: recorded=%v err=%v", res.Recorded, err)
	}
}

// TestRetryInFlight_WaitsThenRecordsTheFailure is #1289 on real Postgres: the
// retry comes back processing; the run waits (no resolve, no email); the
// failure settle then records the failed attempt, its one warning email and
// the moved retry date in its own transaction, and frees the retry's hold.
func TestRetryInFlight_WaitsThenRecordsTheFailure(t *testing.T) {
	db := testutil.SetupTestDB(t)
	retry := &processingRetry{}
	f := newInFlightFixture(t, db, "In Flight Waits", retry)
	retryAt := f.clk.Now(f.ctx)

	f.tick(t)
	run := f.getRun(t)
	if run.State != domain.DunningActive || run.AttemptCount != 1 {
		t.Fatalf("run after a processing retry: %s / %d, want active / 1", run.State, run.AttemptCount)
	}
	if want := retryAt.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("next_action_at %v, want %v", run.NextActionAt, want)
	}
	if _, err := f.runs.GetRunAwaitingPayment(f.ctx, f.tenantID, f.invoiceID, "pi_slow_1"); err != nil {
		t.Errorf("the run does not wait on pi_slow_1: %v", err)
	}
	rows := f.retryRows(t)
	if len(rows) != 1 || rows[0].Reason != domain.DunningRetryReasonPaymentProcessing {
		t.Fatalf("retry rows %+v, want one payment_processing row", rows)
	}
	if n := f.eventsOf(t, domain.DunningEventResolved); n != 0 {
		t.Errorf("resolved events %d, want 0", n)
	}
	if n := countEmailRows(t, db, f.ctx, f.tenantID, "dunning_warning"); n != 0 {
		t.Errorf("warning emails %d while the payment is processing, want 0", n)
	}

	// The failure, a day later.
	f.setHold(t)
	f.clk.Advance(24 * time.Hour)
	failedAt := f.clk.Now(f.ctx)
	f.failSettle(t, "pi_slow_1", "insufficient_funds", "Your card has insufficient funds.")

	run = f.getRun(t)
	if run.State != domain.DunningActive || run.AttemptCount != 1 {
		t.Fatalf("run after the failure: %s / %d, want active / 1", run.State, run.AttemptCount)
	}
	if want := failedAt.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("next_action_at %v, want the failure plus the next wait %v", run.NextActionAt, want)
	}
	if _, err := f.runs.GetRunAwaitingPayment(f.ctx, f.tenantID, f.invoiceID, "pi_slow_1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the run still waits on pi_slow_1 (err=%v)", err)
	}
	if n := countEmailRows(t, db, f.ctx, f.tenantID, "dunning_warning"); n != 1 {
		t.Errorf("warning emails %d, want 1", n)
	}
	if f.held(t) {
		t.Error("the retry's charge hold is still set after its payment failed")
	}
	rows = f.retryRows(t)
	if len(rows) != 2 || rows[1].Metadata["payment_intent_id"] != "pi_slow_1" || !rows[1].CreatedAt.Equal(failedAt) {
		t.Fatalf("retry rows %+v, want the processing row and a failed row at %v carrying pi_slow_1", rows, failedAt)
	}

	// The reconciler reports it again: nothing more.
	f.failSettle(t, "pi_slow_1", "insufficient_funds", "Your card has insufficient funds.")
	if n := countEmailRows(t, db, f.ctx, f.tenantID, "dunning_warning"); n != 1 || len(f.retryRows(t)) != 2 {
		t.Errorf("after a second report: %d warnings, %d retry rows; want 1 and 2", n, len(f.retryRows(t)))
	}
}

// TestTakeAwaitedRetryTx_OneWinnerAmongConcurrentReports: the compare-and-set
// at the heart of the hook. Four reports of the failure race on their own
// transactions; one takes the run. A report for another PaymentIntent never
// does.
func TestTakeAwaitedRetryTx_OneWinnerAmongConcurrentReports(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newInFlightFixture(t, db, "In Flight CAS", &processingRetry{})
	f.tick(t)

	take := func(pi string) (bool, error) {
		tx, err := db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
		if err != nil {
			return false, err
		}
		defer postgres.Rollback(tx)
		_, _, won, err := f.runs.TakeAwaitedRetryTx(f.ctx, tx, f.invoiceID, pi)
		if err != nil {
			return false, err
		}
		return won, tx.Commit()
	}

	if won, err := take("pi_other"); err != nil || won {
		t.Fatalf("another PaymentIntent took the run: won=%v err=%v", won, err)
	}

	const reporters = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]bool, reporters)
	errsOut := make([]error, reporters)
	for i := range reporters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errsOut[i] = take("pi_slow_1")
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	for i := range reporters {
		if errsOut[i] != nil {
			t.Fatalf("reporter %d: %v", i, errsOut[i])
		}
		if results[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d reports took the run, want exactly 1", winners)
	}
}

// TestRecordRetryInFlight_GuardsAndClears: the in-flight write needs the
// invoice still waiting on the payment, and the run still at its attempt; and
// every ordinary run write afterwards clears the wait.
func TestRecordRetryInFlight_GuardsAndClears(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newInFlightFixture(t, db, "In Flight Guards", &processingRetry{})
	stamp(domain.PaymentProcessing, "pi_x", "")(t, f.ctx, f)
	run := f.getRun(t)
	next := f.clk.Now(f.ctx).Add(72 * time.Hour)
	run.AttemptCount, run.NextActionAt = 1, &next
	event := domain.InvoiceDunningEvent{RunID: run.ID, InvoiceID: run.InvoiceID, EventType: domain.DunningEventRetryAttempted,
		State: domain.DunningActive, AttemptCount: 1, Reason: domain.DunningRetryReasonPaymentProcessing}

	if res, err := f.runs.RecordRetryInFlight(f.ctx, f.tenantID, run, 0, "pi_other", event); err != nil || res != dunning.RetryInFlightPaymentMoved {
		t.Fatalf("another PaymentIntent: res=%v err=%v, want RetryInFlightPaymentMoved", res, err)
	}
	if res, err := f.runs.RecordRetryInFlight(f.ctx, f.tenantID, run, 5, "pi_x", event); err != nil || res != dunning.RetryInFlightRunMoved {
		t.Fatalf("a stale attempt count: res=%v err=%v, want RetryInFlightRunMoved", res, err)
	}
	if n := len(f.retryRows(t)); n != 0 {
		t.Fatalf("refused writes left %d retry rows, want 0", n)
	}
	if res, err := f.runs.RecordRetryInFlight(f.ctx, f.tenantID, run, 0, "pi_x", event); err != nil || res != dunning.RetryInFlightRecorded {
		t.Fatalf("record: res=%v err=%v, want RetryInFlightRecorded", res, err)
	}
	if n := len(f.retryRows(t)); n != 1 {
		t.Errorf("retry rows %d, want 1, written with the run", n)
	}
	if _, err := f.runs.GetRunAwaitingPayment(f.ctx, f.tenantID, f.invoiceID, "pi_x"); err != nil {
		t.Fatalf("the run does not wait on pi_x: %v", err)
	}

	// The next attempt's write clears the wait.
	run.AttemptCount = 2
	if applied, err := f.runs.UpdateRunIfActive(f.ctx, f.tenantID, run, 1, nil); err != nil || !applied {
		t.Fatalf("next attempt write: applied=%v err=%v", applied, err)
	}
	if _, err := f.runs.GetRunAwaitingPayment(f.ctx, f.tenantID, f.invoiceID, "pi_x"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the run still waits on pi_x after the next attempt's write (err=%v)", err)
	}
}

// TestRetryInFlight_PaymentSettledBeforeTheRunWrite: the concurrent-resolver
// fake commits the payment's outcome between the charge and dunning's run
// write (P2, P3 of the design).
func TestRetryInFlight_PaymentSettledBeforeTheRunWrite(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("succeeded (P2): the run is resolved once, as a recovered retry", func(t *testing.T) {
		retry := &processingRetry{settle: func(t *testing.T, ctx context.Context, f *exhaustFixture, pi string) {
			if _, err := f.invoices.MarkPaid(ctx, f.tenantID, f.invoiceID, pi, f.clk.Now(ctx)); err != nil {
				t.Fatalf("settle paid: %v", err)
			}
			if err := f.svc.ResolveByInvoice(ctx, f.tenantID, f.invoiceID, domain.ResolutionPaymentRecovered); err != nil {
				t.Fatalf("settle's resolve: %v", err)
			}
		}}
		f := newInFlightFixture(t, db, "In Flight Paid First", retry)
		f.tick(t)
		run := f.getRun(t)
		if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
			t.Fatalf("run: %s/%s, want resolved / payment_recovered", run.State, run.Resolution)
		}
		if n := f.eventsOf(t, domain.DunningEventResolved); n != 1 {
			t.Errorf("resolved events %d, want 1", n)
		}
		for _, r := range f.retryRows(t) {
			if r.Reason == domain.DunningRetryReasonPaymentProcessing {
				t.Errorf("a processing row was written for a payment that had already succeeded: %+v", r)
			}
		}
		if n := countEmailRows(t, db, f.ctx, f.tenantID, "dunning_warning"); n != 0 {
			t.Errorf("warning emails %d, want 0", n)
		}
	})

	t.Run("failed (P3): a counted failure with its warning, no wait left behind", func(t *testing.T) {
		retry := &processingRetry{settle: func(t *testing.T, ctx context.Context, f *exhaustFixture, pi string) {
			f.failSettle(t, pi, "insufficient_funds", "Your card has insufficient funds.")
		}}
		f := newInFlightFixture(t, db, "In Flight Failed First", retry)
		retryAt := f.clk.Now(f.ctx)
		f.tick(t)
		run := f.getRun(t)
		if run.State != domain.DunningActive || run.AttemptCount != 1 {
			t.Fatalf("run: %s / %d, want active / 1", run.State, run.AttemptCount)
		}
		if want := retryAt.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
			t.Errorf("next_action_at %v, want %v", run.NextActionAt, want)
		}
		rows := f.retryRows(t)
		if len(rows) != 1 || rows[0].Metadata["payment_intent_id"] != "pi_slow_1" || rows[0].Reason == domain.DunningRetryReasonPaymentProcessing {
			t.Fatalf("retry rows %+v, want one failed row carrying pi_slow_1", rows)
		}
		if n := countEmailRows(t, db, f.ctx, f.tenantID, "dunning_warning"); n != 1 {
			t.Errorf("warning emails %d, want 1", n)
		}
		if _, err := f.runs.GetRunAwaitingPayment(f.ctx, f.tenantID, f.invoiceID, "pi_slow_1"); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("the run waits on a payment that already failed (err=%v)", err)
		}
		// The failure settle found no run waiting and released nothing; the
		// retry releases its own hold (review finding F2).
		if f.held(t) {
			t.Error("the retry's charge hold is still set: the next retry and the final action would wait for the stale-hold sweep")
		}
	})
}

// TestRetryInFlight_FailureReportBuiltBeforeTheInFlightWrite is review finding
// F1, with the concurrent-resolver fake: the failure webhook's settle builds
// its hook during the charge, after the processing stamp and before dunning's
// in-flight write, when no run waits on the payment yet; its transaction then
// takes the invoice lock after that write committed. The failure must still
// be recorded: one failed row, one warning, the wait cleared, the hold freed.
// Before the fix the hook was left out on that early read, and none of it
// happened.
func TestRetryInFlight_FailureReportBuiltBeforeTheInFlightWrite(t *testing.T) {
	db := testutil.SetupTestDB(t)
	var hook func(*sql.Tx, domain.Invoice) error
	retry := &processingRetry{settle: func(t *testing.T, ctx context.Context, f *exhaustFixture, pi string) {
		inv, err := f.invoices.Get(ctx, f.tenantID, f.invoiceID)
		if err != nil {
			t.Fatalf("read invoice: %v", err)
		}
		hook = f.svc.RetryPaymentFailed(ctx, f.tenantID, inv, pi, "insufficient_funds", "Your card has insufficient funds.")
	}}
	f := newInFlightFixture(t, db, "In Flight Report Before Write", retry)
	f.tick(t)
	if _, err := f.runs.GetRunAwaitingPayment(f.ctx, f.tenantID, f.invoiceID, "pi_slow_1"); err != nil {
		t.Fatalf("the run does not wait on pi_slow_1 after the in-flight write: %v", err)
	}

	// The settle's transaction, after the in-flight write.
	if _, res, err := f.invoices.MarkPaymentFailedReportingTransition(f.ctx, f.tenantID, f.invoiceID,
		domain.PaymentFailureReport{PaymentIntentID: "pi_slow_1", Message: "payment failed", DeclineCode: "insufficient_funds"}, hook); err != nil || !res.Recorded || !res.FirstNotice {
		t.Fatalf("failure settle: recorded=%v first=%v err=%v", res.Recorded, res.FirstNotice, err)
	}
	if n := countEmailRows(t, db, f.ctx, f.tenantID, "dunning_warning"); n != 1 {
		t.Errorf("warning emails %d, want 1", n)
	}
	if _, err := f.runs.GetRunAwaitingPayment(f.ctx, f.tenantID, f.invoiceID, "pi_slow_1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the run still waits on a payment that failed (err=%v)", err)
	}
	if f.held(t) {
		t.Error("the retry's charge hold is still set after its payment failed")
	}
	rows := f.retryRows(t)
	if len(rows) != 2 || rows[1].Metadata["payment_intent_id"] != "pi_slow_1" {
		t.Errorf("retry rows %+v, want the processing row and the failed row", rows)
	}
}
