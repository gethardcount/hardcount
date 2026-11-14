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

// eventResolver puts every record on clock clk_e at eventFrozen (or, with
// noClock, on no clock: the machine's time, as the engine answers), or fails
// with err, and counts the reads.
type eventResolver struct {
	reads   int
	err     error
	noClock bool
}

func (r *eventResolver) answer() (clock.Sim, error) {
	r.reads++
	switch {
	case r.err != nil:
		return clock.Sim{}, r.err
	case r.noClock:
		return clock.Sim{At: time.Now()}, nil
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
// clock invoice is paid at its clock's time, and an invoice on no clock at
// the machine's time, as on main. The charge's anchor, when the event
// carries one, still wins inside the settle. All three pass on main too:
// they guard what the decision must not change.
func TestHandleWebhook_APaymentIsDatedAsTheInvoiceDecides(t *testing.T) {
	anchor := eventFrozen.Add(-36 * time.Hour)
	withAnchor := succeeded
	withAnchor.Payload = map[string]any{"raw": `{"data":{"object":{"metadata":{"hardcount_anchor_at":"` + anchor.Format(time.RFC3339Nano) + `"}}}}`}
	for _, c := range []struct {
		name  string
		res   *eventResolver
		event domain.StripeWebhookEvent
		want  func(time.Time) bool
	}{
		{"a clock invoice: the clock's time", &eventResolver{}, succeeded, func(at time.Time) bool { return at.Equal(eventFrozen) }},
		{"an invoice on no clock: the machine's time", &eventResolver{noClock: true}, succeeded, func(at time.Time) bool { return time.Since(at) < time.Minute }},
		{"the charge's anchor beats the decision", &eventResolver{}, withAnchor, func(at time.Time) bool { return at.Equal(anchor) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			invoices, webhooks := processingInvoice()
			s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
			s.SetResolver(c.res)
			if err := s.HandleWebhook(postgres.WithLivemode(context.Background(), false), "t1", c.event); err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if got := invoices.invoices["inv_1"]; got.PaidAt == nil || !c.want(*got.PaidAt) {
				t.Errorf("paid_at %v", got.PaidAt)
			}
		})
	}
}

// An event with no mode on its ctx is a fault (the webhook handler and the
// restore catch-up both set one): it is refused, not decided as test mode
// or as live.
func TestHandleWebhook_AnEventWithNoModeIsRefused(t *testing.T) {
	invoices, webhooks := processingInvoice()
	res := &eventResolver{}
	s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
	s.SetResolver(res)
	err := s.HandleWebhook(context.Background(), "t1", succeeded)
	if !errors.Is(err, clock.ErrUndecided) || webhooks.ingestCalls != 0 || res.reads != 0 {
		t.Fatalf("err %v, ingest calls %d, clock reads %d; want clock.ErrUndecided, nothing recorded, no read", err, webhooks.ingestCalls, res.reads)
	}
	if got := invoices.invoices["inv_1"]; got.PaymentStatus != domain.PaymentProcessing {
		t.Errorf("invoice moved to %s; want it untouched", got.PaymentStatus)
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

// A customer event names a customer it has not read. Any not-found goes on
// at real time, by design, and the handler's own path answers the event as
// before: a customer that does not exist, or one whose clock was not found,
// which is only a race with the clock's deletion (customers.test_clock_id is
// ON DELETE SET NULL), after which the customer is on no clock.
func TestHandleWebhook_ACustomerEventWithANotFoundGoesOnAtRealTime(t *testing.T) {
	for _, cause := range []error{
		fmt.Errorf("could not read customer cus_ev to find its test clock: %w", errs.ErrNotFound),
		fmt.Errorf("could not read test clock clk_e of customer cus_ev: %w", errs.ErrNotFound),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
			attacher := &recordingAttacher{}
			s.SetPaymentMethodAttacher(attacher)
			s.SetResolver(&eventResolver{err: cause})
			err := s.HandleWebhook(postgres.WithLivemode(context.Background(), false), "t1",
				domain.StripeWebhookEvent{StripeEventID: "evt_seti_ev", EventType: "setup_intent.succeeded", Payload: map[string]any{"raw": setupIntentRaw}})
			if err != nil || attacher.called != 1 {
				t.Errorf("err %v, attached %d times; want the attach to go on", err, attacher.called)
			}
		})
	}
}

// In live mode no record is on a test clock: the decide step reads no clock,
// so a clock that cannot be read never refuses a live event. A card attach
// reads none at all. A payment reads one, the settle's own bind, which every
// settle caller has and which the engine answers in live mode without a
// lookup.
func TestHandleWebhook_ALiveEventDecidesNothing(t *testing.T) {
	invoices, webhooks := processingInvoice()
	payRes := &eventResolver{err: errors.New("connection reset by peer")}
	s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
	s.SetResolver(payRes)
	live := postgres.WithLivemode(context.Background(), true)
	if err := s.HandleWebhook(live, "t1", succeeded); err != nil {
		t.Fatalf("payment: %v", err)
	}
	if got := invoices.invoices["inv_1"]; got.Status != domain.InvoicePaid || payRes.reads != 1 {
		t.Errorf("payment: status %s, clock reads %d; want paid, one read (the settle's bind)", got.Status, payRes.reads)
	}

	cardRes := &eventResolver{err: errors.New("connection reset by peer")}
	attacher := &recordingAttacher{}
	card := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	card.SetPaymentMethodAttacher(attacher)
	card.SetResolver(cardRes)
	if err := card.HandleWebhook(live, "t1", domain.StripeWebhookEvent{StripeEventID: "evt_seti_ev", EventType: "setup_intent.succeeded", Payload: map[string]any{"raw": setupIntentRaw}}); err != nil {
		t.Fatalf("card: %v", err)
	}
	if attacher.called != 1 || cardRes.reads != 0 {
		t.Errorf("card: attached %d times, clock reads %d; want attached, no read", attacher.called, cardRes.reads)
	}
}

// A refund event whose recorder could not decide its time is delivered
// again, even for an old refund and with a not-found underneath: it is not
// "no credit note holds this refund" (acknowledged for an old refund) nor
// "no invoice holds this payment" (recorded as unmatched).
func TestHandleWebhook_ARefundWhoseTimeIsUndecidedIsDeliveredAgain(t *testing.T) {
	undecided := fmt.Errorf("refund re_x: %w: %w", clock.ErrUndecided, fmt.Errorf("read test clock clk_e: %w", errs.ErrNotFound))
	for _, c := range []struct {
		name string
		obj  map[string]any
	}{
		// hardcount_cn_id: Hardcount's own refund, applied to its credit note.
		{"a Hardcount refund's status", dashboardRefund(1000, map[string]string{"hardcount_cn_id": "cn_1"})},
		// No metadata: a refund made in Stripe, recorded against its invoice.
		{"a refund made in Stripe", dashboardRefund(1000, nil)},
	} {
		t.Run(c.name, func(t *testing.T) {
			updater := &fakeRefundUpdater{err: undecided}
			external := &fakeExternalRefunds{err: undecided}
			s := refundAdapter(updater, external)
			webhooks := s.webhooks.(*mockWebhookStore)
			event := domain.StripeWebhookEvent{StripeEventID: "evt_re_undecided", EventType: "refund.updated",
				Payload: map[string]any{"raw": refundRaw(t, c.obj)}}
			err := s.HandleWebhook(postgres.WithLivemode(context.Background(), false), "t1", event)
			if err == nil || !isTransientWebhookError(err) {
				t.Fatalf("err = %v; want a transient error, so Stripe delivers it again", err)
			}
			if webhooks.ingestCalls != 0 {
				t.Errorf("the refused refund event was recorded as processed (%d ingest calls)", webhooks.ingestCalls)
			}
		})
	}
}
