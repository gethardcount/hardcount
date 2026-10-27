package api

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

// noStripeKeys is a credential store with nothing connected.
type noStripeKeys struct{}

func (noStripeKeys) GetPlaintext(context.Context, string, bool) (tenantstripe.PlaintextSecrets, error) {
	return tenantstripe.PlaintextSecrets{}, errs.ErrNotFound
}

// The hosted Pay session create with no Stripe keys for the mode answers the
// typed stripe_not_connected, which the public page turns into its own 409.
// A bare error here became a 502 "failed to create checkout session".
func TestHostedInvoiceAdapter_NotConnectedIsTyped(t *testing.T) {
	a := &hostedInvoiceStripeAdapter{
		clients:  payment.NewStripeClients(noStripeKeys{}),
		sessions: payment.NewCheckoutSessionStore(nil),
	}
	ctx := postgres.WithLivemode(context.Background(), false)
	_, err := a.CreateInvoicePaymentSession(ctx, "t_1", domain.Invoice{ID: "vlx_inv_1", CustomerID: "cus_1"}, "https://s", "https://c")
	if errs.Code(err) != errs.CodeStripeNotConnected {
		t.Fatalf("err = %v (code %q), want %q", err, errs.Code(err), errs.CodeStripeNotConnected)
	}
}
