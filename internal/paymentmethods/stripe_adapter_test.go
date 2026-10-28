package paymentmethods

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// With no Stripe account connected for the caller's mode, the adapter must
// say so in a typed error. A bare error reached the API as a 500 "internal
// error", which told the operator nothing (seen walking the Stripe runbook
// on v0.6.1: card setup after a disconnect).
func TestStripeAdapter_NotConnectedIsATypedError(t *testing.T) {
	a := NewStripeAdapter(nil, nil)
	_, err := a.client(context.Background())
	if err == nil {
		t.Fatal("no client and no error")
	}
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("err = %v, want an invalid-state error (409), not an untyped one (500)", err)
	}
	var de *errs.DomainError
	if !errors.As(err, &de) || de.Code != errs.CodeStripeNotConnected {
		t.Fatalf("err = %#v, want code stripe_not_connected", err)
	}
}

// noStripeIDLink is a customer Hardcount has never sent to Stripe.
type noStripeIDLink struct{ CustomerStripeLink }

func (noStripeIDLink) Get(context.Context, string, string) (domain.Customer, error) {
	return domain.Customer{ID: "hc_cus_1"}, nil
}

// The first card setup for such a customer creates its Stripe customer, so
// it asks for a client before client() does. With none connected it must
// say so the same way, not with a bare error (a 500).
func TestStripeAdapter_EnsureCustomerNotConnectedIsATypedError(t *testing.T) {
	a := NewStripeAdapter(nil, noStripeIDLink{})
	ctx := postgres.WithLivemode(context.Background(), false)
	_, err := a.EnsureStripeCustomer(ctx, "hc_ten_1", "hc_cus_1")
	if errs.Code(err) != errs.CodeStripeNotConnected || !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("err = %#v, want the 409 stripe_not_connected", err)
	}
}
