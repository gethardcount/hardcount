package domain

import (
	"errors"
	"fmt"
)

// NotifyOutcome says what a customer-notification effect did. Effect
// interfaces return it instead of a bare nil, so the caller can tell a policy
// skip (for example, "the customer has no email, so there is nothing to
// send") from a successful send. The rule: returning nil for a skipped effect
// is forbidden, because no surface may claim an effect whose outcome it
// cannot read. New effect interfaces follow it, and so does
// NotifyNoPaymentMethod. A silent skip there would let two surfaces claim a
// send that never happened: the "has been emailed" banner (#403), and a
// resend endpoint answering 200 {"status":"sent"}.
type NotifyOutcome string

const (
	// NotifySent: the notification was handed to the delivery layer (the
	// email outbox). Delivery itself is asynchronous, so "sent" here means
	// queued to be sent. The outbox and the timeline are the delivery record.
	NotifySent NotifyOutcome = "sent"
	// NotifySkippedNoEmail: a policy skip. The customer has no email address
	// on file, so there is nothing to send to. It is not an error, and
	// callers branch on it. The engine logs it, and the resend endpoint
	// returns a typed 409 instead of a false success.
	NotifySkippedNoEmail NotifyOutcome = "skipped_no_email"
)

// ErrNoPaymentMethodOnRetry is the typed failure a dunning charge retry
// returns when the customer still has no payment method, so the retry
// provably never reached the provider. It is typed so the dunning warning
// email can show clean customer copy. Otherwise the internal message, written
// from the system's side ("no payment method for customer"), would reach the
// customer's inbox.
var ErrNoPaymentMethodOnRetry = errors.New("no payment method for customer")

// ErrNoOnlinePaymentOnRetry is what a dunning retry returns when Stripe is
// not connected for the invoice's mode (ADR-060, amendment of 2026-10-30). No
// charge was attempted, whatever card is on file, and none can be until
// Stripe is connected. It wraps ErrNoPaymentMethodOnRetry, so errors.Is
// matches both. The attempt counts (the run still reaches its final action),
// and the timeline shows it as a retry that charged nothing. Only the
// customer's reminder differs: it asks them to pay the invoice, never to add
// a card (DeclineAskPayInvoice).
var ErrNoOnlinePaymentOnRetry = fmt.Errorf("stripe is not connected for this mode: %w", ErrNoPaymentMethodOnRetry)
