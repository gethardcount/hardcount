package payment

import "strings"

// stripeDeclineHumanMessage maps Stripe's machine-readable decline_code
// values to operator-facing English. Sourced from Stripe's official
// documentation: https://docs.stripe.com/declines/codes
//
// Why a curated map rather than mechanical title-case humanization:
//   - Some codes don't read as English when title-cased ("Do not honor.",
//     "Lost card.", "Pickup card." — terse and ambiguous).
//   - Stripe's docs already provide operator-safe phrasings; using them
//     keeps Hardcount's UI consistent with how Stripe's own dashboard
//     renders the same codes.
//   - Operator copy is uniform regardless of which code surfaces (no
//     mix of "Insufficient funds" and "Pickup card.").
//
// Codes not in this map fall back to title-case humanization via
// humanizeDeclineCode — covers new codes Stripe adds without
// requiring a code update.
var stripeDeclineHumanMessage = map[string]string{
	// Generic / issuer rejection.
	"card_declined":   "The card was declined by the issuer.",
	"do_not_honor":    "The card was declined by the issuer.",
	"generic_decline": "The card was declined.",
	"call_issuer":     "The customer should contact their bank.",

	// Card data / format issues.
	"incorrect_number":    "The card number is incorrect.",
	"incorrect_cvc":       "The security code is incorrect.",
	"invalid_cvc":         "The security code is invalid.",
	"invalid_number":      "The card number is invalid.",
	"invalid_expiry_year": "The card's expiry year is invalid.",
	"invalid_amount":      "The amount is invalid for this card.",
	"expired_card":        "The card has expired.",

	// Funds / limit.
	"insufficient_funds":              "Insufficient funds.",
	"card_velocity_exceeded":          "The card has exceeded its credit limit.",
	"withdrawal_count_limit_exceeded": "The card's withdrawal limit was reached.",

	// Card status.
	"lost_card":       "The card has been reported lost. The customer should contact their bank.",
	"stolen_card":     "The card has been reported stolen. The customer should contact their bank.",
	"pickup_card":     "The card cannot be used. The customer should contact their bank.",
	"restricted_card": "The card has restrictions and cannot be used for this purchase.",
	"fraudulent":      "The card was declined as fraudulent.",

	// Service / processing.
	"processing_error":        "An error occurred processing the card. Please retry.",
	"try_again_later":         "The issuer asked us to try again later. Please retry shortly.",
	"reenter_transaction":     "The transaction needs to be re-attempted. Please retry.",
	"issuer_not_available":    "The card issuer couldn't be reached. Please retry shortly.",
	"transaction_not_allowed": "The card cannot be used for this transaction.",
	"service_not_allowed":     "The card cannot be used for this service.",
	"not_permitted":           "The card cannot be used for this transaction.",
	"currency_not_supported":  "The card doesn't support this currency.",
	"card_not_supported":      "The card type isn't supported for this transaction.",

	// Authentication / PIN.
	"incorrect_pin":                  "The PIN is incorrect.",
	"invalid_pin":                    "The PIN is invalid.",
	"pin_try_exceeded":               "The card's PIN-attempt limit was reached.",
	"online_or_offline_pin_required": "The card requires PIN entry.",
	"offline_pin_required":           "The card requires PIN entry.",

	// Test mode.
	"testmode_decline": "Test card declined as expected.",
}

// declineCodeToOperatorMessage returns the operator-facing message for
// a Stripe decline_code. Returns empty when code is empty (caller
// should fall back to a generic "Card was declined" message).
func declineCodeToOperatorMessage(code string) string {
	if code == "" {
		return ""
	}
	if msg, ok := stripeDeclineHumanMessage[strings.ToLower(code)]; ok {
		return msg
	}
	// Fallback: title-case the snake_case code. Covers new codes
	// Stripe adds without requiring a code update; the result reads
	// reasonably for most codes (e.g. a future
	// "verification_required" → "Verification required.").
	return humanizeDeclineCode(code) + "."
}
