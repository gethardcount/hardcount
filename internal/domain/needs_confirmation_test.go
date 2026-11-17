package domain

import (
	"errors"
	"testing"
)

// Which decline codes ask what of the customer, and which stop a dunning
// retry: a hard decline asks for a different card, authentication_required
// for the customer's confirmation, and every other code for nothing beyond
// paying (the ordinary retry schedule).
func TestDeclineAskOf(t *testing.T) {
	cases := []struct {
		code  string
		ask   DeclineAsk
		block error
	}{
		{"lost_card", DeclineAskNewCard, ErrCardNeedsReplacing},
		{"incorrect_number", DeclineAskNewCard, ErrCardNeedsReplacing},
		{"authentication_required", DeclineAskConfirm, ErrCardNeedsConfirmation},
		// Related, but not on Stripe Billing's no-retry list.
		{"authentication_not_handled", DeclineAskNone, nil},
		{"insufficient_funds", DeclineAskNone, nil},
		{"generic_decline", DeclineAskNone, nil},
		{"", DeclineAskNone, nil},
	}
	for _, tc := range cases {
		if got := DeclineAskOf(tc.code); got != tc.ask {
			t.Errorf("DeclineAskOf(%q) = %q, want %q", tc.code, got, tc.ask)
		}
		if got := RetryBlock(tc.code); !errors.Is(got, tc.block) || (tc.block == nil) != (got == nil) {
			t.Errorf("RetryBlock(%q) = %v, want %v", tc.code, got, tc.block)
		}
	}
	if IsHardDecline(AuthenticationRequiredCode) {
		t.Error("authentication_required is a hard decline: its copy would ask for a different card")
	}
}
