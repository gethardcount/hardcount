package dunning

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A dunning retry whose payment comes back still processing must not close
// the run as recovered (#1289). Before, RetryPayment returned nil for it, and
// processRun resolved the run as payment_recovered and fired dunning.resolved
// over an unpaid invoice; when the payment then failed, nothing retried it.
//
// These pin the service's side on the in-memory store, whose predicates
// mirror the real store's. The Postgres SQL and the whole flow through the
// real charge, webhook and settle are pinned in
// internal/dunning/retry_in_flight_integration_test.go and
// internal/api/dunning_retry_in_flight_e2e_test.go.

// inFlightPI is the PaymentIntent the retry's charge left processing.
const inFlightPI = "pi_processing_1"

// inFlight is the adapter's answer for a retry whose payment is processing.
func inFlight(pi string) error {
	return &RetryError{PaymentIntentID: pi, Err: ErrPaymentInFlight}
}

// retryHold is the hold the retry's charge took and kept while its payment
// was processing; the adapter hands it to dunning on the in-flight answer.
var retryHold = domain.ChargeHold{Until: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}

func inFlightHeld(pi string) error {
	return &RetryError{PaymentIntentID: pi, Err: ErrPaymentInFlight, Hold: retryHold}
}

// recordingChargeHolds is the invoice store's hold port: the final action's
// claim always succeeds, and every release is recorded.
type recordingChargeHolds struct{ released []domain.ChargeHold }

func (r *recordingChargeHolds) ClaimChargeHoldForFinalAction(context.Context, string, string) (domain.ChargeHold, error) {
	return domain.ChargeHold{Until: time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

func (r *recordingChargeHolds) ReleaseChargeHold(_ context.Context, _, _ string, h domain.ChargeHold) error {
	r.released = append(r.released, h)
	return nil
}

func (r *recordingChargeHolds) releasedRetryHold() bool {
	for _, h := range r.released {
		if h == retryHold {
			return true
		}
	}
	return false
}

// mutableInvGet serves the invoice as the test last set it.
type mutableInvGet struct{ inv domain.Invoice }

func (g *mutableInvGet) Get(context.Context, string, string) (domain.Invoice, error) {
	return g.inv, nil
}

// recordingHoldReleaser records the hold releases RetryPaymentFailed asks for.
type recordingHoldReleaser struct{ released []string }

func (r *recordingHoldReleaser) ReleaseRetryChargeHoldTx(_ context.Context, _ *sql.Tx, invoiceID, pi string) (bool, error) {
	r.released = append(r.released, invoiceID+"/"+pi)
	return true, nil
}

// funcRetrier runs fn as the retry: a test changes the invoice there, the
// way the charge and a racing settle change it during the call.
type funcRetrier struct{ fn func() error }

func (f funcRetrier) RetryPayment(context.Context, string, string, string) error { return f.fn() }

// explainingRetrier is a funcRetrier that also explains a failure the webhook
// recorded first, as the production adapter does.
type explainingRetrier struct {
	funcRetrier
	explained []string
}

func (e *explainingRetrier) ExplainRetryFailure(_ context.Context, _, _, pi string) error {
	e.explained = append(e.explained, pi)
	return &RetryError{PaymentIntentID: pi, Err: errors.New("payment failed"), Ask: domain.DeclineAskConfirm, NoReason: true}
}

type inFlightHarness struct {
	t       *testing.T
	ctx     context.Context
	store   *memStore
	svc     *Service
	clk     *clock.Fake
	emails  *recordingEmailNotifier
	events  *captureDispatcher
	holds   *recordingHoldReleaser
	invoice *mutableInvGet
	runID   string
	start   time.Time
}

// newInFlightHarness starts a run on a failed invoice under the default
// memStore policy (three retries: 3, 3 and 5 days) and moves the clock to
// retry 1's due time.
func newInFlightHarness(t *testing.T, retrier PaymentRetrier) *inFlightHarness {
	t.Helper()
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	h := &inFlightHarness{
		t: t, ctx: context.Background(), store: newMemStore(), clk: clock.NewFake(start),
		emails: &recordingEmailNotifier{}, events: &captureDispatcher{}, holds: &recordingHoldReleaser{},
		invoice: &mutableInvGet{inv: domain.Invoice{
			ID: "inv_1", CustomerID: "cus_1", InvoiceNumber: "INV-1", Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentFailed, AmountDueCents: 5000,
		}},
		start: start,
	}
	h.svc = NewService(h.store, retrier, h.clk)
	h.svc.SetEmailNotifier(h.emails)
	h.svc.SetCustomerEmailFetcher(stubCustomerEmail{})
	h.svc.SetEventDispatcher(h.events)
	h.svc.SetRetryHoldReleaser(h.holds)
	h.svc.SetSubscriptionPauser(nil, h.invoice)
	run, err := h.svc.StartDunning(h.ctx, "t1", "inv_1", "cus_1", time.Time{}, domain.DunningCausePaymentFailed)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	h.runID = run.ID
	h.clk.Set(*run.NextActionAt)
	return h
}

func (h *inFlightHarness) tick() {
	h.t.Helper()
	if _, errs := h.svc.ProcessDueRuns(h.ctx, "t1", 20); len(errs) != 0 {
		h.t.Fatalf("dunning tick: %v", errs)
	}
}

func (h *inFlightHarness) run() domain.InvoiceDunningRun {
	h.t.Helper()
	run, err := h.store.GetRun(h.ctx, "t1", h.runID)
	if err != nil {
		h.t.Fatalf("get run: %v", err)
	}
	return run
}

// retryEvents are the run's retry_attempted rows, oldest first.
func (h *inFlightHarness) retryEvents() []domain.InvoiceDunningEvent {
	var out []domain.InvoiceDunningEvent
	for _, e := range h.store.events {
		if e.EventType == domain.DunningEventRetryAttempted {
			out = append(out, e)
		}
	}
	return out
}

// processing puts the invoice where the charge left it: processing on pi.
func (h *inFlightHarness) processing(pi string) {
	h.invoice.inv.PaymentStatus = domain.PaymentProcessing
	h.invoice.inv.StripePaymentIntentID = pi
}

// failSettle is the failure settle of pi as payment.SettleFailed runs it: the
// invoice is stamped failed, then the hook from RetryPaymentFailed runs on the
// settle's transaction.
func (h *inFlightHarness) failSettle(pi, declineCode, cardMessage string) {
	h.t.Helper()
	before := h.invoice.inv
	hook := h.svc.RetryPaymentFailed(h.ctx, "t1", before, pi, declineCode, cardMessage)
	h.invoice.inv.PaymentStatus = domain.PaymentFailed
	h.invoice.inv.StripePaymentIntentID = pi
	if hook == nil {
		return
	}
	if err := hook(nil, h.invoice.inv); err != nil {
		h.t.Fatalf("retry failure hook: %v", err)
	}
}

func TestRetryInFlight_KeepsRunActiveWithNoEmail(t *testing.T) {
	h := newInFlightHarness(t, &scriptedRetrier{results: []error{inFlight(inFlightPI)}})
	retryAt := h.clk.Now(h.ctx)
	h.processing(inFlightPI)
	h.tick()

	run := h.run()
	if run.State != domain.DunningActive || run.AttemptCount != 1 {
		t.Fatalf("run after a processing retry: state %s attempts %d, want active / 1 (the attempt counts, the run waits)", run.State, run.AttemptCount)
	}
	if want := retryAt.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("next_action_at %v, want %v: rescheduled exactly as a failure is", run.NextActionAt, want)
	}
	if got := h.store.awaiting[h.runID]; got != inFlightPI {
		t.Errorf("awaited PaymentIntent %q, want %q", got, inFlightPI)
	}
	if h.emails.warnings != 0 {
		t.Errorf("warning emails %d, want 0: the payment has not failed", h.emails.warnings)
	}
	if n := h.events.countOf(domain.EventDunningResolved); n != 0 {
		t.Errorf("dunning.resolved fired %d times over an unpaid invoice", n)
	}
	rows := h.retryEvents()
	if len(rows) != 1 || rows[0].Reason != domain.DunningRetryReasonPaymentProcessing || rows[0].State != domain.DunningActive {
		t.Fatalf("retry rows %+v, want one active payment_processing row", rows)
	}
	if rows[0].Metadata["payment_intent_id"] != nil {
		t.Errorf("processing row carries a PaymentIntent %v; the later failed or paid attempt keeps its own row", rows[0].Metadata)
	}
}

// The payment fails later: the hook records the failed attempt with its
// PaymentIntent, releases the hold, sends the one warning email with the Ask
// the decline code gives, and moves the next retry so it does not follow the
// failure at once. A second report of the same failure does nothing.
func TestRetryInFlight_LaterFailureRecordedOnceWithWarning(t *testing.T) {
	h := newInFlightHarness(t, &scriptedRetrier{results: []error{inFlight(inFlightPI)}})
	h.processing(inFlightPI)
	h.tick()
	scheduled := *h.run().NextActionAt

	// The bank debit fails two days later.
	h.clk.Advance(2 * 24 * time.Hour)
	failedAt := h.clk.Now(h.ctx)
	h.failSettle(inFlightPI, "authentication_required", "Your bank needs you to confirm this payment.")

	run := h.run()
	if run.State != domain.DunningActive || run.AttemptCount != 1 {
		t.Fatalf("run after the failure: state %s attempts %d, want active / 1", run.State, run.AttemptCount)
	}
	if want := failedAt.Add(3 * 24 * time.Hour); !want.After(scheduled) || run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("next_action_at %v, want the failure plus the next wait %v (later than the scheduled %v)", run.NextActionAt, want, scheduled)
	}
	if h.emails.warnings != 1 {
		t.Fatalf("warning emails %d, want 1", h.emails.warnings)
	}
	if h.emails.lastWarnAsk != domain.DeclineAskConfirm || h.emails.lastWarnReason != "" {
		t.Errorf("warning ask %q reason %q, want confirm with no reason line", h.emails.lastWarnAsk, h.emails.lastWarnReason)
	}
	if len(h.holds.released) != 1 || h.holds.released[0] != "inv_1/"+inFlightPI {
		t.Errorf("hold releases %v, want one for inv_1/%s", h.holds.released, inFlightPI)
	}
	rows := h.retryEvents()
	if len(rows) != 2 {
		t.Fatalf("retry rows %+v, want the processing row and the failed row", rows)
	}
	failed := rows[1]
	if failed.Metadata["payment_intent_id"] != inFlightPI || !failed.CreatedAt.Equal(failedAt) || failed.AttemptCount != 1 {
		t.Errorf("failed row %+v, want attempt 1 at the failure %v carrying %s", failed, failedAt, inFlightPI)
	}

	// The reconciler reports the same failure: nothing more happens.
	h.failSettle(inFlightPI, "authentication_required", "")
	if h.emails.warnings != 1 || len(h.retryEvents()) != 2 || len(h.holds.released) != 1 {
		t.Errorf("after a second report: %d warnings, %d retry rows, %d releases; want 1, 2, 1",
			h.emails.warnings, len(h.retryEvents()), len(h.holds.released))
	}
}

// An ordinary decline's warning shows Stripe's card message.
func TestRetryInFlight_LaterDeclineWarnsWithCardMessage(t *testing.T) {
	h := newInFlightHarness(t, &scriptedRetrier{results: []error{inFlight(inFlightPI)}})
	h.processing(inFlightPI)
	h.tick()
	h.failSettle(inFlightPI, "insufficient_funds", "Your card has insufficient funds.")
	if h.emails.warnings != 1 || h.emails.lastWarnAsk != domain.DeclineAskNone || h.emails.lastWarnReason != "Your card has insufficient funds." {
		t.Errorf("warning: %d sent, ask %q reason %q; want one, no ask, the card message", h.emails.warnings, h.emails.lastWarnAsk, h.emails.lastWarnReason)
	}
	// The next retry runs on its date, and this one's charge succeeds.
	h.clk.Set(*h.run().NextActionAt)
	h.svc.SetRetrier(funcRetrier{fn: func() error {
		h.invoice.inv.Status = domain.InvoicePaid
		h.invoice.inv.PaymentStatus = domain.PaymentSucceeded
		return nil
	}})
	h.tick()
	if run := h.run(); run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered || run.AttemptCount != 2 {
		t.Errorf("run after retry 2: %s/%s attempts %d, want resolved / payment_recovered / 2", run.State, run.Resolution, run.AttemptCount)
	}
}

// A synchronous decline never awaited a payment: the hook has nothing to do,
// and processRun's own failure path is the attempt's one record and email.
func TestRetryPaymentFailed_NoRunWaiting(t *testing.T) {
	h := newInFlightHarness(t, &scriptedRetrier{results: []error{&RetryError{PaymentIntentID: "pi_sync", Err: errors.New("payment failed: Your card was declined.")}}})
	h.tick()
	// The settle of that decline runs the hook too; under the lock it finds
	// no run waiting on pi_sync and does nothing.
	hook := h.svc.RetryPaymentFailed(h.ctx, "t1", h.invoice.inv, "pi_sync", "generic_decline", "Your card was declined.")
	if hook == nil {
		t.Fatal("RetryPaymentFailed returned no hook; the compare-and-set under the lock decides, not a read before it")
	}
	if err := hook(nil, h.invoice.inv); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if h.emails.warnings != 1 || len(h.retryEvents()) != 1 || len(h.holds.released) != 0 {
		t.Errorf("%d warnings, %d retry rows, %d releases; want 1, 1, 0", h.emails.warnings, len(h.retryEvents()), len(h.holds.released))
	}
}

// The last retry comes back processing: the final actions are held, the run
// keeps its lease, and the failure sends no warning (the escalation follows).
// Exhaust-on-entry then escalates, dated at the failure, not the retry.
func TestRetryInFlight_FinalRetryHeldThenEscalatedAtFailure(t *testing.T) {
	h := newInFlightHarness(t, &scriptedRetrier{results: []error{inFlight(inFlightPI)}})
	// Two retries already used: the next one is the last.
	run := h.run()
	run.AttemptCount = 2
	h.store.runs[h.runID] = run
	retryAt := h.clk.Now(h.ctx)
	h.processing(inFlightPI)
	h.tick()

	run = h.run()
	if run.State != domain.DunningActive || run.AttemptCount != 3 {
		t.Fatalf("run after the last retry came back processing: %s / %d, want active / 3", run.State, run.AttemptCount)
	}
	lease := leaseFrom(retryAt)
	if run.NextActionAt == nil || !run.NextActionAt.Equal(lease) {
		t.Errorf("next_action_at %v, want the exhaustion lease %v", run.NextActionAt, lease)
	}
	if h.emails.escalations != 0 || h.emails.warnings != 0 {
		t.Errorf("emails while the last payment is processing: %d escalations, %d warnings; want none", h.emails.escalations, h.emails.warnings)
	}

	h.clk.Advance(3 * 24 * time.Hour)
	failedAt := h.clk.Now(h.ctx)
	h.failSettle(inFlightPI, "insufficient_funds", "Your card has insufficient funds.")
	if h.emails.warnings != 0 {
		t.Errorf("warning emails after the last retry failed: %d, want 0 (the escalation is its email)", h.emails.warnings)
	}
	if run := h.run(); run.NextActionAt == nil || !run.NextActionAt.Equal(lease) {
		t.Errorf("next_action_at after the failure %v, want the lease %v kept", run.NextActionAt, lease)
	}

	h.tick()
	run = h.run()
	if run.State != domain.DunningEscalated {
		t.Fatalf("run after exhaust-on-entry: %s, want escalated", run.State)
	}
	if run.ResolvedAt == nil || !run.ResolvedAt.Equal(failedAt) {
		t.Errorf("escalation dated %v, want the failure %v (never the retry %v, days before it)", run.ResolvedAt, failedAt, retryAt)
	}
	if h.emails.escalations != 1 {
		t.Errorf("escalation emails %d, want 1", h.emails.escalations)
	}
}

// P2: the payment succeeded before the in-flight write. The settle resolved
// the run; the write finds the payment gone and records the recovered retry.
func TestRetryInFlight_SucceededBeforeTheWrite(t *testing.T) {
	var h *inFlightHarness
	h = newInFlightHarness(t, funcRetrier{fn: func() error {
		// The charge left it processing, and its success webhook settled it
		// (and resolved the run) before the run write.
		h.store.paymentMoved["inv_1"] = true
		h.invoice.inv.Status = domain.InvoicePaid
		h.invoice.inv.PaymentStatus = domain.PaymentSucceeded
		if err := h.svc.ResolveByInvoice(h.ctx, "t1", "inv_1", domain.ResolutionPaymentRecovered); err != nil {
			t.Errorf("settle's resolve: %v", err)
		}
		return inFlightHeld(inFlightPI)
	}})
	holds := &recordingChargeHolds{}
	h.svc.SetChargeHoldClaimer(holds)
	h.tick()
	run := h.run()
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
		t.Fatalf("run: %s/%s, want resolved / payment_recovered", run.State, run.Resolution)
	}
	if !holds.releasedRetryHold() {
		t.Errorf("hold releases %v, want the retry's own hold released", holds.released)
	}
	if n := h.events.countOf(domain.EventDunningResolved); n != 1 {
		t.Errorf("dunning.resolved fired %d times, want 1", n)
	}
	if h.emails.warnings != 0 {
		t.Errorf("warning emails %d, want 0", h.emails.warnings)
	}
}

// P3: the payment failed before the in-flight write, so no run waited on it
// when its failure settled. The write finds the invoice failed on the retry's
// PaymentIntent and records a counted failure, with its warning, as if the
// charge had answered the decline.
func TestRetryInFlight_FailedBeforeTheWrite(t *testing.T) {
	var h *inFlightHarness
	retrier := &explainingRetrier{funcRetrier: funcRetrier{fn: func() error {
		// The charge left it processing, and its failure settled before the
		// run write, with no run waiting on it yet.
		h.store.paymentMoved["inv_1"] = true
		h.failSettle(inFlightPI, "authentication_required", "")
		return inFlightHeld(inFlightPI)
	}}}
	h = newInFlightHarness(t, retrier)
	holds := &recordingChargeHolds{}
	h.svc.SetChargeHoldClaimer(holds)
	retryAt := h.clk.Now(h.ctx)
	h.tick()
	// The failure settle's hook found no run waiting and released nothing;
	// the retry releases its own hold, so the next retry does not wait for
	// the stale-hold sweep (review finding F2).
	if !holds.releasedRetryHold() {
		t.Errorf("hold releases %v, want the retry's own hold released", holds.released)
	}

	run := h.run()
	if run.State != domain.DunningActive || run.AttemptCount != 1 {
		t.Fatalf("run: %s / %d, want active / 1", run.State, run.AttemptCount)
	}
	if want := retryAt.Add(3 * 24 * time.Hour); run.NextActionAt == nil || !run.NextActionAt.Equal(want) {
		t.Errorf("next_action_at %v, want %v", run.NextActionAt, want)
	}
	if len(retrier.explained) != 1 || retrier.explained[0] != inFlightPI {
		t.Errorf("failures explained %v, want %s", retrier.explained, inFlightPI)
	}
	if h.emails.warnings != 1 || h.emails.lastWarnAsk != domain.DeclineAskConfirm || h.emails.lastWarnReason != "" {
		t.Errorf("warning: %d sent, ask %q reason %q; want one asking to confirm, no reason line", h.emails.warnings, h.emails.lastWarnAsk, h.emails.lastWarnReason)
	}
	rows := h.retryEvents()
	if len(rows) != 1 || rows[0].Metadata["payment_intent_id"] != inFlightPI {
		t.Errorf("retry rows %+v, want one failed row carrying the PaymentIntent", rows)
	}
}

// Case 2 of the design: the charge's processing stamp was refused because the
// failure webhook recorded the decline during the call, so the adapter
// answers a definite failure without a reason line. Counted, warned, active.
func TestRetryFailure_RecordedByWebhookFirst(t *testing.T) {
	h := newInFlightHarness(t, &scriptedRetrier{results: []error{
		&RetryError{PaymentIntentID: "pi_race", Err: errors.New("payment failed"), Ask: domain.DeclineAskNone, NoReason: true},
	}})
	h.tick()
	run := h.run()
	if run.State != domain.DunningActive || run.AttemptCount != 1 {
		t.Fatalf("run: %s / %d, want active / 1", run.State, run.AttemptCount)
	}
	if h.emails.warnings != 1 || h.emails.lastWarnReason != "" {
		t.Errorf("warning: %d sent, reason %q; want one with no reason line", h.emails.warnings, h.emails.lastWarnReason)
	}
}

func TestExhaustStampAt_LaterFailureWins(t *testing.T) {
	retry := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	lease := leaseFrom(retry)
	run := domain.InvoiceDunningRun{LastAttemptAt: &retry}
	attemptAt := retry.Add(time.Hour)
	if got := exhaustStampAt(run, &lease, attemptAt, time.Time{}); !got.Equal(retry) {
		t.Errorf("no recorded failure: %v, want the retry %v", got, retry)
	}
	failed := retry.Add(48 * time.Hour)
	if got := exhaustStampAt(run, &lease, attemptAt, failed); !got.Equal(failed) {
		t.Errorf("failure after the retry: %v, want the failure %v", got, failed)
	}
	if got := exhaustStampAt(run, &lease, attemptAt, retry.Add(-time.Hour)); !got.Equal(retry) {
		t.Errorf("failure before the retry: %v, want the retry %v", got, retry)
	}
	other := retry.Add(24 * time.Hour)
	if got := exhaustStampAt(run, &other, attemptAt, failed); !got.Equal(attemptAt) {
		t.Errorf("not the lease: %v, want the attempt %v", got, attemptAt)
	}
}

// The failure webhook arrives between the charge's processing stamp and the
// in-flight write (review finding F1). Its settle builds the hook before its
// transaction opens, when no run waits on the payment yet, and its
// transaction takes the invoice lock only after the in-flight write
// committed. The hook must still record the failure: the compare-and-set
// under the lock decides, never the read before it.
func TestRetryInFlight_FailureReportReadBeforeTheInFlightWrite(t *testing.T) {
	var h *inFlightHarness
	var hook func(*sql.Tx, domain.Invoice) error
	h = newInFlightHarness(t, funcRetrier{fn: func() error {
		h.processing(inFlightPI)
		hook = h.svc.RetryPaymentFailed(h.ctx, "t1", h.invoice.inv, inFlightPI, "insufficient_funds", "Your card has insufficient funds.")
		return inFlight(inFlightPI)
	}})
	h.tick()
	if got := h.store.awaiting[h.runID]; got != inFlightPI {
		t.Fatalf("awaited PaymentIntent %q after the in-flight write, want %q", got, inFlightPI)
	}

	// The settle's transaction, after the in-flight write.
	h.invoice.inv.PaymentStatus = domain.PaymentFailed
	if hook == nil {
		t.Fatal("RetryPaymentFailed returned no hook because no run waited on the payment when it was called; the failure would be lost")
	}
	if err := hook(nil, h.invoice.inv); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if h.emails.warnings != 1 {
		t.Errorf("warning emails %d, want 1", h.emails.warnings)
	}
	if _, waiting := h.store.awaiting[h.runID]; waiting {
		t.Error("the run still waits on a payment that failed")
	}
	if len(h.holds.released) != 1 {
		t.Errorf("hold releases %v, want one", h.holds.released)
	}
	rows := h.retryEvents()
	if len(rows) != 2 || rows[1].Metadata["payment_intent_id"] != inFlightPI {
		t.Errorf("retry rows %+v, want the processing row and the failed row", rows)
	}
}

// recordThenSettle is the in-memory store, except that the failure settle
// commits right after the in-flight write, before processRun goes on.
type recordThenSettle struct {
	*memStore
	after func()
}

func (r *recordThenSettle) RecordRetryInFlight(ctx context.Context, tenantID string, run domain.InvoiceDunningRun, expected int, pi string, event domain.InvoiceDunningEvent) (RetryInFlightResult, error) {
	res, err := r.memStore.RecordRetryInFlight(ctx, tenantID, run, expected, pi, event)
	if err == nil && res == RetryInFlightRecorded && r.after != nil {
		r.after()
	}
	return res, err
}

// The last retry comes back processing and its failure settles right after
// the in-flight write (review finding F3). The final actions do not run
// inline, where they read the failed invoice and escalated at the retry's
// instant, before the failure row. Exhaust-on-entry runs them once the lease
// has passed, dated at the failure.
func TestRetryInFlight_FinalRetryFailingAtOnceEscalatesAtTheFailure(t *testing.T) {
	var h *inFlightHarness
	h = newInFlightHarness(t, funcRetrier{fn: func() error {
		h.processing(inFlightPI)
		return inFlight(inFlightPI)
	}})
	wrapped := &recordThenSettle{memStore: h.store}
	h.svc = NewService(wrapped, h.svc.retrier, h.clk)
	h.svc.SetEmailNotifier(h.emails)
	h.svc.SetCustomerEmailFetcher(stubCustomerEmail{})
	h.svc.SetEventDispatcher(h.events)
	h.svc.SetRetryHoldReleaser(h.holds)
	h.svc.SetSubscriptionPauser(nil, h.invoice)
	run := h.run()
	run.AttemptCount = 2
	h.store.runs[h.runID] = run
	retryAt := h.clk.Now(h.ctx)
	var failedAt time.Time
	wrapped.after = func() {
		h.clk.Advance(time.Minute)
		failedAt = h.clk.Now(h.ctx)
		h.failSettle(inFlightPI, "insufficient_funds", "Your card has insufficient funds.")
	}
	h.tick()

	if run := h.run(); run.State != domain.DunningActive {
		t.Fatalf("run right after the last retry's failure: %s, want active (the final actions wait for exhaust-on-entry)", run.State)
	}
	if h.emails.escalations != 0 {
		t.Fatalf("escalation emails %d in the retry's own tick, want 0", h.emails.escalations)
	}

	wrapped.after = nil
	h.clk.Set(leaseFrom(retryAt).Add(time.Minute))
	h.tick()
	run = h.run()
	if run.State != domain.DunningEscalated {
		t.Fatalf("run after the lease: %s, want escalated", run.State)
	}
	if run.ResolvedAt == nil || !run.ResolvedAt.Equal(failedAt) {
		t.Errorf("escalation dated %v, want the failure %v (the retry was %v)", run.ResolvedAt, failedAt, retryAt)
	}
}

// The invoice was voided while its payment was processing (review finding
// F4): the failure is recorded on the run, but no warning goes out, as the
// generic failure email is not sent for an invoice no longer finalized.
func TestRetryPaymentFailed_NoWarningForAnInvoiceNoLongerFinalized(t *testing.T) {
	h := newInFlightHarness(t, &scriptedRetrier{results: []error{inFlight(inFlightPI)}})
	h.processing(inFlightPI)
	h.tick()
	h.invoice.inv.Status = domain.InvoiceVoided
	h.failSettle(inFlightPI, "insufficient_funds", "Your card has insufficient funds.")
	if h.emails.warnings != 0 {
		t.Errorf("warning emails %d for a voided invoice, want 0", h.emails.warnings)
	}
	if rows := h.retryEvents(); len(rows) != 2 {
		t.Errorf("retry rows %d, want the processing row and the failed row", len(rows))
	}
}
