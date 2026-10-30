package domain

import "time"

// StripeRefundStatus is Stripe's Refund.status, stored as Stripe sends it
// (https://docs.stripe.com/api/refunds/object#refund_object-status). The
// invoice_stripe_refunds CHECK lists the same five values (migration 00222).
// A status Stripe adds later is refused by the webhook handler, loudly, until
// both are extended together.
type StripeRefundStatus string

const (
	StripeRefundPending        StripeRefundStatus = "pending"
	StripeRefundRequiresAction StripeRefundStatus = "requires_action"
	StripeRefundSucceeded      StripeRefundStatus = "succeeded"
	StripeRefundFailed         StripeRefundStatus = "failed"
	StripeRefundCanceled       StripeRefundStatus = "canceled"
)

// Valid reports whether s is one of the five statuses Hardcount stores.
func (s StripeRefundStatus) Valid() bool { return s.stage() > 0 }

// MovedNoMoney reports whether the refund ended without returning money to
// the customer: it failed, or the operator canceled it in time. Either way
// the money went back to the Stripe balance.
func (s StripeRefundStatus) MovedNoMoney() bool {
	return s == StripeRefundFailed || s == StripeRefundCanceled
}

// CreditNoteRefundStatus is the credit note's refund_status for a refund in
// this Stripe status, the mapping ADR-063 set: canceled counts as failed (the
// money went back to the Stripe balance), requires_action as pending.
func (s StripeRefundStatus) CreditNoteRefundStatus() RefundStatus {
	switch s {
	case StripeRefundSucceeded:
		return RefundSucceeded
	case StripeRefundFailed, StripeRefundCanceled:
		return RefundFailed
	default:
		return RefundPending
	}
}

// stage orders statuses along a refund's life, so two snapshots stamped in
// the same second apply in the order they happened. 0 means not a status
// Hardcount knows.
func (s StripeRefundStatus) stage() int {
	switch s {
	case StripeRefundPending, StripeRefundRequiresAction:
		return 1
	case StripeRefundSucceeded:
		return 2
	case StripeRefundFailed, StripeRefundCanceled:
		return 3
	default:
		return 0
	}
}

// StripeRefundOutcome is what Hardcount did with a refund made in Stripe
// outside Hardcount (ADR-134). The column's CHECK lists the same values.
type StripeRefundOutcome string

const (
	// StripeRefundRecordedAsCreditNote: recorded as an issued credit note
	// whose card refund is this refund. Hardcount made no refund call for it.
	StripeRefundRecordedAsCreditNote StripeRefundOutcome = "credit_note"
	// StripeRefundOtherPayment: the refund is of a payment that did not pay
	// the invoice (a second charge, or money on a voided invoice). That money
	// was never on the invoice, so no credit note.
	StripeRefundOtherPayment StripeRefundOutcome = "other_payment"
	// StripeRefundNeedsReview: a refund of the invoice's own payment that
	// Hardcount did not record; ReviewReason says why. The operator records
	// it with a credit note or dismisses it.
	StripeRefundNeedsReview StripeRefundOutcome = "needs_review"
	// StripeRefundNotRefunded: first seen already failed or canceled. No
	// money moved.
	StripeRefundNotRefunded StripeRefundOutcome = "not_refunded"
	// StripeRefundDismissed: a needs_review refund the operator said was
	// handled outside Hardcount.
	StripeRefundDismissed StripeRefundOutcome = "dismissed"
)

// StripeRefundReviewReason says why a refund of the invoice's own payment was
// flagged instead of recorded as a credit note (ADR-134). The column's CHECK
// lists the same values.
type StripeRefundReviewReason string

const (
	// The invoice was not paid in Hardcount when the refund was first seen,
	// and the refund was older than the race window.
	StripeRefundReviewInvoiceNotPaid StripeRefundReviewReason = "invoice_not_paid"
	// The refund's currency is not the invoice's.
	StripeRefundReviewCurrencyMismatch StripeRefundReviewReason = "currency_mismatch"
	// The invoice bought prepaid credit: an ordinary credit note is refused
	// there (ADR-078 D4), and the credit stays spendable until commit relief
	// retires it.
	StripeRefundReviewPrepaidCommit StripeRefundReviewReason = "prepaid_commit"
	// Hardcount first saw the refund more than StripeRefundLateAfter after
	// Stripe made it, so the operator may already have recorded it by hand.
	StripeRefundReviewSeenLate StripeRefundReviewReason = "seen_late"
	// The refund is more than the card payment has left to refund after the
	// credit notes already on the invoice.
	StripeRefundReviewExceedsCardRefundable StripeRefundReviewReason = "exceeds_card_refundable"
	// The refund is more than the invoice has left to credit: an earlier
	// credit note already gave part of it back another way.
	StripeRefundReviewExceedsCreditable StripeRefundReviewReason = "exceeds_creditable"
)

// StripeRefundLateAfter is how long after Stripe made a refund Hardcount may
// first see it and still record it as a credit note (ADR-134). Later, it is
// flagged instead: after an upgrade Stripe can send refund.updated for a
// refund made before, which the operator may already have recorded by hand,
// and a second credit note would count it twice. Both instants are Stripe's
// (the event's creation time and the refund's).
const StripeRefundLateAfter = 24 * time.Hour

// InvoiceStripeRefund is a refund made in Stripe outside Hardcount on one of
// the invoice's payments (ADR-134). One row per Stripe refund. Each refund
// event carries the whole refund as it stood when the event was created; the
// row keeps the newest one (SupersededBy), so events can arrive in any order.
// The outcome is decided once, when Hardcount first sees the refund, and only
// the operator changes it after that (record it as a credit note, or
// dismiss it).
type InvoiceStripeRefund struct {
	ID                    string `json:"id"`
	InvoiceID             string `json:"invoice_id"`
	StripeRefundID        string `json:"stripe_refund_id"`
	StripePaymentIntentID string `json:"stripe_payment_intent_id"`
	StripeChargeID        string `json:"stripe_charge_id,omitempty"`
	// AmountCents is the refunded amount in Currency's minor units.
	AmountCents int64              `json:"amount_cents"`
	Currency    string             `json:"currency"`
	Status      StripeRefundStatus `json:"status"`
	// Reason is Stripe's Refund.reason (duplicate, fraudulent,
	// requested_by_customer), empty when the operator gave none.
	Reason string `json:"reason,omitempty"`
	// FailureReason is Stripe's Refund.failure_reason, set once it failed.
	FailureReason string                   `json:"failure_reason,omitempty"`
	Outcome       StripeRefundOutcome      `json:"outcome"`
	ReviewReason  StripeRefundReviewReason `json:"review_reason,omitempty"`
	// CreditNoteID names the credit note that records the refund, when the
	// outcome is credit_note.
	CreditNoteID  string     `json:"credit_note_id,omitempty"`
	DismissedNote string     `json:"dismissed_note,omitempty"`
	DismissedAt   *time.Time `json:"dismissed_at,omitempty"`
	// RefundCreatedAt is when Stripe made the refund.
	RefundCreatedAt time.Time `json:"refund_created_at"`
	// LastEventAt and LastStripeEventID name the Stripe event whose snapshot
	// the row holds. They decide whether a later event is newer.
	LastEventAt       time.Time `json:"-"`
	LastStripeEventID string    `json:"-"`
	Livemode          bool      `json:"livemode"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// StripeRefundSnapshot is a refund as one refund webhook carries it.
type StripeRefundSnapshot struct {
	StripeEventID string
	// EventAt is the event's creation time (Stripe's event.created), the
	// snapshot's version.
	EventAt               time.Time
	StripeRefundID        string
	StripePaymentIntentID string
	StripeChargeID        string
	AmountCents           int64
	// Currency as Stripe sends it (lower case); the store upper-cases it.
	Currency      string
	Status        StripeRefundStatus
	Reason        string
	FailureReason string
	// CreatedAt is the refund's own creation time (Refund.created).
	CreatedAt time.Time
	// Young is true while the refund is younger than the webhook race window
	// on the wall clock, or its age is unknown: the invoice's own payment may
	// not be recorded yet, so an unpaid invoice asks Stripe to deliver the
	// event again instead of flagging it.
	Young bool
}

// SeenLate reports whether this event, the first Hardcount sees for the
// refund, came more than StripeRefundLateAfter after Stripe made it.
func (s StripeRefundSnapshot) SeenLate() bool {
	return !s.CreatedAt.IsZero() && s.EventAt.Sub(s.CreatedAt) > StripeRefundLateAfter
}

// SupersededBy reports whether snapshot s is newer than what the row holds.
// Stripe does not deliver events in order, so the event's creation time
// decides; within one second the later stage of the refund's life wins. The
// event the row already holds is never newer than itself, which makes a
// redelivery a no-op.
func (r InvoiceStripeRefund) SupersededBy(s StripeRefundSnapshot) bool {
	if s.StripeEventID != "" && s.StripeEventID == r.LastStripeEventID {
		return false
	}
	if !s.EventAt.Equal(r.LastEventAt) {
		return s.EventAt.After(r.LastEventAt)
	}
	return s.Status.stage() > r.Status.stage()
}

// StripeRefundRecordResult says what one refund snapshot did.
type StripeRefundRecordResult struct {
	// Refund is the row, nil when a credit note already held the refund and
	// no row exists (Hardcount's own refund from before refunds carried
	// hardcount_cn_id, or one the operator linked by hand).
	Refund *InvoiceStripeRefund
	// CreditNoteID is the credit note that holds the refund, if any.
	CreditNoteID string
	// Recorded: this snapshot was the first sighting. The row was written and
	// payment.refunded_in_stripe queued with it.
	Recorded bool
	// Applied: the snapshot changed something (the row or a credit note's
	// refund status). False for a redelivery or an older event.
	Applied bool
}

// StripeRefundFacts is a refund as a read from Stripe returns it, for the
// operator's link of a refund to the credit note they create.
type StripeRefundFacts struct {
	ID              string
	PaymentIntentID string
	AmountCents     int64
	// Currency as Stripe sends it (lower case).
	Currency string
	Status   StripeRefundStatus
	// HardcountCreditNoteID is the refund's hardcount_cn_id metadata: set on
	// every refund Hardcount made since 2026-08-04 (ADR-063 amendment).
	HardcountCreditNoteID string
}
