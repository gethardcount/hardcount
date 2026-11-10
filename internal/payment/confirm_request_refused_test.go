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

// afterConfirmError's contract (stripe.go): a confirm request Stripe refuses
// outright asked no bank for money, so the invoice is stamped failed with no
// PaymentIntent, no customer email is sent, and the unconfirmed PaymentIntent
// is canceled. Only a card error is a bank's answer.
//
// Stripe puts the payment_intent object on the error of a request that
// involves a PaymentIntent, and a confirm is one. So the refusal is told from
// a decline by its type, not by whether the body names the PaymentIntent:
// both replies below are a 400 invalid_request_error to the confirm, one
// without the object and one with it. Each is decoded by stripe-go through
// LiveStripeClient, as in production.

var confirmRefusals = []struct{ name, body string }{
	{"the reply names no PaymentIntent", `{"error": {"code": "parameter_invalid_empty",
  "message": "As per Indian regulations, export transactions require a description.",
  "type": "invalid_request_error"}}`},
	{"the reply names the PaymentIntent", `{"error": {"code": "payment_intent_incompatible_payment_method",
  "message": "This PaymentIntent's payment_method could not be updated.",
  "payment_intent": {"id": "pi_x", "object": "payment_intent", "status": "requires_confirmation"},
  "type": "invalid_request_error"}}`},
}

// The live client must hand afterConfirmError the fact it branches on: a
// refused confirm request names no PaymentIntent, whatever its reply carried.
func TestConfirmPaymentIntent_RequestRefusal_NamesNoPaymentIntent(t *testing.T) {
	for _, tc := range confirmRefusals {
		t.Run(tc.name, func(t *testing.T) {
			withCannedStripe(t, http.StatusBadRequest, tc.body)
			ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
			c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))

			_, err := c.ConfirmPaymentIntent(ctx, "pi_x", PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
			var pe *PaymentError
			if !errors.As(err, &pe) || pe.Unknown {
				t.Fatalf("err = %#v, want a definite refusal", err)
			}
			if pe.PaymentIntentID != "" {
				t.Errorf("PaymentIntentID = %q on a refused confirm request: afterConfirmError reads a named definite failure as a bank's decline (the payment-failed email, dunning, and no cancel)", pe.PaymentIntentID)
			}
		})
	}
}

// A card error at the confirm is a bank's answer: it names the PaymentIntent,
// whether or not the reply carried the object.
func TestConfirmPaymentIntent_CardError_NamesThePaymentIntent(t *testing.T) {
	withCannedStripe(t, http.StatusPaymentRequired, `{"error": {"code": "card_declined", "decline_code": "insufficient_funds",
  "message": "Your card has insufficient funds.", "type": "card_error"}}`)
	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")
	c := NewLiveStripeClient(NewStripeClients(fixedCreds{}))

	_, err := c.ConfirmPaymentIntent(ctx, "pi_x", PaymentIntentParams{AmountCents: 100, Currency: "usd", CustomerID: "cus_x", PaymentMethodID: "pm_x", IdempotencyKey: "k"})
	var pe *PaymentError
	if !errors.As(err, &pe) || pe.Unknown || pe.PaymentIntentID != "pi_x" {
		t.Fatalf("err = %#v, want a definite decline that names pi_x", err)
	}
}

// End to end through chargeInvoice on the live client: no bank saw this
// charge, so no payment-failed email may be sent, the invoice must not keep a
// PaymentIntent as a declined payment, and the unconfirmed PaymentIntent must
// be canceled.
func TestChargeInvoice_ConfirmRequestRefused_NoCustomerEmail(t *testing.T) {
	for _, tc := range confirmRefusals {
		t.Run(tc.name, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			snapshot := pendingSnapshot()
			invoices.invoices["inv_1"] = snapshot
			stub := withCannedConfirm(t, http.StatusBadRequest, tc.body)
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
		})
	}
}
