package payment

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/scheduler"
)

// A live payment settles on every path: a payment's event, paid or failed,
// the reconciler's sweeps, and a charge settled inline.
func TestSettle_ALivePaymentSettlesOnEveryPath(t *testing.T) {
	live := func() context.Context { return postgres.WithLivemode(context.Background(), true) }

	t.Run("the hosted-session sweep, paid", func(t *testing.T) {
		inv := finalizedPendingInvoice()
		r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
			"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: inv.AmountDueCents},
		}})
		r.SetHostedSessions(
			&fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", InvoiceID: inv.ID, StripeSessionID: "cs_1", Status: "completed"}}},
			fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}},
			nil)
		if _, errs := r.sweepHostedSessions(live(), 50, new(scheduler.Panics)); len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		if got := updater.invoices[inv.ID]; got.Status != domain.InvoicePaid {
			t.Errorf("status %s; want paid", got.Status)
		}
	})

	for _, c := range []struct {
		event domain.StripeWebhookEvent
		want  domain.InvoicePaymentStatus
	}{
		{succeeded, domain.PaymentSucceeded},
		{domain.StripeWebhookEvent{StripeEventID: "evt_ev_failed", EventType: "payment_intent.payment_failed", PaymentIntentID: "pi_ev"}, domain.PaymentFailed},
	} {
		t.Run("a payment's event, "+c.event.EventType, func(t *testing.T) {
			invoices, webhooks := processingInvoice()
			s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil, &recordingDunningStarter{})
			if err := s.HandleWebhook(live(), "t1", c.event); err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if got := invoices.invoices["inv_1"]; got.PaymentStatus != c.want {
				t.Errorf("payment status %s; want %s", got.PaymentStatus, c.want)
			}
		})
	}

	for _, c := range []struct {
		status string
		want   domain.InvoicePaymentStatus
	}{{"succeeded", domain.PaymentSucceeded}, {"canceled", domain.PaymentFailed}} {
		t.Run("the reconciler's sweep, "+c.status, func(t *testing.T) {
			inv := unknownPaymentInvoice()
			r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
				"pi_sweep": {ID: "pi_sweep", Status: c.status, AmountReceivedCents: inv.AmountDueCents, Charged: true},
			}})
			if _, errs := r.Run(live(), 10); len(errs) != 0 {
				t.Fatalf("errors: %v", errs)
			}
			if got := updater.invoices[inv.ID]; got.PaymentStatus != c.want {
				t.Errorf("payment status %s; want %s", got.PaymentStatus, c.want)
			}
		})
	}

	for _, c := range []struct {
		name   string
		client *mockStripeClient
		want   domain.InvoicePaymentStatus
	}{
		{"a charge paid inline", &mockStripeClient{piID: "pi_ok", chargeStatus: "succeeded"}, domain.PaymentSucceeded},
		{"a charge declined inline", &mockStripeClient{shouldFail: true}, domain.PaymentFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = finalizedPendingInvoice()
			s := NewStripe(c.client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
			_, _ = s.ChargeInvoice(live(), "t1", invoices.invoices["inv_1"], testHold, "cus_stripe_abc", "pm_test")
			if got := invoices.invoices["inv_1"]; got.PaymentStatus != c.want {
				t.Errorf("payment status %s; want %s", got.PaymentStatus, c.want)
			}
		})
	}
}

// A settle reads its time from the server's clock (clock.AtArrival), the one
// the router passes and the engine bills by, not the machine's: paid_at, the failed stamp and dunning's start are at the
// server's now.
func TestSettle_RealTimeIsTheServersClock(t *testing.T) {
	serverNow := time.Date(2025, 6, 3, 10, 0, 0, 0, time.UTC)
	live := func() context.Context { return postgres.WithLivemode(context.Background(), true) }

	t.Run("paid", func(t *testing.T) {
		invoices, webhooks := processingInvoice()
		s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
		s.SetClock(clock.NewFake(serverNow))
		if err := s.SettleSucceeded(live(), "t1", invoices.invoices["inv_1"], "pi_ev", 0, SourceWebhook); err != nil {
			t.Fatalf("SettleSucceeded: %v", err)
		}
		if got := invoices.invoices["inv_1"]; got.PaidAt == nil || !got.PaidAt.Equal(serverNow) {
			t.Errorf("paid_at %v; want the server's %v", got.PaidAt, serverNow)
		}
	})

	t.Run("failed", func(t *testing.T) {
		inv := unknownPaymentInvoice()
		updater := &stampTimeUpdater{mockInvoiceUpdater: newMockInvoiceUpdater()}
		updater.invoices[inv.ID] = inv
		dunning := &recordingDunningStarter{}
		s := NewStripe(&mockStripeClient{}, updater, newMockWebhookStore(), nil, dunning)
		s.SetClock(clock.NewFake(serverNow))
		if err := s.SettleFailed(live(), "t1", inv, PaymentFailure{PaymentIntentID: "pi_sweep", Message: "card declined"}, SourceWebhook); err != nil {
			t.Fatalf("SettleFailed: %v", err)
		}
		if !updater.failedAt.Equal(serverNow) {
			t.Errorf("failed stamp at %v; want the server's %v", updater.failedAt, serverNow)
		}
		if len(dunning.calls) != 1 || !dunning.calls[0].at.Equal(serverNow) {
			t.Errorf("dunning starts %+v; want one at the server's %v", dunning.calls, serverNow)
		}
	})
}
