package domain

import (
	"strings"
	"testing"
	"time"
)

// Issue #1575: while a charge hold is on a failed invoice (ADR-127, an
// interrupted charge being confirmed with Stripe), the notice must say the
// charge is being confirmed and offer no Retry payment, which the hold refuses
// with 409 payment_in_flight.
func TestChargeHeldFailureNoticeOffersNoRetry(t *testing.T) {
	failed := Invoice{
		ID: "inv_1", Status: InvoiceFinalized, PaymentStatus: PaymentFailed,
		LastPaymentError: "Your card was declined.",
		UpdatedAt:        time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
	}
	att := ClassifyInvoiceAttention(failed, AttentionContext{HasPaymentMethod: true, ChargeHeld: true})
	if att == nil {
		t.Fatal("attention is nil, want the charge-confirming notice")
	}
	if !strings.Contains(att.Message, "being confirmed with Stripe") {
		t.Errorf("message %q, want it to say the charge is being confirmed with Stripe", att.Message)
	}
	for _, a := range att.Actions {
		if a.Code == AttentionActionRetryPayment {
			t.Errorf("actions %+v offer Retry payment while the hold is on, want none", att.Actions)
		}
	}
	checkShape(t, att.Message)
}
