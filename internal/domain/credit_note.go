package domain

import (
	"time"
)

// CreditNoteStatus is a credit note's status: draft, issued or voided.
type CreditNoteStatus string

const (
	CreditNoteDraft  CreditNoteStatus = "draft"
	CreditNoteIssued CreditNoteStatus = "issued"
	CreditNoteVoided CreditNoteStatus = "voided"
)

// RefundStatus is the state of a credit note's card refund.
type RefundStatus string

const (
	RefundNone      RefundStatus = "none"
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
	// RefundUnknown: Hardcount asked Stripe for the refund and got no clear
	// answer (a timeout, a network error, a Stripe 5xx, a key already in use
	// or a rate limit). So the customer may or may not have been refunded.
	// No Stripe status maps to it. Retry refund settles it. It looks for the
	// refund at Stripe, then resends the same request under the same
	// idempotency key, so Stripe answers with the first request's result
	// instead of making a second refund (#965). ADR-107 names the 24-hour
	// limit on the key.
	RefundUnknown RefundStatus = "unknown"
)

// CreditNote is a credit note on an invoice.
type CreditNote struct {
	ID                   string           `json:"id"`
	TenantID             string           `json:"tenant_id,omitempty"`
	InvoiceID            string           `json:"invoice_id"`
	CustomerID           string           `json:"customer_id"`
	CreditNoteNumber     string           `json:"credit_note_number"`
	Status               CreditNoteStatus `json:"status"`
	Reason               string           `json:"reason"`
	SubtotalCents        int64            `json:"subtotal_cents"`
	TaxAmountCents       int64            `json:"tax_amount_cents"`
	TotalCents           int64            `json:"total_cents"`
	RefundAmountCents    int64            `json:"refund_amount_cents"`
	CreditAmountCents    int64            `json:"credit_amount_cents"`
	OutOfBandAmountCents int64            `json:"out_of_band_amount_cents"`
	// CommitRetiredCents is how much commit credit this credit note retired,
	// its relief leg (ADR-080). It is 0 on every credit note that is not a
	// relief. It is a real column, not metadata, so auditors can reconcile
	// grant.cn_retired_cents == SUM(cn.commit_retired_cents).
	CommitRetiredCents int64        `json:"commit_retired_cents,omitempty"`
	Currency           string       `json:"currency"`
	IssuedAt           *time.Time   `json:"issued_at,omitempty"`
	VoidedAt           *time.Time   `json:"voided_at,omitempty"`
	RefundStatus       RefundStatus `json:"refund_status"`
	StripeRefundID     string       `json:"stripe_refund_id,omitempty"`
	// RecordedFromStripe marks a credit note whose card refund is a refund
	// made in Stripe outside Hardcount (ADR-134): recorded from the refund
	// webhook, or linked by the operator at create. Hardcount made no refund
	// call for it. StripeRefundID names the refund.
	RecordedFromStripe bool `json:"recorded_from_stripe"`
	// RefundUnknownKey is the idempotency key of the refund request whose
	// result is unknown. It is set exactly while RefundStatus is
	// RefundUnknown, and Retry refund resends that request with it. It is
	// internal and never sent to API callers.
	RefundUnknownKey string `json:"-"`
	// BillToSnapshot and SupplierSnapshot are the buyer and seller details
	// this credit note was issued with (ADR-109). They are taken from the
	// live customer and company rows in the transaction that issued it. They
	// are nil on drafts and on credit notes issued before migration 0177, and
	// both of those render from the live rows.
	BillToSnapshot   *DocumentBillTo   `json:"bill_to_snapshot,omitempty"`
	SupplierSnapshot *DocumentSupplier `json:"supplier_snapshot,omitempty"`
	// TaxTransactionID is the id of the reversal transaction that the tax
	// provider returns when Issue succeeds (for Stripe, tx_xxx for the
	// negative tax_transaction). It is empty while the credit note is a
	// draft. It is also empty when the invoice has no upstream tax state to
	// reverse: the provider is manual or none, or the invoice is a legacy one
	// from before invoice.tax_transaction_id.
	TaxTransactionID string `json:"tax_transaction_id,omitempty"`
	// IsSimulated marks a credit note whose issued_at is in simulated
	// (test-clock) time. It is the source invoice's flag, whoever issues the
	// note: engine clawbacks and operator issuance both run on the invoice's
	// clock (ADR-030). The wall-clock recovery sweeps skip notes that carry
	// it. On the invoice timeline it gates the Recorded subline, which shows
	// the real insert time (ADR-104).
	IsSimulated bool `json:"is_simulated"`
	// IssuePending marks an auto-issue clawback draft. The draft is created
	// in the same transaction as a subscription downgrade, item removal or
	// quantity decrease, so the item change and the clawback obligation
	// commit together. It is issued after the commit. The flag is set to true
	// only at create, and any exit from draft clears it: the status
	// transition in creditnote/postgres.go sets issue_pending=false with the
	// new status. So the API never serves issue_pending=true on an issued
	// note, and `issue_pending AND status='voided'` can never be true. To
	// find a credit note whose relief never applied, test
	// `status='voided' AND issued_at IS NULL` instead.
	//
	// RetryPendingClawbackIssue recovers a draft whose post-commit Issue()
	// never ran, for example after a crash before issuance. It scans
	// status='draft' AND issue_pending, so an issued note drops out by its
	// status, not by this flag. Issue() commits the status change and the
	// internal money effect in one transaction (ADR-061), so no issued note
	// has an internal effect still to apply. The external legs recover
	// through their own sweeps, RetryRefund and
	// RetryPendingCreditNoteTaxReversal. The flag is always false on
	// operator-created drafts.
	IssuePending bool `json:"issue_pending"`
	// TaxReversalPending marks an issued credit note that still owes its
	// upstream tax reversal. The statement that issues the note sets it, for
	// a note with an amount on an invoice Stripe Tax taxed with tax; the write
	// that stores the reversal's transaction clears it. The reversal is a Stripe
	// call made after the issue commit, so the flag is true from the commit
	// until that call succeeds: normally within the same request, longer if
	// the call fails, the process stops first, or the invoice's own tax
	// transaction is not recorded yet. RetryPendingCreditNoteTaxReversal
	// makes the reversal for marked notes with the per-note
	// hardcount_tax_rev_<cn.ID> key (ADR-061, ADR-062). A note whose invoice
	// never gets a tax transaction (its tax commit never succeeds, or it is on
	// a test clock) stays marked: nothing was reported, so nothing is
	// reversed.
	TaxReversalPending bool           `json:"tax_reversal_pending"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	// RecordedAt is the real-world instant this row was inserted (ADR-104,
	// Invariant A). CreatedAt follows the entity's calendar, which is
	// simulated time when the entity is pinned to a test clock (ADR-030).
	// Without RecordedAt the row would have no wall-clock stamp at all, and
	// the timeline could not show both calendars. It is nil on rows created
	// before migration 0164, which were never backfilled.
	RecordedAt *time.Time `json:"recorded_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// CreditNoteLineItem is one line of a credit note.
type CreditNoteLineItem struct {
	ID                string    `json:"id"`
	CreditNoteID      string    `json:"credit_note_id"`
	TenantID          string    `json:"tenant_id,omitempty"`
	InvoiceLineItemID string    `json:"invoice_line_item_id,omitempty"`
	Description       string    `json:"description"`
	Quantity          int64     `json:"quantity"`
	UnitAmountCents   int64     `json:"unit_amount_cents"`
	AmountCents       int64     `json:"amount_cents"`
	CreatedAt         time.Time `json:"created_at"`
}
