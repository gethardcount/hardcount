package domain

import "time"

// DisputeStatus is Stripe's Dispute.status, stored as Stripe sends it
// (https://docs.stripe.com/api/disputes/object#dispute_object-status). The
// warning_* values are an inquiry: the card network asks about the payment
// before any chargeback. The other values are a chargeback.
//
// The column's CHECK lists the same eight values (migration 00185). A status
// Stripe adds later is refused by the webhook handler, loudly, until both are
// extended together.
type DisputeStatus string

const (
	DisputeWarningNeedsResponse DisputeStatus = "warning_needs_response"
	DisputeWarningUnderReview   DisputeStatus = "warning_under_review"
	DisputeWarningClosed        DisputeStatus = "warning_closed"
	DisputeNeedsResponse        DisputeStatus = "needs_response"
	DisputeUnderReview          DisputeStatus = "under_review"
	DisputeWon                  DisputeStatus = "won"
	DisputeLost                 DisputeStatus = "lost"
	DisputePrevented            DisputeStatus = "prevented"
)

// Valid reports whether s is one of the eight statuses Velox stores.
func (s DisputeStatus) Valid() bool {
	return false // failing-first skeleton: not implemented
}

// Closed reports whether the dispute has ended. Stripe sends
// charge.dispute.closed for won, lost and warning_closed; prevented (stopped
// before it became a chargeback) is an end state too.
func (s DisputeStatus) Closed() bool {
	return false // failing-first skeleton: not implemented
}

// InvoiceDispute is a card dispute (chargeback) Stripe reported against one
// of the invoice's payments (#979). Velox records it and reports it; it never
// changes the invoice's payment status and never moves money because of it.
// The operator answers the dispute in Stripe, and decides what a lost one
// means for the invoice.
//
// One row per Stripe dispute. Each charge.dispute.* event carries the whole
// dispute as it stood when the event was created; the row keeps the newest
// one (see SupersededBy), so events can arrive in any order.
type InvoiceDispute struct {
	ID                    string        `json:"id"`
	InvoiceID             string        `json:"invoice_id"`
	StripeDisputeID       string        `json:"stripe_dispute_id"`
	StripeChargeID        string        `json:"stripe_charge_id,omitempty"`
	StripePaymentIntentID string        `json:"stripe_payment_intent_id"`
	Status                DisputeStatus `json:"status"`
	// Reason is the reason the cardholder gave (Stripe's Dispute.reason, for
	// example fraudulent or product_not_received).
	Reason string `json:"reason,omitempty"`
	// AmountCents is the disputed amount in Currency's minor units. Stripe
	// notes it can differ from the payment (a partial dispute, a currency
	// conversion).
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	// EvidenceDueBy is the deadline to answer the dispute in Stripe. Nil when
	// Stripe gave none.
	EvidenceDueBy *time.Time `json:"evidence_due_by,omitempty"`
	// OpenedAt is when Stripe created the dispute.
	OpenedAt time.Time `json:"opened_at"`
	// ClosedAt is when Stripe closed it: the creation time of the event that
	// first carried its closed status. Nil while it is open.
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	// LastEventAt and LastStripeEventID name the Stripe event whose snapshot
	// the row holds. They decide whether a later event is newer.
	LastEventAt       time.Time `json:"-"`
	LastStripeEventID string    `json:"-"`
	Livemode          bool      `json:"livemode"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// DisputeSnapshot is the dispute as one charge.dispute.* event carries it.
type DisputeSnapshot struct {
	StripeEventID string
	// EventAt is the event's creation time (Stripe's event.created), the
	// snapshot's version.
	EventAt               time.Time
	StripeDisputeID       string
	StripeChargeID        string
	StripePaymentIntentID string
	Status                DisputeStatus
	Reason                string
	AmountCents           int64
	Currency              string
	EvidenceDueBy         *time.Time
	// OpenedAt is the dispute's own creation time (Dispute.created).
	OpenedAt time.Time
}

// SupersededBy reports whether snapshot s is newer than what the row holds,
// so it should replace it. Stripe does not deliver events in order, so the
// event's creation time decides, not the arrival order. Its precision is one
// second; within one second the later stage of the dispute's life wins. The
// event the row already holds is never newer than itself, which makes a
// redelivery a no-op.
func (d InvoiceDispute) SupersededBy(s DisputeSnapshot) bool {
	return false // failing-first skeleton: not implemented
}

// DisputeRecordResult says what one snapshot did.
type DisputeRecordResult struct {
	Dispute InvoiceDispute
	// Opened: this snapshot recorded the dispute for the first time, and
	// payment.dispute_opened was queued with it.
	Opened bool
	// Closed: this snapshot moved the dispute to a closed status it did not
	// have (open to closed, or lost to won, a "late win"), and
	// payment.dispute_closed was queued with it.
	Closed bool
	// Applied: the snapshot was newer than the row and was written. False
	// for a redelivery or an older event arriving late.
	Applied bool
}
