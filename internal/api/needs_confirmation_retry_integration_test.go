package api

import (
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
)

// The bank's request that the customer confirm a payment
// (authentication_required) reaches Velox only through the
// payment_intent.payment_failed webhook: the charge call's own report was
// lost. Stripe sends it in two shapes, with the decline code set and with
// only the error code (https://docs.stripe.com/error-codes, entry
// authentication_required). Either way the webhook puts it on record with the
// card, the notice asks for the customer's confirmation, and the next dunning
// retry charges nothing (Stripe Billing does not retry this code,
// https://docs.stripe.com/billing/revenue-recovery/smart-retries#non-retryable-decline-codes).
func TestNeedsConfirmation_WebhookReportStopsTheRetry_RealStore(t *testing.T) {
	for _, tc := range []struct{ name, lastPaymentError string }{
		{"decline_code", `{"code": "authentication_required", "decline_code": "authentication_required", "message": "Your card was declined. This transaction requires authentication.",
		  "payment_method": {"id": "pm_3ds", "object": "payment_method", "type": "card"}, "type": "card_error"}`},
		{"error_code_only", `{"code": "authentication_required", "message": "Your card was declined. This transaction requires authentication.",
		  "payment_method": {"id": "pm_3ds", "object": "payment_method", "type": "card"}, "type": "card_error"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHardDeclineRig(t, "Needs Confirm Webhook "+tc.name)
			custID := r.customerWithCard("sca", "pm_3ds")
			inv := r.finalizedInvoice(custID, "INV-SCA-1")

			body := `{"id": "evt_sca_1", "object": "event", "type": "payment_intent.payment_failed",
			  "data": {"object": {"id": "pi_sca_1", "object": "payment_intent", "amount": 5000, "currency": "usd",
			    "last_payment_error": ` + tc.lastPaymentError + `,
			    "metadata": {"velox_invoice_id": "` + inv.ID + `", "velox_tenant_id": "` + r.tenantID + `"},
			    "payment_method": null, "status": "requires_payment_method"}}}`
			if err := r.charger.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
				StripeEventID: "evt_sca_1", EventType: "payment_intent.payment_failed", PaymentIntentID: "pi_sca_1",
				InvoiceID: inv.ID, FailureMessage: "Your card was declined. This transaction requires authentication.", Payload: map[string]any{"raw": body},
			}); err != nil {
				t.Fatalf("webhook: %v", err)
			}
			attempts, err := r.invoices.ListChargeAttemptsByInvoice(r.ctx, r.tenantID, inv.ID)
			if err != nil {
				t.Fatalf("list attempts: %v", err)
			}
			if len(attempts) != 1 || attempts[0].DeclineCode != "authentication_required" || attempts[0].StripePaymentMethodID != "pm_3ds" {
				t.Fatalf("attempt rows = %+v, want one with authentication_required on pm_3ds", attempts)
			}
			if code := r.attentionCode(inv.ID); code != "payment.authentication_required" {
				t.Errorf("attention %q, want payment.authentication_required", code)
			}
			// Twice in a row: the first skip must release the charge lease, or
			// the second comes back as an uncounted transient skip instead.
			for i := 1; i <= 2; i++ {
				if err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID); !errors.Is(err, domain.ErrCardNeedsConfirmation) {
					t.Fatalf("retry %d: %v, want ErrCardNeedsConfirmation: a counted retry that charges nothing", i, err)
				}
			}
			if len(r.charges.charged) != 0 {
				t.Errorf("Stripe charges %v, want none: an off-session retry cannot pass the bank's confirmation", r.charges.charged)
			}
		})
	}
}

// A card the bank refused for good stays refused when a later charge on it
// gets authentication_required instead: the refusal decides, so the retry
// asks for a different card, not for confirmation.
func TestNeedsConfirmation_HardDeclineOnTheSameCardStillDecides_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Needs Confirm After Hard Decline")
	custID := r.customerWithCard("lost3ds", "pm_lost3ds")
	inv := r.finalizedInvoice(custID, "INV-SCA-2")
	r.charges.answers = []scriptedCharge{
		{err: stripeDeclined("pi_l3_1", "lost_card", "Your card was declined.")},
		{err: stripeDeclined("pi_l3_2", "authentication_required", "Your card was declined. This transaction requires authentication.")},
	}
	if _, err := r.chargeAsCycle(inv, "cus_stripe_lost3ds", "pm_lost3ds"); err == nil {
		t.Fatal("the lost-card charge returned no error")
	}
	if _, err := r.chargeAsCycle(r.invoice(inv.ID), "cus_stripe_lost3ds", "pm_lost3ds"); err == nil {
		t.Fatal("the second charge returned no error")
	}
	if code := r.attentionCode(inv.ID); code != "payment.card_refused" {
		t.Errorf("attention %q, want payment.card_refused", code)
	}
	if err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID); !errors.Is(err, domain.ErrCardNeedsReplacing) {
		t.Fatalf("retry: %v, want ErrCardNeedsReplacing", err)
	}
	if len(r.charges.charged) != 2 {
		t.Errorf("Stripe charges %v, want the two the test made", r.charges.charged)
	}
}

// The newest answer for the card decides between a request to confirm and an
// ordinary decline. An operator's Collect payment after the bank asked for
// confirmation is declined for funds this time: the bank no longer asked for
// confirmation, so the next retry charges the card again.
func TestNeedsConfirmation_ALaterOrdinaryDeclineIsRetried_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Needs Confirm Then Decline")
	custID := r.customerWithCard("scansf", "pm_scansf")
	inv := r.finalizedInvoice(custID, "INV-SCA-3")
	r.charges.answers = []scriptedCharge{
		{err: stripeDeclined("pi_sn_1", "authentication_required", "Your card was declined. This transaction requires authentication.")},
		{err: stripeDeclined("pi_sn_2", "insufficient_funds", "Your card has insufficient funds.")},
		{err: stripeDeclined("pi_sn_3", "insufficient_funds", "Your card has insufficient funds.")},
	}
	if _, err := r.chargeAsCycle(inv, "cus_stripe_scansf", "pm_scansf"); err == nil {
		t.Fatal("the first charge returned no error")
	}
	if code := r.attentionCode(inv.ID); code != "payment.authentication_required" {
		t.Errorf("attention after the request to confirm: %q, want payment.authentication_required", code)
	}
	if _, err := r.chargeAsCycle(r.invoice(inv.ID), "cus_stripe_scansf", "pm_scansf"); err == nil {
		t.Fatal("the second charge returned no error")
	}
	if code := r.attentionCode(inv.ID); code != "payment.declined" {
		t.Errorf("attention after the later decline: %q, want payment.declined", code)
	}
	err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID)
	var re *dunning.RetryError
	if !errors.As(err, &re) || re.PaymentIntentID != "pi_sn_3" {
		t.Fatalf("retry: %#v, want a counted decline on pi_sn_3 (the card charged again)", err)
	}
	if len(r.charges.charged) != 3 {
		t.Errorf("Stripe charges %v, want three", r.charges.charged)
	}
}
