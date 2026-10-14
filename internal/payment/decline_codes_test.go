package payment

import (
	"strings"
	"testing"
)

// TestDeclineCodeToOperatorMessage_CuratedCodes asserts the high-traffic
// codes resolve to their curated phrasings rather than the awkward
// title-case fallback. These are the codes operators see most often
// in production; their text is part of the customer-support surface
// and shouldn't drift accidentally.
func TestDeclineCodeToOperatorMessage_CuratedCodes(t *testing.T) {
	cases := map[string]string{
		"insufficient_funds": "Insufficient funds.",
		"expired_card":       "The card has expired.",
		"do_not_honor":       "The card was declined by the issuer.",
		"lost_card":          "The card has been reported lost. The customer should contact their bank.",
		"stolen_card":        "The card has been reported stolen. The customer should contact their bank.",
		"incorrect_cvc":      "The security code is incorrect.",
		"card_declined":      "The card was declined by the issuer.",
		"testmode_decline":   "Test card declined as expected.",
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			got := declineCodeToOperatorMessage(code)
			if got != want {
				t.Errorf("declineCodeToOperatorMessage(%q) = %q, want %q", code, got, want)
			}
		})
	}
}

// TestDeclineCodeToOperatorMessage_UnknownFallsBackToTitleCase asserts
// the title-case fallback fires for codes Stripe might add later that
// aren't in our curated map. The fallback's job is to keep the path
// readable without a curated entry.
func TestDeclineCodeToOperatorMessage_UnknownFallsBackToTitleCase(t *testing.T) {
	got := declineCodeToOperatorMessage("future_unknown_reason")
	want := "Future unknown reason."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestDeclineCodeToOperatorMessage_EmptyReturnsEmpty asserts the empty
// string short-circuits — caller decides what to do (typically falls
// back to the "Payment provider rejected" generic message).
func TestDeclineCodeToOperatorMessage_EmptyReturnsEmpty(t *testing.T) {
	if got := declineCodeToOperatorMessage(""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// TestPaymentError_OperatorSafeMessage_UsesCuratedMap asserts the
// PaymentError integration: when DeclineCode is set, the message
// flows through declineCodeToOperatorMessage. Catches regressions
// where someone reverts to the title-case-only path.
func TestPaymentError_OperatorSafeMessage_UsesCuratedMap(t *testing.T) {
	pe := &PaymentError{DeclineCode: "lost_card"}
	got := pe.OperatorSafeMessage()
	want := "Card was declined: The card has been reported lost. The customer should contact their bank."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestPaymentError_OperatorSafeMessage_UnknownPaymentReturnsGeneric
// asserts non-decline (no DeclineCode, not Unknown) errors return the
// generic operator-safe message — never the raw SDK string.
func TestPaymentError_OperatorSafeMessage_UnknownPaymentReturnsGeneric(t *testing.T) {
	pe := &PaymentError{
		Message: "Keys for idempotent requests can only be used with the same parameters they were first used with",
	}
	got := pe.OperatorSafeMessage()
	if got != "Payment provider rejected the request. Please retry; if the problem persists, contact support." {
		t.Errorf("expected generic message, got %q", got)
	}
}

// An unknown outcome carries its own code and never reads as a refusal; a
// decline or a refusal keeps provider_error.
func TestPaymentError_UnknownOutcomeHasOwnCode(t *testing.T) {
	unknown := &PaymentError{Message: "stripe: 503", Unknown: true}
	if got := unknown.OperatorErrorCode(); got != "payment_outcome_unknown" {
		t.Errorf("unknown code: got %q", got)
	}
	if got := unknown.OperatorSafeMessage(); !strings.Contains(got, "didn't confirm whether the charge went through") {
		t.Errorf("unknown message: got %q", got)
	}
	for _, pe := range []*PaymentError{{Message: "x", DeclineCode: "card_declined"}, {Message: "x"}} {
		if got := pe.OperatorErrorCode(); got != "" {
			t.Errorf("decline/refusal must keep provider_error, got %q", got)
		}
	}
}
