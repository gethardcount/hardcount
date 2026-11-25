package domain

import (
	"strings"
	"time"
)

// DisputeStatus is Stripe's Dispute.status, stored as Stripe sends it
// (https://docs.stripe.com/api/disputes/object#dispute_object-status). The
// warning_* values are an inquiry: the card network asks about the payment
// before any chargeback. The other values are a chargeback. Stripe sends the
// appeal_* values only when appeals are turned on: under_review moves to
// appeal_needs_response when the bank rejects the response and the dispute
// can be appealed, then to appeal_under_review once the appeal is sent, and
// the dispute closes won or lost. Closing it at appeal_needs_response makes it
// lost.
//
// The column's CHECK lists the same ten values (migration 00246). The
// webhook handler refuses, loudly, a status that Stripe adds later, until both
// lists are extended together.
type DisputeStatus string

const (
	DisputeWarningNeedsResponse DisputeStatus = "warning_needs_response"
	DisputeWarningUnderReview   DisputeStatus = "warning_under_review"
	DisputeWarningClosed        DisputeStatus = "warning_closed"
	DisputeNeedsResponse        DisputeStatus = "needs_response"
	DisputeUnderReview          DisputeStatus = "under_review"
	DisputeAppealNeedsResponse  DisputeStatus = "appeal_needs_response"
	DisputeAppealUnderReview    DisputeStatus = "appeal_under_review"
	DisputeWon                  DisputeStatus = "won"
	DisputeLost                 DisputeStatus = "lost"
	DisputePrevented            DisputeStatus = "prevented"
)

// Valid reports whether s is one of the ten statuses Hardcount stores.
func (s DisputeStatus) Valid() bool {
	return s.stage() > 0
}

// Closed reports whether the dispute has ended. Stripe sends
// charge.dispute.closed for won, lost and warning_closed. Prevented (stopped
// before it became a chargeback) is an end state too.
func (s DisputeStatus) Closed() bool {
	return s.stage() == disputeStageClosed
}

// Inquiry reports whether s is an inquiry, not a chargeback: the card
// network asks about the payment, and no money has left yet.
func (s DisputeStatus) Inquiry() bool {
	return s == DisputeWarningNeedsResponse || s == DisputeWarningUnderReview || s == DisputeWarningClosed
}

const disputeStageClosed = 7

// stage orders statuses along the dispute's life, so two snapshots stamped in
// the same second apply in the order they happened: an inquiry before the
// chargeback it became, a review before its appeal, an open dispute before its
// close. Statuses at the same stage do not order each other. 0 means not a
// status Hardcount knows. A close outranks an appeal, so a closed dispute
// never reads as open.
func (s DisputeStatus) stage() int {
	switch s {
	case DisputeWarningNeedsResponse:
		return 1
	case DisputeWarningUnderReview:
		return 2
	case DisputeNeedsResponse:
		return 3
	case DisputeUnderReview:
		return 4
	case DisputeAppealNeedsResponse:
		return 5
	case DisputeAppealUnderReview:
		return 6
	case DisputeWarningClosed, DisputeWon, DisputeLost, DisputePrevented:
		return disputeStageClosed
	default:
		return 0
	}
}

// InvoiceDispute is a card dispute (chargeback) Stripe reported against one
// of the invoice's payments (#979). Hardcount never changes the invoice's
// payment status because of it. The operator answers the dispute in Stripe.
// A loss on a top-up's own payment is recorded as a credit note that takes
// the credit back (LossTakesCredit); what any other loss means for the
// invoice is the operator's to decide.
//
// There is one row per Stripe dispute. Each charge.dispute.* event carries
// the whole dispute as it stood when the event was created. The row keeps the
// newest one (see SupersededBy), so events can arrive in any order.
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
	// notes that it can differ from the payment (a partial dispute, a
	// currency conversion).
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
	// CreditNoteID names the credit note that records this dispute's loss on
	// a top-up (LossTakesCredit). A loss is recorded only while it is empty,
	// so a dispute lost twice, after an appeal, writes one note.
	CreditNoteID string `json:"credit_note_id,omitempty"`
	// LastEventAt and LastStripeEventID name the Stripe event whose snapshot
	// the row holds. They decide whether a later event is newer.
	LastEventAt       time.Time `json:"-"`
	LastStripeEventID string    `json:"-"`
	Livemode          bool      `json:"livemode"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// OnTopupPayment reports whether d is on the own payment of inv, a paid
// top-up.
func (d InvoiceDispute) OnTopupPayment(inv Invoice) bool {
	return inv.BillingReason == BillingReasonTopup && inv.Status == InvoicePaid &&
		inv.StripePaymentIntentID != "" && d.StripePaymentIntentID == inv.StripePaymentIntentID
}

// LossTakesCredit reports whether losing d is recorded as a credit note that
// takes inv's credit back: d is on the top-up's own payment, in its
// currency. A dispute in another currency writes no note: its amount cannot
// be set against what the top-up was paid.
func (d InvoiceDispute) LossTakesCredit(inv Invoice) bool {
	return d.OnTopupPayment(inv) && strings.EqualFold(d.Currency, inv.Currency)
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
// second. Within one second, the later stage of the dispute's life wins. The
// event the row already holds is never newer than itself, which makes a
// redelivery a no-op.
func (d InvoiceDispute) SupersededBy(s DisputeSnapshot) bool {
	if s.StripeEventID != "" && s.StripeEventID == d.LastStripeEventID {
		return false
	}
	if !s.EventAt.Equal(d.LastEventAt) {
		return s.EventAt.After(d.LastEventAt)
	}
	return s.Status.stage() > d.Status.stage()
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
