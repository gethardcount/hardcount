package activity

import "strings"

// Credit-note reasons the engine and the refund form write as codes. Any
// other reason is the operator's own text and is shown as written.
var creditNoteReasons = map[string]string{
	"subscription_downgrade":         "Plan downgrade",
	"subscription_quantity_decrease": "Quantity decrease",
	"subscription_item_removed":      "Item removed",
	"subscription_cancellation":      "Cancellation",
	"subscription_plan_change":       "Plan change",
	"commit_relief":                  "Unused prepaid credits refunded",
	// The refund form's reasons (Stripe's refund reasons, plus other).
	"duplicate":             "Duplicate",
	"fraudulent":            "Fraudulent",
	"requested_by_customer": "Requested by customer",
	"other":                 "Other",
}

// CreditNoteReason labels a credit note's reason.
func CreditNoteReason(reason string) string {
	if l, ok := creditNoteReasons[reason]; ok {
		return l
	}
	return reason
}

// CreditNoteReasonCodes lists the coded reasons, for tests.
func CreditNoteReasonCodes() []string {
	out := make([]string, 0, len(creditNoteReasons))
	for c := range creditNoteReasons {
		out = append(out, c)
	}
	return out
}

// RefundParts says where a credit note's total went: each part that is not
// zero, as a meta segment.
func RefundParts(refundCents, creditCents, outOfBandCents int64, currency string) []Segment {
	var out []Segment
	if refundCents > 0 {
		out = append(out, Segment{M(refundCents, currency), T(" refunded to card")})
	}
	if creditCents > 0 {
		out = append(out, Segment{M(creditCents, currency), T(" to credit balance")})
	}
	if outOfBandCents > 0 {
		out = append(out, Segment{M(outOfBandCents, currency), T(" refunded outside Hardcount")})
	}
	return out
}

// Decline reasons, written about the cardholder, not to them: "Card
// declined: insufficient funds", not the provider's "Your card has
// insufficient funds." The provider's "your" means the operator on the
// operator's screen.
var declineReasons = map[string]string{
	"insufficient_funds": "Card declined: insufficient funds",
	"expired_card":       "Card declined: expired card",
	"incorrect_cvc":      "Card declined: incorrect security code",
	"invalid_cvc":        "Card declined: incorrect security code",
	"lost_card":          "Card declined: reported lost or stolen",
	"stolen_card":        "Card declined: reported lost or stolen",
	"generic_decline":    "Card declined by the bank",
	"card_declined":      "Card declined by the bank",
	"do_not_honor":       "Card declined by the bank",
	// Not a decline of the card: the bank wants the customer to confirm the
	// payment (3-D Secure), which a charge made without them can't do.
	// Stripe's own message for it is written for developers ("If the
	// customer is off session, ask them to return to your application").
	"authentication_required": "The bank asked the customer to confirm this payment",
}

// DeclineCodes lists the mapped decline codes, for tests.
func DeclineCodes() []string {
	out := make([]string, 0, len(declineReasons))
	for c := range declineReasons {
		out = append(out, c)
	}
	return out
}

// DeclineReason is a failed charge's problem line: the mapped decline code
// when there is one, else the provider's own text without Hardcount's wrappers.
func DeclineReason(declineCode, providerText string) string {
	if r, ok := declineReasons[strings.ToLower(strings.TrimSpace(declineCode))]; ok {
		return r
	}
	return StripWrapper(providerText)
}

// wrappers are the prefixes the charge path puts in front of the provider's
// message (payment.Stripe's "%s: %w" with its verb), which the dunning retry
// event stores as its reason.
var wrappers = []string{"payment failed: ", "payment state unknown: "}

// StripWrapper removes Hardcount's own wrapper from a provider's message
// ("payment failed: Your card was declined." → "Your card was declined."),
// so only the provider's words reach the screen.
func StripWrapper(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range wrappers {
		if len(s) > len(p) && strings.EqualFold(s[:len(p)], p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	return s
}
