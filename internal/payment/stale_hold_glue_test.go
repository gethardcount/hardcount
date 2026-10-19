package payment

import (
	"context"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The seams the stale-hold tests (#976, ADR-127) reach production through:
// taking a claim, charging under it, releasing it, and what the list read
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

// setStaleHoldCheckTimeout bounds each stale-hold check's Stripe calls.
func setStaleHoldCheckTimeout(rec *Reconciler, d time.Duration) { rec.staleHoldTimeout = d }

// setStaleHoldBudget bounds one run of the stale-hold sweep.
func setStaleHoldBudget(rec *Reconciler, d time.Duration) { rec.staleHoldBudget = d }

// setFakeListFields fills what the list read reports beyond the common
// PaymentIntent fields: the invoice it is for, and whether its charge was
// refunded in full.
func setFakeListFields(res *PaymentIntentResult, pi *fakePI) {
	res.InvoiceID = pi.invoiceID
	res.FullyRefunded = pi.refunded
}

// fakeCustomerMissingErr is LiveStripeClient's error for a customer that does
// not exist in this Stripe account.
func fakeCustomerMissingErr(stripeCustomerID string) error {
	return &listRefused{ErrStripeCustomerMissing, "No such customer: '" + stripeCustomerID + "'"}
}

// fakeAuthFailedErr is LiveStripeClient's error for a rejected API key.
func fakeAuthFailedErr() error {
	return &listRefused{ErrStripeAuthFailed, "Invalid API Key provided"}
}

// listRefused wraps a typed list refusal the way classifyListError does.
type listRefused struct {
	kind error
	msg  string
}

func (e *listRefused) Error() string { return e.kind.Error() + ": " + e.msg }
func (e *listRefused) Unwrap() error { return e.kind }
