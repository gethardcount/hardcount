package payment

import (
	"context"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The seams the stale-hold tests (#976, ADR-127) reach production through:
// taking a claim, charging under it, releasing it, and what the list read
// returns. This is the only test file written against the API's current
// shape; the scenarios in stale_hold_integration_test.go do not change when
// it does.

// chargeClaim is what a claim gave its owner.
type chargeClaim struct{ held bool }

func (c chargeClaim) ok() bool { return c.held }

func (r *shRig) claim(kind chargeKind, invoiceID string) (chargeClaim, error) {
	var ok bool
	var err error
	switch kind {
	case autoCharge:
		ok, err = r.invoices.ClaimAutoCharge(r.ctx, r.tenantID, invoiceID)
	case dunningRetry:
		ok, err = r.invoices.ClaimChargeForDunningRetry(r.ctx, r.tenantID, invoiceID)
	case collect:
		ok, err = r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, invoiceID)
	}
	return chargeClaim{held: ok}, err
}

func (r *shRig) chargeUnder(ctx context.Context, kind chargeKind, _ chargeClaim, inv domain.Invoice, card string) (domain.Invoice, error) {
	if kind == dunningRetry {
		return r.stripe.ChargeInvoiceForDunningRetry(ctx, r.tenantID, inv, stripeCustomerOf(inv), card)
	}
	return r.stripe.ChargeInvoice(ctx, r.tenantID, inv, stripeCustomerOf(inv), card)
}

func (r *shRig) release(_ chargeClaim, invoiceID string) error {
	return r.invoices.ReleaseAutoChargeClaim(r.ctx, r.tenantID, invoiceID)
}

// setFakeListFields fills what the list read reports beyond the common
// PaymentIntent fields.
func setFakeListFields(_ *PaymentIntentResult, _ *fakePI) {}

// fakeCustomerMissingErr is LiveStripeClient's error for a customer that does
// not exist in this Stripe account.
func fakeCustomerMissingErr(stripeCustomerID string) error {
	return &PaymentError{Message: "No such customer: '" + stripeCustomerID + "'"}
}

// fakeAuthFailedErr is LiveStripeClient's error for a rejected API key.
func fakeAuthFailedErr() error {
	return &PaymentError{Message: "Invalid API Key provided"}
}
