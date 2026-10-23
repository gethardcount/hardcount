package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// An off-session charge the customer's bank wants them to confirm (3-D
// Secure): Stripe answers 402 card_error authentication_required, as the
// decline code and as the error code (https://docs.stripe.com/error-codes),
// or as the error code alone. Errors are decoded from Stripe's JSON through
// stripe-go, as in hard_decline_error_code_test.go.
func authRequiredErrorJSON(withDeclineCode bool) string {
	decline := ""
	if withDeclineCode {
		decline = `"decline_code": "authentication_required",`
	}
	return `{"error": {"code": "authentication_required", ` + decline + `
	  "message": "This payment requires authentication. If the customer is off session, ask them to return to your application and complete the payment.",
	  "payment_intent": {"id": "pi_3ds", "object": "payment_intent", "status": "requires_payment_method",
	    "last_payment_error": {"code": "authentication_required", ` + decline + ` "message": "Your card was declined. This transaction requires authentication.",
	      "type": "card_error", "payment_method": {"id": "pm_on_file", "object": "payment_method"}}},
	  "payment_method": {"id": "pm_on_file", "object": "payment_method", "type": "card"},
	  "type": "card_error"}}`
}

// The charge call records the code with the card, the customer's email uses
// the "confirm your payment" copy with no reason line (Stripe's message for
// it is written for developers), and Collect payment's answer says the
// customer must confirm, not that the card was declined.
func TestChargeDecline_AuthenticationRequired_AsksTheCustomerToConfirm(t *testing.T) {
	for _, withDeclineCode := range []bool{true, false} {
		invoices := newMockInvoiceUpdater()
		snapshot := pendingSnapshot()
		invoices.invoices["inv_1"] = snapshot
		withCannedStripe(t, http.StatusPaymentRequired, authRequiredErrorJSON(withDeclineCode))
		email := &recordingFailedEmail{}
		s := NewStripe(NewLiveStripeClient(NewStripeClients(fixedCreds{})), invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
		s.SetEmailPaymentFailed(email, staticCustomerEmail{})

		_, err := s.ChargeInvoice(auth.WithTenantID(context.Background(), "t1"), "t1", snapshot, testHold, "cus_stripe", "pm_on_file")
		var pe *PaymentError
		if !errors.As(err, &pe) || pe.Unknown || pe.PaymentIntentID != "pi_3ds" {
			t.Fatalf("decline_code=%v: charge error = %#v, want a definite decline naming pi_3ds", withDeclineCode, err)
		}
		if pe.DeclineCode != domain.AuthenticationRequiredCode {
			t.Errorf("decline_code=%v: decline code %q, want authentication_required", withDeclineCode, pe.DeclineCode)
		}
		if len(invoices.failureReports) != 1 || invoices.failureReports[0].DeclineCode != domain.AuthenticationRequiredCode ||
			invoices.failureReports[0].PaymentMethodID != "pm_on_file" {
			t.Errorf("decline_code=%v: failure reports %+v, want authentication_required on pm_on_file", withDeclineCode, invoices.failureReports)
		}
		if len(email.asks) != 1 || email.asks[0] != domain.DeclineAskConfirm || email.reasons[0] != "" {
			t.Errorf("decline_code=%v: customer email asks %q reasons %q, want one confirm email with no reason line", withDeclineCode, email.asks, email.reasons)
		}
		if msg := pe.OperatorSafeMessage(); strings.Contains(strings.ToLower(msg), "declined") || !strings.Contains(msg, "confirm") {
			t.Errorf("decline_code=%v: operator message %q, want the customer must confirm", withDeclineCode, msg)
		}
	}
}

func TestWebhookAndReconciler_AuthenticationRequiredAsErrorCode(t *testing.T) {
	const lpe = `{"code": "authentication_required", "message": "Your card was declined. This transaction requires authentication.",
		"payment_method": {"id": "pm_3ds", "object": "payment_method", "type": "card"}, "type": "card_error"}`
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = pendingSnapshot()
	s, _, _ := newDeclineStripe(&mockStripeClient{}, invoices)
	if err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID: "evt_3ds", EventType: "payment_intent.payment_failed",
		PaymentIntentID: "pi_web", InvoiceID: "inv_1", FailureMessage: "Your card was declined.",
		Payload: map[string]any{"raw": paymentFailedEventBody(lpe)},
	}); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(invoices.failureReports) != 1 || invoices.failureReports[0].DeclineCode != domain.AuthenticationRequiredCode {
		t.Errorf("webhook failure reports %+v, want authentication_required", invoices.failureReports)
	}

	var pi stripe.PaymentIntent
	if err := json.Unmarshal([]byte(`{"id": "pi_rec", "object": "payment_intent", "last_payment_error": `+lpe+`, "status": "requires_payment_method"}`), &pi); err != nil {
		t.Fatalf("decode PaymentIntent: %v", err)
	}
	if res := paymentIntentResult(&pi); res.DeclineCode != domain.AuthenticationRequiredCode || res.PaymentMethodID != "pm_3ds" {
		t.Errorf("reconciler result decline %q card %q, want authentication_required pm_3ds", res.DeclineCode, res.PaymentMethodID)
	}
}
