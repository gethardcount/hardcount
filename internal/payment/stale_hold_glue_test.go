package payment

import (
	"context"
	"fmt"

	"github.com/gethardcount/hardcount/internal/domain"
)

// The seams the stale-hold tests (#976, ADR-127) reach production through:
// taking a claim, charging under it, releasing it, and what the read by id
// returns. This is the only test file written against the API's current
// shape; the scenarios in stale_hold_integration_test.go do not change when
// it does.

// chargeClaim is what a claim gave its owner: its hold token.
type chargeClaim struct{ hold domain.ChargeHold }

func (c chargeClaim) ok() bool { return c.hold.Held() }

func (r *shRig) claim(kind chargeKind, invoiceID string) (chargeClaim, error) {
	var h domain.ChargeHold
	var err error
	switch kind {
	case autoCharge:
		h, err = r.invoices.ClaimAutoCharge(r.ctx, r.tenantID, invoiceID)
	case dunningRetry:
		h, err = r.invoices.ClaimChargeForDunningRetry(r.ctx, r.tenantID, invoiceID)
	case collect:
		h, err = r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, invoiceID)
	}
	return chargeClaim{hold: h}, err
}

func (r *shRig) chargeUnder(ctx context.Context, kind chargeKind, c chargeClaim, inv domain.Invoice, card string) (domain.Invoice, error) {
	if kind == dunningRetry {
		return r.stripe.ChargeInvoiceForDunningRetry(ctx, r.tenantID, inv, c.hold, stripeCustomerOf(inv), card)
	}
	return r.stripe.ChargeInvoice(ctx, r.tenantID, inv, c.hold, stripeCustomerOf(inv), card)
}

func (r *shRig) release(c chargeClaim, invoiceID string) error {
	return r.invoices.ReleaseChargeHold(r.ctx, r.tenantID, invoiceID, c.hold)
}

// setFakeListFields fills what the read reports beyond the common
// PaymentIntent fields: the invoice it is for, whether its charge was refunded
// in full, and whether any confirm reached a bank.
func setFakeListFields(res *PaymentIntentResult, pi *fakePI) {
	res.InvoiceID = pi.invoiceID
	res.FullyRefunded = pi.refunded
	// A charge exists once a confirm reached the card network: it succeeded,
	// is processing, or was declined (and may since have been canceled).
	res.Charged = pi.status == "succeeded" || pi.status == "processing" || pi.declineCode != "" || pi.refunded
}

// fakeAuthFailedErr is LiveStripeClient's error for a rejected API key.
func fakeAuthFailedErr() error {
	return fmt.Errorf("%w: %w", ErrStripeAuthFailed, &PaymentError{Message: "Invalid API Key provided"})
}

// fakeUnreachableErr is LiveStripeClient's error for a read Stripe never
// answered: a network error, or the caller's time limit.
func fakeUnreachableErr(cause string) error {
	return fmt.Errorf("%w: %w", ErrStripeUnreachable, &PaymentError{Message: cause, Unknown: true})
}
