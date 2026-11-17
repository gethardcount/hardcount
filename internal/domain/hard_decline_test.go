package domain

import (
	"slices"
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
	// anything Hardcount does not know.
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
