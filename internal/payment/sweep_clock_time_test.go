package payment

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The payment reconciler runs on the billing role's tick, which binds no
// instant, and binds each row's own (rowTime). The settle's times are read
// when Stripe's answer arrives, and the bound instant dates the row's other
// writes.

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

// sweepHarness is the reconciler wired as production wires it, with the real
// settler. inv is in both stores.
func sweepHarness(inv domain.Invoice, client *mockStripeClient) (*Reconciler, *stampTimeUpdater, *recordingDunningStarter) {
	updater := &stampTimeUpdater{mockInvoiceUpdater: newMockInvoiceUpdater()}
	updater.invoices[inv.ID] = inv
	if inv.StripePaymentIntentID != "" {
		updater.byPI[inv.StripePaymentIntentID] = inv.ID
	}
	dunning := &recordingDunningStarter{}
	settler := NewStripe(client, updater, newMockWebhookStore(), nil, dunning)
	r := NewReconciler(client, newMockReconcileStore(inv), settler, time.Second)
	return r, updater, dunning
}

// A test-mode ctx with no instant bound: what the billing tick gives.
func tickCtx() context.Context { return postgres.WithLivemode(context.Background(), false) }

func unknownPaymentInvoice() domain.Invoice {
	inv := finalizedPendingInvoice()
	inv.PaymentStatus = domain.PaymentUnknown
	inv.StripePaymentIntentID = "pi_sweep"
	return inv
}

// A test-mode invoice is paid at the time Stripe's answer arrives, not the
// row's instant.
func TestSweep_AnInvoiceIsPaidWhenStripesAnswerArrives(t *testing.T) {
	inv := unknownPaymentInvoice()
	r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_sweep": {ID: "pi_sweep", Status: "succeeded", AmountReceivedCents: 5000},
	}})
	arrival := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC) // far from the row's instant, read from the machine
	r.settler.(*Stripe).SetClock(clock.NewFake(arrival))
	if _, runErrs := r.Run(tickCtx(), 10); len(runErrs) != 0 {
		t.Fatalf("errors %v; want none", runErrs)
	}
	if got := updater.invoices[inv.ID]; got.PaidAt == nil || !got.PaidAt.Equal(arrival) {
		t.Errorf("paid_at %v; want %v, when Stripe's answer arrived, not the row's instant", got.PaidAt, arrival)
	}
}

// A row is bound to one read of the server's clock, as an event is bound, so
// what the row's work stamps is that instant, not a later read each time.
func TestSweep_ARowIsBoundToOneInstant(t *testing.T) {
	if _, ok := clock.EffectiveNow(rowTime(tickCtx())); !ok {
		t.Error("the row's ctx carries no instant: each stamp would read the clock again")
	}
}
