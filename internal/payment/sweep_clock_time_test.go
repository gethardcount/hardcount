package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/scheduler"
)

// The payment reconciler runs in real time (the billing role's tick) and
// reaches clock invoices too. It decides each row's time from the row's
// invoice (decideRow), and the settle keeps that decision: with no
// contracted instant on the charge, paid_at, the failed stamp and dunning's
// start are all at the invoice's clock time (dunningSimFailedAt: the
// charge's anchor, else the decided time).

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
	r := NewReconciler(client, newMockReconcileStore(inv), time.Second)
	r.SetSettler(settler)
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

func TestRealTimeSweep_PaysAClockInvoiceAtItsClockTime(t *testing.T) {
	r, updater, _ := sweepHarness(unknownClockInvoice(), &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	if _, errs := r.Run(tickCtx(), 10); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	got := updater.invoices["inv_1"]
	if got.Status != domain.InvoicePaid || got.PaidAt == nil || !got.PaidAt.Equal(sweepFrozen) {
		t.Errorf("status %s, paid_at %v; want paid at the clock's %v", got.Status, got.PaidAt, sweepFrozen)
	}
}

// On a canceled PaymentIntent with no anchor, the failed stamp is at the
// clock's time and so is dunning's start: not the invoice's issue instant,
// which would put the first retry in the past and run it at once.
func TestRealTimeSweep_FailingAClockInvoice_StampsAndStartsDunningAtClockTime(t *testing.T) {
	r, updater, dunning := sweepHarness(unknownClockInvoice(), &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "canceled", Charged: true},
	}})
	if _, errs := r.Run(tickCtx(), 10); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if !updater.failedAt.Equal(sweepFrozen) {
		t.Errorf("failed stamp at %v, want the clock's %v", updater.failedAt, sweepFrozen)
	}
	if len(dunning.calls) != 1 || !dunning.calls[0].failureAt.Equal(sweepFrozen) {
		t.Errorf("dunning starts %+v, want one at the clock's %v (not the issue instant %v)", dunning.calls, sweepFrozen, *unknownClockInvoice().IssuedAt)
	}
}

// The hosted-session sweep settles through SettleSucceeded directly, not the
// PaymentIntent sweep, so it is guarded on its own.
func TestRealTimeSweep_HostedSessionPaysAClockInvoiceAtItsClockTime(t *testing.T) {
	inv := finalizedPendingInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	r.SetHostedSessions(
		&fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", InvoiceID: inv.ID, StripeSessionID: "cs_1", Status: "completed"}}},
		fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}},
		nil)
	if _, errs := r.sweepHostedSessions(tickCtx(), 50, new(scheduler.Panics)); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	got := updater.invoices[inv.ID]
	if got.Status != domain.InvoicePaid || got.PaidAt == nil || !got.PaidAt.Equal(sweepFrozen) {
		t.Errorf("status %s, paid_at %v; want paid at the clock's %v", got.Status, got.PaidAt, sweepFrozen)
	}
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
	if len(errs) != 1 || !errors.Is(errs[0], clock.ErrUndecided) {
		t.Fatalf("errors %v; want one clock.ErrUndecided for the row", errs)
	}
	if got := updater.invoices[inv.ID]; got.Status == domain.InvoicePaid || got.PaidAt != nil || len(dunning.calls) != 0 {
		t.Errorf("status %s, paid_at %v, dunning %v; want the invoice untouched", got.Status, got.PaidAt, dunning.calls)
	}
	if store.byID[inv.ID].ProviderSyncedAt == nil {
		t.Error("the row was not stamped, so the queue does not rotate past it")
	}
}

// An invoice deleted with its clock between the list and the read is
// skipped: nothing to settle, and no error.
func TestRealTimeSweep_AnInvoiceGoneWithItsClockIsSkipped(t *testing.T) {
	inv := unknownClockInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	r.SetResolver(failingClock{fmt.Errorf("could not read invoice inv_1: %w", errs.ErrNotFound)})
	if _, runErrs := r.Run(tickCtx(), 10); len(runErrs) != 0 {
		t.Fatalf("errors %v; want none", runErrs)
	}
	if got := updater.invoices[inv.ID]; got.Status == domain.InvoicePaid {
		t.Error("a gone invoice was settled")
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
