package payment

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

var eventFrozen = time.Date(2027, 1, 20, 8, 0, 0, 0, time.UTC)

// eventResolver puts every record on clock clk_e at eventFrozen, or fails
// with err, and counts the reads.
type eventResolver struct {
	reads int
	err   error
}

func (r *eventResolver) answer() (clock.Sim, error) {
	r.reads++
	if r.err != nil {
		return clock.Sim{}, r.err
	}
	return clock.Sim{At: eventFrozen, TestClockID: "clk_e"}, nil
}
func (r *eventResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *eventResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}
func (r *eventResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return r.answer()
}

func processingInvoice() (*mockInvoiceUpdater, *mockWebhookStore) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, StripePaymentIntentID: "pi_ev",
	}
	invoices.byPI["pi_ev"] = "inv_1"
	return invoices, newMockWebhookStore()
}

var succeeded = domain.StripeWebhookEvent{StripeEventID: "evt_ev", EventType: "payment_intent.succeeded", PaymentIntentID: "pi_ev"}

// A test-mode payment event decides its time from the invoice it names: a
// clock invoice is paid at its clock's time.
func TestHandleWebhook_AClockInvoicesPaymentIsRecordedAtItsClockTime(t *testing.T) {
	invoices, webhooks := processingInvoice()
	res := &eventResolver{}
	s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
	s.SetResolver(res)
	if err := s.HandleWebhook(postgres.WithLivemode(context.Background(), false), "t1", succeeded); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if got := invoices.invoices["inv_1"]; got.PaidAt == nil || !got.PaidAt.Equal(eventFrozen) {
		t.Errorf("paid_at %v, want the clock's %v", got.PaidAt, eventFrozen)
	}
}

const setupIntentRaw = `{"id": "evt_seti_ev", "type": "setup_intent.succeeded", "data": {"object": {
	"id": "seti_ev", "payment_method": "pm_ev", "customer": "cus_stripe_ev",
	"metadata": {"hardcount_tenant_id": "t1", "hardcount_customer_id": "cus_ev"}}}}`

const checkoutRaw = `{"id": "evt_cs_ev", "type": "checkout.session.completed", "data": {"object": {
	"id": "cs_ev", "customer": "cus_stripe_ev",
	"metadata": {"hardcount_tenant_id": "t1", "hardcount_customer_id": "cus_ev"}}}}`

// An event whose record's clock cannot be read is refused as transient: the
// handler answers 5xx, the event is not recorded as processed, so Stripe's
// redelivery is not dropped as a duplicate, and nothing is done at a time
// nobody decided. For an invoice event that holds even with a not-found
// underneath: the invoice was just read. A customer event's customer was not
// read, so its not-found is real time (the next test).
func TestHandleWebhook_AnUndecidableTimeIsDeliveredAgain(t *testing.T) {
	clockGone := fmt.Errorf("read test clock clk_e: %w", errs.ErrNotFound)
	dbDown := errors.New("connection reset by peer")
	for _, c := range []struct {
		event domain.StripeWebhookEvent
		cause error
	}{
		{succeeded, clockGone},
		{domain.StripeWebhookEvent{StripeEventID: "evt_ev_failed", EventType: "payment_intent.payment_failed", PaymentIntentID: "pi_ev", FailureMessage: "card declined"}, clockGone},
		{domain.StripeWebhookEvent{StripeEventID: "evt_seti_ev", EventType: "setup_intent.succeeded", Payload: map[string]any{"raw": setupIntentRaw}}, dbDown},
		{domain.StripeWebhookEvent{StripeEventID: "evt_cs_ev", EventType: "checkout.session.completed", Payload: map[string]any{"raw": checkoutRaw}}, dbDown},
	} {
		event := c.event
		t.Run(event.EventType, func(t *testing.T) {
			invoices, webhooks := processingInvoice()
			s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil, &recordingDunningStarter{})
			attacher := &recordingAttacher{}
			s.SetPaymentMethodAttacher(attacher)
			s.SetResolver(&eventResolver{err: c.cause})

			err := s.HandleWebhook(postgres.WithLivemode(context.Background(), false), "t1", event)
			if err == nil || !errors.Is(err, clock.ErrUndecided) || !isTransientWebhookError(err) {
				t.Fatalf("err = %v; want clock.ErrUndecided, transient", err)
			}
			if seen, _ := webhooks.WasProcessed(context.Background(), "t1", event.StripeEventID); seen || webhooks.ingestCalls != 0 {
				t.Errorf("the refused event was recorded as processed (ingest calls %d): its redelivery would be dropped", webhooks.ingestCalls)
			}
			if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentProcessing || got.PaidAt != nil || attacher.called != 0 {
				t.Errorf("invoice moved to %s (paid_at %v), card attached %d times; want nothing done", got.PaymentStatus, got.PaidAt, attacher.called)
			}
		})
	}
}

// A customer event names a customer it has not read: one the resolver does
// not find gets real time, and the handler's own path answers it as before.
func TestHandleWebhook_ACustomerEventForAnUnknownCustomerGoesOnAtRealTime(t *testing.T) {
	s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	attacher := &recordingAttacher{}
	s.SetPaymentMethodAttacher(attacher)
	s.SetResolver(&eventResolver{err: fmt.Errorf("read customer cus_ev: %w", errs.ErrNotFound)})
	err := s.HandleWebhook(postgres.WithLivemode(context.Background(), false), "t1",
		domain.StripeWebhookEvent{StripeEventID: "evt_seti_ev", EventType: "setup_intent.succeeded", Payload: map[string]any{"raw": setupIntentRaw}})
	if err != nil || attacher.called != 1 {
		t.Errorf("err %v, attached %d times; want the attach to go on", err, attacher.called)
	}
}

// In live mode no record is on a test clock: the event decides nothing, so
// a clock that cannot be read never refuses a live payment.
func TestHandleWebhook_ALiveEventDecidesNothing(t *testing.T) {
	invoices, webhooks := processingInvoice()
	s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
	s.SetResolver(&eventResolver{err: errors.New("connection reset by peer")})
	if err := s.HandleWebhook(postgres.WithLivemode(context.Background(), true), "t1", succeeded); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if got := invoices.invoices["inv_1"]; got.Status != domain.InvoicePaid {
		t.Errorf("status %s; want paid", got.Status)
	}
}
