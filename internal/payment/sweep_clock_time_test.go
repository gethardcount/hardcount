package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/scheduler"
)

// The payment reconciler runs in real time (the billing role's tick) and
// decides each row's time from the row's invoice (decideRow). The settle's
// times are read when Stripe's answer arrives, and the bound instant dates
// the row's other writes.

var sweepFrozen = time.Date(2027, 2, 14, 9, 30, 0, 0, time.UTC)

// onClock puts every record on clock clk_1 at sweepFrozen.
type onClock struct{}

func (onClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: sweepFrozen, TestClockID: "clk_1"}, nil
}
func (onClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: sweepFrozen, TestClockID: "clk_1"}, nil
}
func (onClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: sweepFrozen, TestClockID: "clk_1"}, nil
}

// stampTimeUpdater records the time its ctx gives when the failed stamp is
// written, which is the time the store stamps it at.
type stampTimeUpdater struct {
	*mockInvoiceUpdater
	failedAt time.Time
}

func (u *stampTimeUpdater) MarkPaymentFailedReportingTransition(ctx context.Context, tenantID, id string, report domain.PaymentFailureReport, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, domain.PaymentFailureResult, error) {
	u.failedAt = clock.Now(ctx)
	return u.mockInvoiceUpdater.MarkPaymentFailedReportingTransition(ctx, tenantID, id, report, then)
}

// sweepHarness is the reconciler wired as production wires it: the real
// settler, and the clock resolver on both. inv is in both stores.
func sweepHarness(inv domain.Invoice, client *mockStripeClient) (*Reconciler, *stampTimeUpdater, *recordingDunningStarter) {
	updater := &stampTimeUpdater{mockInvoiceUpdater: newMockInvoiceUpdater()}
	updater.invoices[inv.ID] = inv
	if inv.StripePaymentIntentID != "" {
		updater.byPI[inv.StripePaymentIntentID] = inv.ID
	}
	dunning := &recordingDunningStarter{}
	settler := NewStripe(client, updater, newMockWebhookStore(), nil, dunning)
	settler.SetResolver(onClock{})
	r := NewReconciler(client, newMockReconcileStore(inv), settler, time.Second)
	r.SetResolver(onClock{})
	return r, updater, dunning
}

// A real-time, test-mode ctx with nothing bound: what the billing tick gives.
func tickCtx() context.Context { return postgres.WithLivemode(context.Background(), false) }

func unknownClockInvoice() domain.Invoice {
	inv := finalizedPendingInvoice()
	inv.PaymentStatus = domain.PaymentUnknown
	inv.StripePaymentIntentID = "pi_sweep"
	return inv
}

// failingClock answers every record with err.
type failingClock struct{ err error }

func (f failingClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, f.err
}
func (f failingClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, f.err
}
func (f failingClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, f.err
}

// A row whose clock cannot be read is not settled: its sweep reports
// clock.ErrUndecided for it, and the row is stamped like a failed check so
// the queue rotates and a later tick tries it again.
func TestRealTimeSweep_AClockThatCannotBeReadLeavesTheRowForALaterTick(t *testing.T) {
	inv := unknownClockInvoice()
	r, updater, dunning := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	store := newMockReconcileStore(inv)
	r.invoices = store
	r.SetResolver(failingClock{errors.New("connection reset by peer")})
	_, errs := r.Run(tickCtx(), 10)
	if len(errs) != 1 || !errors.Is(errs[0], clock.ErrUndecided) || !strings.Contains(errs[0].Error(), "checked again on the next run; no payment is lost") {
		t.Fatalf("errors %v; want one clock.ErrUndecided for the row, saying it is checked again and no payment is lost", errs)
	}
	if got := updater.invoices[inv.ID]; got.Status == domain.InvoicePaid || got.PaidAt != nil || len(dunning.calls) != 0 {
		t.Errorf("status %s, paid_at %v, dunning %v; want the invoice untouched", got.Status, got.PaidAt, dunning.calls)
	}
	if store.byID[inv.ID].ProviderSyncedAt == nil {
		t.Error("the row was not stamped, so the queue does not rotate past it")
	}
}

// A row whose invoice, or its clock, is not found under the decision is
// skipped: nothing to settle, and no error. It is stamped like the other
// skipped rows, so a row that keeps answering not-found does not stay at the
// head of the list and take a slot every run.
func TestRealTimeSweep_AnInvoiceGoneWithItsClockIsSkippedAndStamped(t *testing.T) {
	inv := unknownClockInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	store := newMockReconcileStore(inv)
	r.invoices = store
	r.SetResolver(failingClock{fmt.Errorf("could not read invoice inv_1: %w", errs.ErrNotFound)})
	if _, runErrs := r.Run(tickCtx(), 10); len(runErrs) != 0 {
		t.Fatalf("errors %v; want none", runErrs)
	}
	if got := updater.invoices[inv.ID]; got.Status == domain.InvoicePaid {
		t.Error("a gone invoice was settled")
	}
	if store.byID[inv.ID].ProviderSyncedAt == nil {
		t.Error("the gone row was not stamped, so it stays at the head of the list")
	}
}

// The hosted-session sweep treats a gone row the same way: skipped, no
// error, and the session touched so the list moves on.
func TestRealTimeSweep_HostedSessionWhoseInvoiceIsGoneIsTouched(t *testing.T) {
	inv := finalizedPendingInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	ledger := &fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", InvoiceID: inv.ID, StripeSessionID: "cs_1", Status: "completed"}}}
	r.SetHostedSessions(ledger, fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}}, nil)
	r.SetResolver(failingClock{fmt.Errorf("could not read invoice inv_1: %w", errs.ErrNotFound)})
	if _, runErrs := r.sweepHostedSessions(tickCtx(), 50, new(scheduler.Panics)); len(runErrs) != 0 {
		t.Fatalf("errors %v; want none", runErrs)
	}
	if got := updater.invoices[inv.ID]; got.Status == domain.InvoicePaid || len(ledger.touched) != 1 {
		t.Errorf("status %s, touched %v; want unpaid and the session touched", got.Status, ledger.touched)
	}
}

// noClock puts every record on no clock: the machine's time, as the engine
// answers.
type noClock struct{}

func (noClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: time.Now()}, nil
}
func (noClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: time.Now()}, nil
}
func (noClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: time.Now()}, nil
}

// A test-mode invoice on no clock is decided as real time: paid at the
// machine's time.
func TestRealTimeSweep_AnInvoiceOnNoClockIsPaidAtRealTime(t *testing.T) {
	inv := unknownClockInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	r.SetResolver(noClock{})
	r.settler.(*Stripe).SetResolver(noClock{})
	arrival := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC) // far from the row's instant, read from the machine
	r.settler.(*Stripe).SetClock(clock.NewFake(arrival))
	if _, runErrs := r.Run(tickCtx(), 10); len(runErrs) != 0 {
		t.Fatalf("errors %v; want none", runErrs)
	}
	if got := updater.invoices[inv.ID]; got.PaidAt == nil || !got.PaidAt.Equal(arrival) {
		t.Errorf("paid_at %v; want %v, when Stripe's answer arrived, not the row's instant", got.PaidAt, arrival)
	}
}

// A row on no clock is bound to one read of the server's clock, as decideFor
// binds an event, so what the row's work stamps is that instant, not a
// later read each time.
func TestRealTimeSweep_ARowOnNoClockIsBoundToOneInstant(t *testing.T) {
	r, _, _ := sweepHarness(unknownClockInvoice(), &mockStripeClient{})
	r.SetResolver(noClock{})
	decided, gone, err := r.decideRow(tickCtx(), "t1", "inv_x")
	if err != nil || gone {
		t.Fatalf("decideRow: gone %v, err %v", gone, err)
	}
	if _, ok := clock.EffectiveNow(decided); !ok {
		t.Error("the row's ctx carries no instant: each stamp would read the clock again")
	}
}

// The hosted-session sweep decides each row's time too: a session whose
// invoice's clock cannot be read is not settled, its error is reported, and
// the session is touched so the queue rotates.
func TestRealTimeSweep_HostedSessionWithAnUnreadableClockIsNotSettled(t *testing.T) {
	inv := finalizedPendingInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	ledger := &fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", InvoiceID: inv.ID, StripeSessionID: "cs_1", Status: "completed"}}}
	r.SetHostedSessions(ledger, fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}}, nil)
	r.SetResolver(failingClock{errors.New("connection reset by peer")})
	_, runErrs := r.sweepHostedSessions(tickCtx(), 50, new(scheduler.Panics))
	if len(runErrs) != 1 || !errors.Is(runErrs[0], clock.ErrUndecided) {
		t.Fatalf("errors %v; want one clock.ErrUndecided", runErrs)
	}
	if got := updater.invoices[inv.ID]; got.Status == domain.InvoicePaid || len(ledger.touched) != 1 {
		t.Errorf("status %s, touched %v; want unpaid and the session touched", got.Status, ledger.touched)
	}
}

// In live mode no record is on a test clock: the sweep decides nothing, so
// a clock that cannot be read never holds back a live payment.
func TestRealTimeSweep_ALiveTickDecidesNothing(t *testing.T) {
	inv := unknownClockInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	r.SetResolver(failingClock{errors.New("connection reset by peer")})
	if _, runErrs := r.Run(postgres.WithLivemode(context.Background(), true), 10); len(runErrs) != 0 {
		t.Fatalf("errors %v; want none", runErrs)
	}
	if got := updater.invoices[inv.ID]; got.Status != domain.InvoicePaid {
		t.Errorf("status %s; want paid", got.Status)
	}
}

// A sweep with no mode on its ctx is a fault (Run reads one first), so a
// row reached without one is refused, not decided as test mode or as live.
func TestRealTimeSweep_ARowWithNoModeIsRefused(t *testing.T) {
	inv := finalizedPendingInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	r.SetHostedSessions(
		&fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", InvoiceID: inv.ID, StripeSessionID: "cs_1", Status: "completed"}}},
		fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}},
		nil)
	_, runErrs := r.sweepHostedSessions(context.Background(), 50, new(scheduler.Panics))
	if len(runErrs) != 1 || !errors.Is(runErrs[0], clock.ErrUndecided) || updater.invoices[inv.ID].Status == domain.InvoicePaid {
		t.Fatalf("errors %v, status %s; want one clock.ErrUndecided and the invoice unpaid", runErrs, updater.invoices[inv.ID].Status)
	}
}
