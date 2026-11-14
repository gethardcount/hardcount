package payment

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/scheduler"
)

// The payment reconciler runs in real time (the billing role's tick) and
// reaches clock invoices too. When it settles one, with no contracted
// instant on the charge, paid_at and the failed stamp are dated at the
// invoice's clock time, and dunning starts from the invoice's issue instant
// (dunningSimFailedAt: the charge's anchor, else the issue instant). These
// pass today; they guard the step that makes the reconciler decide each
// row's time itself.

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
// clock's time and dunning starts at the invoice's issue instant, not the
// clock's time. What main does; the step that decides the sweep's time per
// row revisits the dunning start.
func TestRealTimeSweep_FailingAClockInvoice_StampsAtClockTime_DunningStartsAtIssue(t *testing.T) {
	r, updater, dunning := sweepHarness(unknownClockInvoice(), &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "canceled", Charged: true},
	}})
	if _, errs := r.Run(tickCtx(), 10); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if !updater.failedAt.Equal(sweepFrozen) {
		t.Errorf("failed stamp at %v, want the clock's %v", updater.failedAt, sweepFrozen)
	}
	issued := *unknownClockInvoice().IssuedAt
	if len(dunning.calls) != 1 || !dunning.calls[0].failureAt.Equal(issued) {
		t.Errorf("dunning starts %+v, want one from the issue instant %v", dunning.calls, issued)
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
