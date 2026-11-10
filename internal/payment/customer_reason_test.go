package payment

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The reason the payment-failed email shows the customer is Stripe's message
// for a card error, which Stripe says can be shown to the customer
// (https://docs.stripe.com/api/errors, "message"). Hardcount's own text for the
// invoice ("reconciled: requires_payment_method", the default "payment
// failed") and Stripe's developer text for other error types are not
// shown: the email then has no reason line.

func TestPaymentFailedEmail_Reconciler_ShowsOnlyStripesCardMessage(t *testing.T) {
	cases := []struct {
		name, pi, wantReason string
	}{
		{"declined, no last_payment_error", `{"id": "pi_rec", "object": "payment_intent", "status": "requires_payment_method", "last_payment_error": null}`, ""},
		{"canceled after a charge", `{"id": "pi_rec", "object": "payment_intent", "status": "canceled", "last_payment_error": null, "latest_charge": {"id": "ch_rec", "object": "charge"}}`, ""},
		// A payment that needs the customer to authenticate is refused with a
		// payment error and no charge (error_on_requires_action). Canceled
		// afterwards, it is still a failed payment the customer must hear of:
		// one email (the "confirm with your bank" copy, which has no reason
		// line). Read as "no bank was asked", it sent none.
		{"canceled after authentication was required (no charge)", `{"id": "pi_rec", "object": "payment_intent", "status": "canceled", "latest_charge": null,
		  "last_payment_error": {"code": "authentication_required", "message": "This payment requires authentication.", "type": "card_error",
		    "payment_method": {"id": "pm_on_file", "object": "payment_method"}}}`, ""},
		{"card error", `{"id": "pi_rec", "object": "payment_intent", "status": "requires_payment_method",
		  "last_payment_error": {"code": "card_declined", "decline_code": "insufficient_funds", "message": "Your card has insufficient funds.", "type": "card_error",
		    "payment_method": {"id": "pm_on_file", "object": "payment_method"}}}`, "Your card has insufficient funds."},
		{"not a card error", `{"id": "pi_rec", "object": "payment_intent", "status": "requires_payment_method",
		  "last_payment_error": {"code": "payment_intent_unexpected_state", "message": "This PaymentIntent's payment_method could not be updated.", "type": "invalid_request_error"}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withCannedStripe(t, http.StatusOK, tc.pi)
			unknown := pendingSnapshot()
			unknown.PaymentStatus = domain.PaymentUnknown
			unknown.StripePaymentIntentID = "pi_rec"
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = unknown
			client := NewLiveStripeClient(NewStripeClients(fixedCreds{}))
			s, email, _ := newDeclineStripe(&mockStripeClient{}, invoices)
			r := NewReconciler(client, newMockReconcileStore(unknown), time.Second)
			r.SetSettler(s)

			if _, errs := r.Run(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), 10); len(errs) != 0 {
				t.Fatalf("reconciler errors: %v", errs)
			}
			if len(email.reasons) != 1 || email.reasons[0] != tc.wantReason {
				t.Errorf("customer email reasons %q, want [%q]", email.reasons, tc.wantReason)
			}
		})
	}
}

// Canceled with no charge: no confirm ever reached a bank (a charge whose
// confirm never arrived, canceled by the reconciler). Not a failed payment, so
// no email.
func TestPaymentFailedEmail_Reconciler_CanceledWithNoChargeSendsNone(t *testing.T) {
	withCannedStripe(t, http.StatusOK, `{"id": "pi_rec", "object": "payment_intent", "status": "canceled", "last_payment_error": null, "latest_charge": null}`)
	unknown := pendingSnapshot()
	unknown.PaymentStatus = domain.PaymentUnknown
	unknown.StripePaymentIntentID = "pi_rec"
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = unknown
	s, email, _ := newDeclineStripe(&mockStripeClient{}, invoices)
	r := NewReconciler(NewLiveStripeClient(NewStripeClients(fixedCreds{})), newMockReconcileStore(unknown), time.Second)
	r.SetSettler(s)
	if _, errs := r.Run(auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1"), 10); len(errs) != 0 {
		t.Fatalf("reconciler errors: %v", errs)
	}
	if len(email.reasons) != 0 {
		t.Errorf("customer emails %q, want none: no bank saw a charge", email.reasons)
	}
}

func TestPaymentFailedEmail_Webhook_ShowsOnlyStripesCardMessage(t *testing.T) {
	cases := []struct {
		name, failureMessage, lastPaymentError, wantReason string
	}{
		{"no message", "", `null`, ""},
		{"card error", "Your card has insufficient funds.",
			`{"code": "card_declined", "decline_code": "insufficient_funds", "message": "Your card has insufficient funds.", "type": "card_error"}`,
			"Your card has insufficient funds."},
		{"not a card error", "This PaymentIntent's payment_method could not be updated.",
			`{"code": "payment_intent_unexpected_state", "message": "This PaymentIntent's payment_method could not be updated.", "type": "invalid_request_error"}`,
			""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = pendingSnapshot()
			s, email, _ := newDeclineStripe(&mockStripeClient{}, invoices)
			if err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_" + tc.name, EventType: "payment_intent.payment_failed",
				PaymentIntentID: "pi_web", InvoiceID: "inv_1", FailureMessage: tc.failureMessage,
				Payload: map[string]any{"raw": paymentFailedEventBody(tc.lastPaymentError)},
			}); err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if len(email.reasons) != 1 || email.reasons[0] != tc.wantReason {
				t.Errorf("customer email reasons %q, want [%q]", email.reasons, tc.wantReason)
			}
		})
	}
}
