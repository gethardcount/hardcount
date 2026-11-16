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

// An event decides its time from the record it names and binds it, so every
// action the event runs reads one instant: the record's clock and its frozen
// time, with no read of the server's clock; for a record on no clock, a
// not-found under a customer, and in live mode, one read of the server's
// clock. A clock that cannot be read binds nothing, and the event is
// delivered again.
func TestDecideForAnEvent_BindsOneInstant(t *testing.T) {
	testMode := postgres.WithLivemode(context.Background(), false)
	live := postgres.WithLivemode(context.Background(), true)
	decides := []struct {
		name   string
		decide func(s *Stripe, ctx context.Context) (context.Context, error)
	}{
		{"decideFor (an invoice)", func(s *Stripe, ctx context.Context) (context.Context, error) {
			return s.decideFor(ctx, clock.Pin{TenantID: "t1", InvoiceID: "inv_1"})
		}},
		{"decideForCustomer", func(s *Stripe, ctx context.Context) (context.Context, error) {
			return s.decideForCustomer(ctx, "t1", "cus_1")
		}},
	}
	for _, d := range decides {
		for _, c := range []struct {
			name            string
			ctx             context.Context
			res             *eventResolver
			wantAt          time.Time
			wantClock       string
			wantServerReads int
			wantResolver    int
			wantErr         bool
		}{
			{"a record on a clock", testMode, &eventResolver{}, eventFrozen, "clk_e", 0, 1, false},
			{"a record on no clock", testMode, &eventResolver{noClock: true}, eventServerAt, "", 1, 1, false},
			{"live mode", live, &eventResolver{err: errors.New("must not be read")}, eventServerAt, "", 1, 0, false},
			{"an unreadable clock", testMode, &eventResolver{err: errors.New("connection reset by peer")}, time.Time{}, "", 0, 1, true},
		} {
			t.Run(d.name+": "+c.name, func(t *testing.T) {
				s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
				srv := &eventServer{}
				s.SetClock(srv)
				s.SetResolver(c.res)
				ctx, err := d.decide(s, c.ctx)
				if c.wantErr != (err != nil) {
					t.Fatalf("err %v; want error %v", err, c.wantErr)
				}
				at, _ := clock.EffectiveNow(ctx)
				sim, _ := clock.SimOf(ctx)
				if !at.Equal(c.wantAt) || sim.TestClockID != c.wantClock || srv.reads != c.wantServerReads || c.res.reads != c.wantResolver {
					t.Errorf("bound %v on clock %q after %d server and %d resolver reads; want %v on %q after %d and %d",
						at, sim.TestClockID, srv.reads, c.res.reads, c.wantAt, c.wantClock, c.wantServerReads, c.wantResolver)
				}
			})
		}
	}
	t.Run("decideForCustomer: a not-found is one read of the server's clock", func(t *testing.T) {
		s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
		srv := &eventServer{}
		s.SetClock(srv)
		s.SetResolver(&eventResolver{err: fmt.Errorf("could not read customer cus_1: %w", errs.ErrNotFound)})
		ctx, err := s.decideForCustomer(testMode, "t1", "cus_1")
		if at, _ := clock.EffectiveNow(ctx); err != nil || !at.Equal(eventServerAt) || srv.reads != 1 {
			t.Errorf("err %v, bound %v after %d server reads; want %v after one", err, at, srv.reads, eventServerAt)
		}
	})
}

// A card event's attach runs on the event's one instant: a customer on a
// clock at its clock's time, with no read of the server's clock; a customer
// on no clock, and any customer in live mode, at one read of the server's
// clock.
func TestHandleWebhook_ACardEventAttachesOnTheEventsInstant(t *testing.T) {
	for _, c := range []struct {
		name            string
		live            bool
		res             *eventResolver
		wantAt          time.Time
		wantServerReads int
	}{
		{"a customer on a clock", false, &eventResolver{}, eventFrozen, 0},
		{"a customer on no clock", false, &eventResolver{noClock: true}, eventServerAt, 1},
		{"live mode", true, &eventResolver{err: errors.New("must not be read")}, eventServerAt, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
			srv := &eventServer{}
			s.SetClock(srv)
			attacher := &recordingAttacher{}
			s.SetPaymentMethodAttacher(attacher)
			s.SetResolver(c.res)
			err := s.HandleWebhook(postgres.WithLivemode(context.Background(), c.live), "t1",
				domain.StripeWebhookEvent{StripeEventID: "evt_seti_one", EventType: "setup_intent.succeeded", Payload: map[string]any{"raw": setupIntentRaw}})
			if err != nil || attacher.called != 1 {
				t.Fatalf("err %v, attached %d times; want one attach", err, attacher.called)
			}
			if !attacher.at.Equal(c.wantAt) || srv.reads != c.wantServerReads {
				t.Errorf("attached at %v after %d server reads; want %v after %d", attacher.at, srv.reads, c.wantAt, c.wantServerReads)
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
// (customers.updated_at) on the event's one instant, bound at its decision:
// a customer on a clock at its clock's time, with no read of the server's
// clock; a customer on no clock, and any customer in live mode, at one read
// of the server's clock. The card's Stripe calls run between the decision and
// the write, so the write's time is the decision's, not the write's own.
func TestHandleWebhook_ACheckoutEventWritesTheCustomerOnTheEventsInstant(t *testing.T) {
	for _, c := range []struct {
		name            string
		live            bool
		res             *eventResolver
		wantAt          time.Time
		wantServerReads int
	}{
		{"a customer on a clock", false, &eventResolver{}, eventFrozen, 0},
		{"a customer on no clock", false, &eventResolver{noClock: true}, eventServerAt, 1},
		{"live mode", true, &eventResolver{err: errors.New("must not be read")}, eventServerAt, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			setups := &instantSetups{}
			s := NewStripe(nil, nil, newMockWebhookStore(), setups)
			srv := &eventServer{}
			s.SetClock(srv)
			s.SetResolver(c.res)
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
			if setups.writes != 1 || !setups.at.Equal(c.wantAt) || srv.reads != c.wantServerReads {
				t.Errorf("%d setup writes, at %v after %d server reads; want one at %v after %d",
					setups.writes, setups.at, srv.reads, c.wantAt, c.wantServerReads)
			}
		})
	}
}
