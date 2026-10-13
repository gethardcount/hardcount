package paymentmethods

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
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
	if !errors.As(err, &de) || de.Code != "stripe_not_connected" {
		t.Fatalf("err = %#v, want code stripe_not_connected", err)
	}
}
