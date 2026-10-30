package domain

import (
	"errors"
	"fmt"
)

// NotifyOutcome is the typed disposition of a customer-notification effect.
// Effect interfaces return it INSTEAD of a bare nil so a policy skip (e.g.
// "customer has no email — nothing to send") is observable by the caller
// rather than indistinguishable from a successful send. The rule this
// enforces (2026-07-10 design review, principle amendment): returning nil for
// a skipped effect is forbidden — no surface may assert an effect whose
// outcome it cannot read. Prospective for new effect interfaces; retrofitted
// to NotifyNoPaymentMethod, whose silent skip produced two lying surfaces
// (the "has been emailed" banner, #403, and a resend endpoint answering
// 200 {"status":"sent"} for a send that never happened).
type NotifyOutcome string

const (
	// NotifySent: the notification was handed to the delivery layer
	// (the email outbox). Delivery itself is asynchronous — "sent" here
	// means queued-with-intent, and the outbox/timeline is the delivery
	// record.
	NotifySent NotifyOutcome = "sent"
	// NotifySkippedNoEmail: policy skip — the customer has no email
	// address on file, so there is nothing to send to. Not an error:
	// callers branch on it (the engine logs it; the resend endpoint
	// returns a typed 409 instead of a false success).
	NotifySkippedNoEmail NotifyOutcome = "skipped_no_email"
)

// ErrNoPaymentMethodOnRetry is the typed failure a dunning charge-retry
// returns when the customer still has no payment method — the retry
// provably never reached the provider. Typed (2026-07-22 payment-
// surfacing audit, P2-3) so the dunning warning email can render clean
// customer copy instead of leaking the internal system-perspective
// string ("no payment method for customer") into a customer inbox.
var ErrNoPaymentMethodOnRetry = errors.New("no payment method for customer")

// ErrNoOnlinePaymentOnRetry is what a dunning retry returns when Stripe is
// not connected for the invoice's mode (ADR-060 amendment 2026-10-30): no
// charge was attempted, whatever card is on file, and none can be until
// Stripe is connected. It wraps ErrNoPaymentMethodOnRetry, so errors.Is
// matches both: the attempt counts (the run still reaches its final action)
// and the timeline shows it as a retry that charged nothing. Only the
// customer's reminder differs: it asks them to pay the invoice, never to add
// a card (DeclineAskPayInvoice).
var ErrNoOnlinePaymentOnRetry = fmt.Errorf("stripe is not connected for this mode: %w", ErrNoPaymentMethodOnRetry)
