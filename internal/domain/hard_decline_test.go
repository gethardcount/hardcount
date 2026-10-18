package domain

import (
	"slices"
	"strings"
	"testing"
)

// The named set (#966). Each code here is one Stripe Billing will not retry
// (docs.stripe.com/billing/revenue-recovery/smart-retries, "Hard decline
// codes"). Two of Stripe's codes are left out: authentication_required, which
// needs the customer to authenticate rather than a new card, and
// highest_risk_level, which is Stripe Radar's block reason (a charge's
// outcome.reason, never a bank's decline code: docs.stripe.com/declines,
// "Blocked payments"), so a decline code can never carry it.
func TestIsHardDecline(t *testing.T) {
	hard := []string{
		"lost_card", "stolen_card", "pickup_card",
		"incorrect_number", "revocation_of_authorization", "revocation_of_all_authorizations",
		"transaction_not_allowed",
	}
	for _, code := range hard {
		if !IsHardDecline(code) {
			t.Errorf("IsHardDecline(%q) = false, want true", code)
		}
	}
	// Retried on the normal schedule: recoverable declines, Radar decisions,
	// codes Stripe's retry rule does not treat as final, the one Stripe code
	// that needs the customer to authenticate rather than a new card, and
	// anything Velox does not know.
	retried := []string{
		"insufficient_funds", "do_not_honor", "generic_decline", "card_declined",
		"try_again_later", "processing_error", "issuer_not_available", "card_velocity_exceeded",
		"expired_card", "incorrect_cvc", "fraudulent", "merchant_blacklist", "restricted_card",
		"stop_payment_order", "authentication_required", "highest_risk_level", "", "LOST_CARD", "some_future_code",
	}
	for _, code := range retried {
		if IsHardDecline(code) {
			t.Errorf("IsHardDecline(%q) = true, want false (keeps the normal retry schedule)", code)
		}
	}

	got := HardDeclineCodes()
	if !slices.IsSorted(got) {
		t.Errorf("HardDeclineCodes() = %v, want sorted", got)
	}
	want := slices.Clone(hard)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("HardDeclineCodes() = %v, want %v — the SQL reads this list, so it must be exactly the set IsHardDecline accepts", got, want)
	}
}

func refusedCardInvoice() Invoice {
	return Invoice{
		ID: "inv_1", Status: InvoiceFinalized, PaymentStatus: PaymentFailed,
		LastPaymentError: "Your card was declined.",
	}
}

func TestClassifyAttention_RefusedCard_AsksForADifferentCard(t *testing.T) {
	att := ClassifyInvoiceAttention(refusedCardInvoice(), AttentionContext{
		HasPaymentMethod: true, RefusedCardDeclineCode: "lost_card",
	})
	if att == nil {
		t.Fatal("no attention on a failed invoice")
	}
	if att.Reason != AttentionReasonPaymentFailed || att.Severity != AttentionSeverityCritical || att.Code != "payment.card_refused" {
		t.Errorf("attention = %s/%s/%s, want payment_failed/critical/payment.card_refused", att.Reason, att.Severity, att.Code)
	}
	if !strings.Contains(att.Message, "different card") {
		t.Errorf("message %q does not tell the operator the customer needs a different card", att.Message)
	}
	if len(strings.Fields(strings.Split(att.Message, "\n")[0])) > 25 {
		t.Errorf("message %q is longer than 25 words", att.Message)
	}
	if !strings.Contains(att.Detail, "lost_card") {
		t.Errorf("detail %q does not name Stripe's decline code", att.Detail)
	}
	// Retry would charge the card the bank refused: not offered.
	if len(att.Actions) != 1 || att.Actions[0].Code != AttentionActionUpdatePaymentMethod {
		t.Errorf("actions = %+v, want only update_payment_method", att.Actions)
	}
	if att.ProviderResponse != "Your card was declined." {
		t.Errorf("provider response %q, want Stripe's message", att.ProviderResponse)
	}
}

// Control: without a refused card the ordinary decline notice is unchanged,
// Retry included — the next retry will charge the card.
func TestClassifyAttention_OrdinaryDecline_Unchanged(t *testing.T) {
	att := ClassifyInvoiceAttention(refusedCardInvoice(), AttentionContext{HasPaymentMethod: true})
	if att == nil || att.Code != "payment.declined" {
		t.Fatalf("attention = %+v, want payment.declined", att)
	}
	if len(att.Actions) != 2 || att.Actions[1].Code != AttentionActionRetryPayment {
		t.Errorf("actions = %+v, want update_payment_method then retry_payment", att.Actions)
	}
}
