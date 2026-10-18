package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// Review of #1225: Stripe sends incorrect_number in two shapes. As the bank's
// decline code it comes with code card_declined, like lost_card. As Stripe's
// own error code it comes with no decline code at all: the test-card table
// lists "Incorrect number decline: error code incorrect_number, decline code
// n/a" (https://docs.stripe.com/testing#declined-payments). Reading only
// decline_code missed the second shape, so the card stayed on the retry
// schedule. Each reporter must read both. Errors are decoded from Stripe's
// JSON, as in hard_decline_test.go.

// incorrectNumberErrorJSON is Stripe's card_error with incorrect_number as
// the error code and no decline_code, on the error and on its PaymentIntent's
// last_payment_error.
const incorrectNumberErrorJSON = `{
  "code": "incorrect_number",
  "doc_url": "https://stripe.com/docs/error-codes/incorrect-number",
  "message": "Your card number is incorrect.",
  "param": "number",
  "payment_intent": {
    "id": "pi_badnum",
    "object": "payment_intent",
    "amount": 5000,
    "currency": "usd",
    "last_payment_error": {
      "code": "incorrect_number",
      "doc_url": "https://stripe.com/docs/error-codes/incorrect-number",
      "message": "Your card number is incorrect.",
      "param": "number",
      "payment_method": {"id": "pm_on_file", "object": "payment_method", "type": "card"},
      "type": "card_error"
    },
    "metadata": {"velox_invoice_id": "inv_1"},
    "payment_method": null,
    "status": "requires_payment_method"
  },
  "payment_method": {"id": "pm_on_file", "object": "payment_method", "type": "card"},
  "type": "card_error"
}`

func TestChargeDecline_IncorrectNumberAsErrorCode_IsAHardDecline(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	withCannedStripe(t, http.StatusPaymentRequired, `{"error": `+incorrectNumberErrorJSON+`}`)
	email := &recordingFailedEmail{}
	s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetEmailPaymentFailed(email, staticCustomerEmail{})

	_, err := s.ChargeInvoice(auth.WithTenantID(context.Background(), "t1"), "t1", snapshot, "cus_stripe", "pm_on_file")
	var pe *PaymentError
	if !errors.As(err, &pe) || pe.Unknown || pe.PaymentIntentID != "pi_badnum" {
		t.Fatalf("charge error = %#v, want a definite decline naming pi_badnum", err)
	}
	if pe.DeclineCode != "incorrect_number" || !domain.IsHardDecline(pe.DeclineCode) {
		t.Errorf("charge error decline code %q, want incorrect_number (a hard decline)", pe.DeclineCode)
	}
	if len(invoices.failureReports) != 1 {
		t.Fatalf("failure reports = %d, want 1", len(invoices.failureReports))
	}
	if rep := invoices.failureReports[0]; rep.DeclineCode != "incorrect_number" || rep.PaymentMethodID != "pm_on_file" {
		t.Errorf("report decline %q card %q, want incorrect_number on pm_on_file", rep.DeclineCode, rep.PaymentMethodID)
	}
	var attempt *domain.InvoiceChargeAttempt
	for i := range invoices.chargeAttempts {
		if invoices.chargeAttempts[i].StripePaymentIntentID == "pi_badnum" {
			attempt = &invoices.chargeAttempts[i]
		}
	}
	if attempt == nil || attempt.DeclineCode != "incorrect_number" {
		t.Errorf("charge attempt row = %+v, want decline incorrect_number", attempt)
	}
	if len(email.reasons) != 1 || email.reasons[0] != refusedCardCustomerMessage {
		t.Errorf("customer email reasons = %q, want the different-card message", email.reasons)
	}
}

// Control: an error code that is not a hard decline (expired_card, also sent
// with no decline code) stays out of the decline code, so the decline keeps
// the ordinary schedule and Stripe's own message.
func TestChargeDecline_OtherErrorCodeWithoutDeclineCode_StaysOrdinary(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	invoices.invoices["inv_1"] = snapshot
	body := `{"error": {"code": "expired_card", "message": "Your card has expired.", "param": "exp_month",
	  "payment_intent": {"id": "pi_exp", "object": "payment_intent", "status": "requires_payment_method",
	    "last_payment_error": {"code": "expired_card", "message": "Your card has expired.", "type": "card_error",
	      "payment_method": {"id": "pm_on_file", "object": "payment_method"}}},
	  "type": "card_error"}}`
	withCannedStripe(t, http.StatusPaymentRequired, body)
	email := &recordingFailedEmail{}
	s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
	s.SetEmailPaymentFailed(email, staticCustomerEmail{})

	_, err := s.ChargeInvoice(auth.WithTenantID(context.Background(), "t1"), "t1", snapshot, "cus_stripe", "pm_on_file")
	var pe *PaymentError
	if !errors.As(err, &pe) || pe.PaymentIntentID != "pi_exp" {
		t.Fatalf("charge error = %#v, want a decline naming pi_exp", err)
	}
	if pe.DeclineCode != "" {
		t.Errorf("decline code %q, want empty for expired_card", pe.DeclineCode)
	}
	if len(email.reasons) != 1 || email.reasons[0] != "Your card has expired." {
		t.Errorf("customer email reasons = %q, want Stripe's message", email.reasons)
	}
}

func TestWebhookPaymentFailed_IncorrectNumberAsErrorCode(t *testing.T) {
	cases := []struct {
		name, lastPaymentError, wantCode string
	}{
		{
			name: "incorrect_number as the error code",
			lastPaymentError: `{"code": "incorrect_number", "message": "Your card number is incorrect.", "param": "number",
				"payment_method": {"id": "pm_badnum", "object": "payment_method", "type": "card"}, "type": "card_error"}`,
			wantCode: "incorrect_number",
		},
		{
			name: "incorrect_number as the decline code",
			lastPaymentError: `{"code": "card_declined", "decline_code": "incorrect_number", "message": "Your card was declined.",
				"payment_method": {"id": "pm_badnum", "object": "payment_method", "type": "card"}, "type": "card_error"}`,
			wantCode: "incorrect_number",
		},
		{
			name: "expired_card as the error code",
			lastPaymentError: `{"code": "expired_card", "message": "Your card has expired.",
				"payment_method": {"id": "pm_badnum", "object": "payment_method", "type": "card"}, "type": "card_error"}`,
			wantCode: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = pendingSnapshot()
			s, _, _ := newDeclineStripe(&mockStripeClient{}, invoices)
			if err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_" + tc.name, EventType: "payment_intent.payment_failed",
				PaymentIntentID: "pi_web", InvoiceID: "inv_1", FailureMessage: "Your card number is incorrect.",
				Payload: map[string]any{"raw": paymentFailedEventBody(tc.lastPaymentError)},
			}); err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if len(invoices.failureReports) != 1 {
				t.Fatalf("failure reports = %d, want 1", len(invoices.failureReports))
			}
			if rep := invoices.failureReports[0]; rep.DeclineCode != tc.wantCode || rep.PaymentMethodID != "pm_badnum" {
				t.Errorf("report decline %q card %q, want %q on pm_badnum", rep.DeclineCode, rep.PaymentMethodID, tc.wantCode)
			}
		})
	}
}

func TestPaymentIntentResult_IncorrectNumberAsErrorCode(t *testing.T) {
	var pi stripe.PaymentIntent
	if err := json.Unmarshal([]byte(`{
  "id": "pi_rec",
  "object": "payment_intent",
  "last_payment_error": {
    "code": "incorrect_number",
    "message": "Your card number is incorrect.",
    "param": "number",
    "payment_method": {"id": "pm_badnum", "object": "payment_method", "type": "card"},
    "type": "card_error"
  },
  "status": "requires_payment_method"
}`), &pi); err != nil {
		t.Fatalf("decode PaymentIntent: %v", err)
	}
	if res := paymentIntentResult(&pi); res.DeclineCode != "incorrect_number" || res.PaymentMethodID != "pm_badnum" {
		t.Errorf("result decline %q card %q, want incorrect_number pm_badnum", res.DeclineCode, res.PaymentMethodID)
	}
}
