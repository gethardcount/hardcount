package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// latestCardFetcher answers like Stripe after a hosted payment saved a card
// (setup_future_usage, ADR-021): the newest card on the Stripe customer.
type latestCardFetcher struct{ card payment.CardDetails }

func (f *latestCardFetcher) FetchCardDetails(context.Context, string) (payment.CardDetails, error) {
	return f.card, nil
}
func (f *latestCardFetcher) FetchCardForPaymentIntent(context.Context, string) (payment.CardDetails, error) {
	return f.card, nil
}

// pmCardStripe is the payment-methods side's Stripe: only card metadata is used.
type pmCardStripe struct{ last4, fingerprint string }

func (s *pmCardStripe) CreateSetupIntent(context.Context, string, map[string]string) (string, string, error) {
	return "", "", nil
}
func (s *pmCardStripe) CreateSetupCheckoutSession(context.Context, string, string, string, map[string]string) (string, string, error) {
	return "", "", nil
}
func (s *pmCardStripe) EnsureStripeCustomer(context.Context, string, string) (string, error) {
	return "", nil
}
func (s *pmCardStripe) DetachPaymentMethod(context.Context, string) error { return nil }
func (s *pmCardStripe) FetchPaymentMethodCard(context.Context, string) (paymentmethods.CardMetadata, error) {
	return paymentmethods.CardMetadata{Brand: "visa", Last4: s.last4, ExpMonth: 12, ExpYear: 2034, Fingerprint: s.fingerprint}, nil
}
func (s *pmCardStripe) SetDefaultPaymentMethod(context.Context, string, string, string) error {
	return nil
}

// A hosted payment's checkout.session.completed records the card the
// customer paid with in payment_methods — the table charges read. Since
// migration 0097 the webhook only wrote the customer↔Stripe mapping, so the
// card was never recorded (Stripe held two saved 4242s for a customer whose
// Velox record had only the declining 0341, 2026-10-11). Production wiring:
// the composite setup store + the real payment-methods service.
//
// A customer with no card gets it as the default (ADR-021: the next invoice
// auto-charges it). A customer with a default keeps it; the new card is
// added beside it, as Add card does.
func TestCheckoutCompleted_RecordsTheCard_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Hosted Card Record")
	customers := customer.NewPostgresStore(db)
	pmStore := paymentmethods.NewPostgresStore(db)
	pmStripe := &pmCardStripe{}
	pmSvc := paymentmethods.NewService(pmStore, pmStripe, nil)

	fetcher := &latestCardFetcher{}
	stripe := payment.NewStripe(nil, nil, payment.NewPostgresWebhookStore(db), &compositePaymentSetupStore{customers: customers, pms: pmSvc})
	stripe.SetCardFetcher(fetcher)
	stripe.SetPaymentMethodAttacher(pmSvc)

	webhook := func(t *testing.T, custID, stripeCustID, evtID string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"id": evtID, "type": "checkout.session.completed", "data": map[string]any{"object": map[string]any{
			"id": "cs_" + evtID, "customer": stripeCustID,
			"metadata": map[string]string{"velox_customer_id": custID, "velox_purpose": payment.PurposeHostedInvoicePay},
		}}})
		if err := stripe.HandleWebhook(ctx, tenantID, domain.StripeWebhookEvent{
			StripeEventID: evtID, EventType: "checkout.session.completed", Payload: map[string]any{"raw": string(body)},
		}); err != nil {
			t.Fatalf("webhook: %v", err)
		}
	}
	defaultCard := func(t *testing.T, custID string) (string, int) {
		t.Helper()
		pms, err := pmSvc.List(ctx, tenantID, custID)
		if err != nil {
			t.Fatalf("list cards: %v", err)
		}
		def := ""
		for _, pm := range pms {
			if pm.IsDefault {
				def = pm.StripePaymentMethodID
			}
		}
		return def, len(pms)
	}

	t.Run("no card on file: the paid card becomes the default", func(t *testing.T) {
		c, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_hosted_new", DisplayName: "New"})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		fetcher.card = payment.CardDetails{PaymentMethodID: "pm_hosted_4242", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2034}
		pmStripe.last4, pmStripe.fingerprint = "4242", "fp_4242"
		webhook(t, c.ID, "cus_stripe_new", "evt_new")
		if def, n := defaultCard(t, c.ID); def != "pm_hosted_4242" || n != 1 {
			t.Fatalf("cards = %d, default %q; want the hosted card recorded as the default", n, def)
		}
	})

	t.Run("a default card on file stays the default; the paid card is added", func(t *testing.T) {
		c, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_hosted_old", DisplayName: "Old"})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		pmStripe.last4, pmStripe.fingerprint = "0341", "fp_0341"
		if _, err := pmSvc.AttachFromSetupIntent(ctx, tenantID, c.ID, "pm_old_0341"); err != nil {
			t.Fatalf("seed old card: %v", err)
		}
		fetcher.card = payment.CardDetails{PaymentMethodID: "pm_hosted_4242b", Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2034}
		pmStripe.last4, pmStripe.fingerprint = "4242", "fp_4242b"
		webhook(t, c.ID, "cus_stripe_old", "evt_old")
		if def, n := defaultCard(t, c.ID); def != "pm_old_0341" || n != 2 {
			t.Fatalf("cards = %d, default %q; want both cards, the old one still default", n, def)
		}
	})
}
