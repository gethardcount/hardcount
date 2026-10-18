package domain

import "time"

// InterruptedCharge is a charge record (ADR-127) old enough for the
// reconciler to check with Stripe: a charge Velox recorded on the invoice
// before it called Stripe, whose result was never saved. A crash, a deploy or
// the caller's deadline ending during the Stripe call leaves one.
//
// It carries only what the check needs: where the record is, which attempt
// it is (Seq, the charge_attempt_seq the key was built from), the exact
// idempotency key Stripe received (Stripe also has it as the PaymentIntent's
// velox_attempt_key metadata), and the Stripe customer the PaymentIntent was
// created for.
type InterruptedCharge struct {
	TenantID         string
	InvoiceID        string
	StartedAt        time.Time
	Seq              int64
	Key              string
	StripeCustomerID string
	// PastGiveUp is true once the record is older than the give-up age, on
	// the database's clock. A record Stripe could not be asked about by then
	// parks the invoice (ADR-107).
	PastGiveUp bool
}
