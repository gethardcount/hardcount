package payment

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// From review 4 (2026-11-10). afterConfirmError's contract (stripe.go): a confirm request
// Stripe refuses outright, naming no PaymentIntent, asked no bank for money,
// so the invoice is stamped failed with no PaymentIntent, no customer email is
// sent, and the unconfirmed PaymentIntent is canceled.
//
// The reply below is a plain 400 invalid_request_error to the confirm: no
// card was asked, and the error body carries no payment_intent object. It is
// decoded by stripe-go through LiveStripeClient, as in production.

const stripeConfirmRefusedNoPI = `{"error": {"code": "parameter_invalid_empty",
  "message": "As per Indian regulations, export transactions require a description.",
  "type": "invalid_request_error"}}`

// The live client must hand afterConfirmError the fact it branches on: a
// confirm refusal whose reply named no PaymentIntent.
func TestConfirmPaymentIntent_RequestRefusal_NamesNoPaymentIntent(t *testing.T) {
	withCannedStripe(t, http.StatusBadRequest, stripeConfirmRefusedNoPI)
	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))

	_, err := c.ConfirmPaymentIntent(ctx, "pi_x", PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
	var pe *PaymentError
	if !errors.As(err, &pe) || pe.Unknown {
		t.Fatalf("err = %#v, want a definite refusal", err)
	}
	if pe.PaymentIntentID != "" {
		t.Errorf("PaymentIntentID = %q on a refusal whose reply named none: afterConfirmError reads a named definite failure as a bank's decline (SettleFailed: payment.failed and the customer's payment-failed email), so its no-email branch is unreachable in production", pe.PaymentIntentID)
	}
}

// End to end through chargeInvoice on the live client: no bank saw this
// charge, so no payment-failed email may be sent, the invoice must not keep a
// PaymentIntent as a declined payment, and the unconfirmed PaymentIntent must
// be canceled.
func TestChargeInvoice_ConfirmRequestRefused_NoCustomerEmail(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	stub := withCannedConfirm(t, http.StatusBadRequest, stripeConfirmRefusedNoPI)
	email := &recordingFailedEmail{}
	s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetEmailPaymentFailed(email, staticCustomerEmail{})

	_, err := s.ChargeInvoice(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), "t1", snapshot, testHold, "cus_stripe", "pm_on_file")
	if err == nil {
		t.Fatal("charge succeeded, want the refusal")
	}
	if email.sends != 0 {
		t.Errorf("payment-failed emails = %d, want 0: Stripe refused the confirm request itself, no bank was asked", email.sends)
	}
	got := invoices.invoices["inv_1"]
	if got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != "" {
		t.Errorf("invoice %s with PaymentIntent %q, want failed with no PaymentIntent (stampFailedWithoutPayment)", got.PaymentStatus, got.StripePaymentIntentID)
	}
	// create + confirm + the cancel of the unconfirmed PaymentIntent.
	if stub.calls < 3 {
		t.Errorf("Stripe calls = %d, want 3 (create, confirm, cancel): the refused confirm's PaymentIntent was left uncanceled", stub.calls)
	}
}
