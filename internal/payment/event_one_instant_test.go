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

// eventServer is the server's clock in these tests: a fixed time, counting
// the reads it answers itself (a bound instant is not a read).
type eventServer struct{ reads int }

var eventServerAt = time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)

func (c *eventServer) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	return eventServerAt
}

// An event binds one read of the server's clock in either mode, so every
// action it runs reads one instant.
func TestBindEvent_BindsOneInstant(t *testing.T) {
	for _, live := range []bool{false, true} {
		s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
		srv := &eventServer{}
		s.SetClock(srv)
		ctx := s.bindEvent(postgres.WithLivemode(context.Background(), live))
		if at, _ := clock.EffectiveNow(ctx); !at.Equal(eventServerAt) || srv.reads != 1 {
			t.Errorf("live %v: bound %v after %d server reads; want %v after one", live, at, srv.reads, eventServerAt)
		}
	}
}

// A card event's attach runs on the event's one instant, one read of the
// server's clock in either mode.
func TestHandleWebhook_ACardEventAttachesOnTheEventsInstant(t *testing.T) {
	for _, c := range []struct {
		name string
		live bool
	}{{"test mode", false}, {"live mode", true}} {
		t.Run(c.name, func(t *testing.T) {
			s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
			srv := &eventServer{}
			s.SetClock(srv)
			attacher := &recordingAttacher{}
			s.SetPaymentMethodAttacher(attacher)
			err := s.HandleWebhook(postgres.WithLivemode(context.Background(), c.live), "t1",
				domain.StripeWebhookEvent{StripeEventID: "evt_seti_one", EventType: "setup_intent.succeeded", Payload: map[string]any{"raw": setupIntentRaw}})
			if err != nil || attacher.called != 1 {
				t.Fatalf("err %v, attached %d times; want one attach", err, attacher.called)
			}
			if !attacher.at.Equal(eventServerAt) || srv.reads != 1 {
				t.Errorf("attached at %v after %d server reads; want %v after one", attacher.at, srv.reads, eventServerAt)
			}
		})
	}
}

// instantSetups records the instant the payment-setup write ran on: the write
// that stamps customers.updated_at (customer/postgres.go, from ctx's time).
type instantSetups struct {
	nopSetups
	at     time.Time
	writes int
}

func (s *instantSetups) UpsertPaymentSetupAudited(ctx context.Context, _ string, ps domain.CustomerPaymentSetup, _ func(*sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	s.writes++
	s.at, _ = clock.EffectiveNow(ctx)
	return ps, nil
}

// A checkout.session.completed event writes the customer's payment setup
// (customers.updated_at) on the event's one instant, one read of the server's
// clock in either mode. The card's Stripe calls run between the bind and the
// write, so the write's time is the bind's, not the write's own.
func TestHandleWebhook_ACheckoutEventWritesTheCustomerOnTheEventsInstant(t *testing.T) {
	for _, c := range []struct {
		name string
		live bool
	}{{"test mode", false}, {"live mode", true}} {
		t.Run(c.name, func(t *testing.T) {
			setups := &instantSetups{}
			s := NewStripe(nil, nil, newMockWebhookStore(), setups)
			srv := &eventServer{}
			s.SetClock(srv)
			s.SetCardFetcher(fixedCardFetcher{card: CardDetails{PaymentMethodID: "pm_saved", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2034}})
			s.SetPaymentMethodAttacher(&recordingAttacher{})
			raw := `{"id": "evt_cs_one", "type": "checkout.session.completed", "data": {"object": {
				"id": "cs_one", "mode": "setup", "customer": "cus_stripe_1",
				"metadata": {"hardcount_customer_id": "cus_local_1", "hardcount_purpose": "portal_add_payment_method"}}}}`
			if err := s.HandleWebhook(postgres.WithLivemode(context.Background(), c.live), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_cs_one", EventType: "checkout.session.completed", Payload: map[string]any{"raw": raw},
			}); err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if setups.writes != 1 || !setups.at.Equal(eventServerAt) || srv.reads != 1 {
				t.Errorf("%d setup writes, at %v after %d server reads; want one at %v after one",
					setups.writes, setups.at, srv.reads, eventServerAt)
			}
		})
	}
}
