package domain

import (
	"errors"
	"sort"
)

// hardDeclineCodes are the Stripe decline codes after which Velox does not
// charge the same card again for the same invoice (#966, ADR-126).
//
// The list is Stripe Billing's own: "Stripe can't automatically retry a
// payment if the card issuer returns any of these hard decline codes"
// (https://docs.stripe.com/billing/revenue-recovery/smart-retries#non-retryable-decline-codes).
// Visa's "issuer will never approve" category, where any reattempt costs the
// merchant a fee, covers the same ground: pick up card (04), invalid account
// number (14), lost (41), stolen (43), transaction not permitted (57) and the
// revoked recurring authorizations (R1, R3).
//
// One code on Stripe's list is left out: authentication_required. That
// decline asks for the customer to confirm the payment (3-D Secure), not for
// a new card, and the "use a different card" copy would tell them the wrong
// thing. It keeps the ordinary retry schedule, as it did before.
//
// Not on the list, so still retried on schedule: fraudulent and
// merchant_blacklist (Stripe Radar's decisions, not the bank's),
// restricted_card, stop_payment_order, expired_card and every other code.
// Stripe's retry rule does not treat them as final either.
var hardDeclineCodes = map[string]bool{
	"incorrect_number":                 true,
	"lost_card":                        true,
	"pickup_card":                      true,
	"stolen_card":                      true,
	"revocation_of_authorization":      true,
	"revocation_of_all_authorizations": true,
	"highest_risk_level":               true,
	"transaction_not_allowed":          true,
}

// IsHardDecline reports whether a Stripe decline code means the bank refused
// the card for good, so retrying the same card cannot succeed.
func IsHardDecline(declineCode string) bool {
	return hardDeclineCodes[declineCode]
}

// HardDeclineCodes returns the codes IsHardDecline accepts, sorted. The
// stores pass it to SQL, so a read and the Go check use one list.
func HardDeclineCodes() []string {
	out := make([]string, 0, len(hardDeclineCodes))
	for c := range hardDeclineCodes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ErrCardNeedsReplacing is what a dunning retry returns when it charged
// nothing because the bank refused the customer's default card for good on
// this invoice. Like ErrNoPaymentMethodOnRetry, the retry provably never
// reached Stripe, and it still counts as an attempt: Stripe Billing keeps
// its schedule and attempt count for a hard decline, and charges only once
// it sees a new payment method.
var ErrCardNeedsReplacing = errors.New("the bank refused the default card for good; a different card is needed")
