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

// In live mode no record is on a test clock, so no settle reads one: a live
// payment must never wait on a clock read, nor be refused when one fails.
// Not a failed payment's event (a paid one is
// TestHandleWebhook_ALiveEventDecidesNothing), not the reconciler's sweeps,
// and not a charge settled inline.
func TestSettle_LiveReadsNoClock(t *testing.T) {
	live := func() context.Context { return postgres.WithLivemode(context.Background(), true) }

	t.Run("the hosted-session sweep, paid", func(t *testing.T) {
		res := &eventResolver{}
		inv := finalizedPendingInvoice()
		r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
			"pi_hosted": {ID: "pi_hosted", Status: "succeeded", AmountReceivedCents: inv.AmountDueCents},
		}})
		r.SetResolver(res)
		r.settler.(*Stripe).SetResolver(res)
		r.SetHostedSessions(
			&fakeHostedLedger{rows: []CheckoutClaim{{ID: "cks_1", TenantID: "t1", InvoiceID: inv.ID, StripeSessionID: "cs_1", Status: "completed"}}},
			fakeSessionReader{"cs_1": {Status: "complete", PaymentStatus: "paid", PaymentIntentID: "pi_hosted"}},
			nil)
		if _, errs := r.sweepHostedSessions(live(), 50, new(scheduler.Panics)); len(errs) != 0 {
			t.Fatalf("errors: %v", errs)
		}
		if got := updater.invoices[inv.ID]; got.Status != domain.InvoicePaid || res.reads != 0 {
			t.Errorf("status %s, clock reads %d; want paid, none", got.Status, res.reads)
		}
	})

	t.Run("a failed payment's event", func(t *testing.T) {
		res := &eventResolver{}
		invoices, webhooks := processingInvoice()
		s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil, &recordingDunningStarter{})
		s.SetResolver(res)
		failed := domain.StripeWebhookEvent{StripeEventID: "evt_ev_failed", EventType: "payment_intent.payment_failed", PaymentIntentID: "pi_ev"}
		if err := s.HandleWebhook(live(), "t1", failed); err != nil {
			t.Fatalf("HandleWebhook: %v", err)
		}
		if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentFailed || res.reads != 0 {
			t.Errorf("payment status %s, clock reads %d; want failed, none", got.PaymentStatus, res.reads)
		}
	})

	for _, c := range []struct {
		status string
		want   domain.InvoicePaymentStatus
	}{{"succeeded", domain.PaymentSucceeded}, {"canceled", domain.PaymentFailed}} {
		t.Run("the reconciler's sweep, "+c.status, func(t *testing.T) {
			res := &eventResolver{}
			inv := unknownClockInvoice()
			r, updater, _ := sweepHarness(inv, &mockStripeClient{piStates: map[string]PaymentIntentResult{
				"pi_sweep": {ID: "pi_sweep", Status: c.status, AmountReceivedCents: inv.AmountDueCents, Charged: true},
			}})
			r.SetResolver(res)
			r.settler.(*Stripe).SetResolver(res)
			if _, errs := r.Run(live(), 10); len(errs) != 0 {
				t.Fatalf("errors: %v", errs)
			}
			if got := updater.invoices[inv.ID]; got.PaymentStatus != c.want || res.reads != 0 {
				t.Errorf("payment status %s, clock reads %d; want %s, none", got.PaymentStatus, res.reads, c.want)
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
			res := &eventResolver{}
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = finalizedPendingInvoice()
			s := NewStripe(c.client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
			s.SetResolver(res)
			_, _ = s.ChargeInvoice(live(), "t1", invoices.invoices["inv_1"], testHold, "cus_stripe_abc", "pm_test")
			if got := invoices.invoices["inv_1"]; got.PaymentStatus != c.want || res.reads != 0 {
				t.Errorf("payment status %s, clock reads %d; want %s, none", got.PaymentStatus, res.reads, c.want)
			}
		})
	}
}

// With no instant decided (live, or real time) a settle reads real time from
// the server's clock, the one the router passes and the engine bills by, not
// the machine's: paid_at, the failed stamp and dunning's start are at the
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
		inv := unknownClockInvoice()
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
