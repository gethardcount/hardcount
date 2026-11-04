package paymentmethods

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// returnStripe counts the Stripe calls a setup session makes and keeps the
// return URLs it was given.
type returnStripe struct {
	ensures, sessions     int
	successURL, cancelURL string
}

func (f *returnStripe) CreateSetupCheckoutSession(_ context.Context, _, successURL, cancelURL string, _ map[string]string) (string, string, error) {
	f.sessions++
	f.successURL, f.cancelURL = successURL, cancelURL
	return "https://checkout.stripe.com/fake", "cs_fake", nil
}
func (f *returnStripe) EnsureStripeCustomer(context.Context, string, string) (string, error) {
	f.ensures++
	return "cus_fake", nil
}
func (f *returnStripe) DetachPaymentMethod(context.Context, string) error { return nil }
func (f *returnStripe) FetchPaymentMethodCard(context.Context, string) (CardMetadata, error) {
	return CardMetadata{}, errors.New("not used")
}
func (f *returnStripe) SetDefaultPaymentMethod(context.Context, string, string, string) error {
	return nil
}

// With no return_url and no CUSTOMER_PORTAL_URL, Stripe would have no page
// to send the customer back to. The session is refused with a stable code,
// before any Stripe call: no Stripe customer is created for nothing. It used
// to return the customer to http://localhost:5173.
func TestCreateSetupSession_NoReturnAddress_Refused(t *testing.T) {
	stripe := &returnStripe{}
	svc := NewService(newMemStore(), stripe, nil)
	ctx := postgres.WithLivemode(context.Background(), false)

	_, _, err := svc.CreateSetupSession(ctx, "ten_1", "cus_1", "")
	if errs.Code(err) != errs.CodeReturnAddressNotSet {
		t.Fatalf("err = %v (code %q), want code %q", err, errs.Code(err), errs.CodeReturnAddressNotSet)
	}
	if stripe.ensures != 0 || stripe.sessions != 0 {
		t.Errorf("Stripe calls: ensure=%d session=%d, want none", stripe.ensures, stripe.sessions)
	}
}

// With CUSTOMER_PORTAL_URL set, the session returns the customer to the
// public success page on that address.
func TestCreateSetupSession_PortalAddress_Used(t *testing.T) {
	stripe := &returnStripe{}
	svc := NewService(newMemStore(), stripe, nil)
	svc.SetPortalBaseURL("https://app.example.test/")
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, _, err := svc.CreateSetupSession(ctx, "ten_1", "cus_1", ""); err != nil {
		t.Fatalf("CreateSetupSession: %v", err)
	}
	const page = "https://app.example.test/payment-method-added"
	if stripe.successURL != page+"?status=success" || stripe.cancelURL != page+"?status=cancel" {
		t.Errorf("success=%q cancel=%q, want %q with ?status=success and ?status=cancel", stripe.successURL, stripe.cancelURL, page)
	}
}

// A caller's own return_url needs no CUSTOMER_PORTAL_URL.
func TestCreateSetupSession_CallerReturnURL_NoPortalNeeded(t *testing.T) {
	stripe := &returnStripe{}
	svc := NewService(newMemStore(), stripe, nil)
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, _, err := svc.CreateSetupSession(ctx, "ten_1", "cus_1", "https://app.example.test/customers/cus_1"); err != nil {
		t.Fatalf("CreateSetupSession: %v", err)
	}
	if stripe.successURL != "https://app.example.test/customers/cus_1?status=success" {
		t.Errorf("success = %q, want the caller's return_url with ?status=success", stripe.successURL)
	}
}
