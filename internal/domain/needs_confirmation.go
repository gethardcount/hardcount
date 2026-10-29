package domain

import "errors"

// AuthenticationRequiredCode is Stripe's code for an off-session charge the
// customer's bank wants the customer to confirm, usually with 3-D Secure
// ("The card was declined because the transaction requires authentication",
// https://docs.stripe.com/declines/codes). Stripe sends it as the decline
// code, and as the error code itself
// (https://docs.stripe.com/error-codes); the payment package reads both.
//
// The card works: the customer confirms the payment on the hosted invoice
// page, where Stripe runs 3-D Secure. What cannot work is another charge
// without the customer, so Hardcount follows Stripe Billing, which lists the
// code among those it does not retry: "the scheduled retries continue but
// the payment only executes if you obtain a new payment method"
// (https://docs.stripe.com/billing/revenue-recovery/smart-retries#non-retryable-decline-codes).
// Stripe Billing then emails the customer a link to confirm the payment, and
// reminders until they do
// (https://docs.stripe.com/billing/revenue-recovery/customer-emails#payment-confirmation-notifications).
// Hardcount's payment-failed email and each dunning reminder carry the hosted
// invoice link with the same request.
const AuthenticationRequiredCode = "authentication_required"

// IsAuthenticationRequired reports whether a Stripe decline code says the
// bank wants the customer to confirm the payment.
func IsAuthenticationRequired(declineCode string) bool {
	return declineCode == AuthenticationRequiredCode
}

// ErrCardNeedsConfirmation is what a dunning retry returns when it charged
// nothing because the bank asked the customer to confirm the last charge of
// the customer's default card on this invoice. Like ErrCardNeedsReplacing it
// provably never reached Stripe and still counts as an attempt (Stripe
// Billing keeps its schedule and attempt count); the next retry charges a
// different card once it is the default.
var ErrCardNeedsConfirmation = errors.New("the bank asked the customer to confirm the payment; an off-session retry cannot")

// DeclineAsk is what a failed card payment, or a retry that found no card,
// asks of the customer, and the copy their emails use for it.
type DeclineAsk string

const (
	// DeclineAskNone: an ordinary decline. Pay the invoice or update the
	// card; dunning retries the same card.
	DeclineAskNone DeclineAsk = ""
	// DeclineAskNewCard: the bank refused the card for good (IsHardDecline).
	// Pay with a different card; no retry charges this one.
	DeclineAskNewCard DeclineAsk = "new_card"
	// DeclineAskConfirm: the bank wants the customer to confirm the payment
	// (IsAuthenticationRequired). Pay on the hosted invoice page, where the
	// bank asks for the confirmation; no retry charges this card.
	DeclineAskConfirm DeclineAsk = "confirm"
	// DeclineAskAddCard: no card is on file, so the retry charged nothing
	// (ErrNoPaymentMethodOnRetry). Nothing was declined and no retry can
	// succeed until a card is added, so the email asks the customer to pay
	// the invoice and promises no retry.
	DeclineAskAddCard DeclineAsk = "add_card"
)

// DeclineAskOf is the ask for a Stripe decline code.
func DeclineAskOf(declineCode string) DeclineAsk {
	switch {
	case IsHardDecline(declineCode):
		return DeclineAskNewCard
	case IsAuthenticationRequired(declineCode):
		return DeclineAskConfirm
	}
	return DeclineAskNone
}

// RetryBlock reads the decline code a store found for the card a dunning
// retry is about to charge (see invoice.PostgresStore.RetryBlockingDecline):
// ErrCardNeedsReplacing for a hard decline, ErrCardNeedsConfirmation for a
// request to confirm, nil when the card may be charged.
func RetryBlock(declineCode string) error {
	switch DeclineAskOf(declineCode) {
	case DeclineAskNewCard:
		return ErrCardNeedsReplacing
	case DeclineAskConfirm:
		return ErrCardNeedsConfirmation
	}
	return nil
}
