package domain

import "time"

// CreditEntryType is the kind of a credit ledger entry: a grant, a usage, an
// expiry or an adjustment.
type CreditEntryType string

const (
	CreditGrant      CreditEntryType = "grant"
	CreditUsage      CreditEntryType = "usage"
	CreditExpiry     CreditEntryType = "expiry"
	CreditAdjustment CreditEntryType = "adjustment"
)

// CreditLedgerEntry is one row of a customer's credit ledger.
type CreditLedgerEntry struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id,omitempty"`
	CustomerID string          `json:"customer_id"`
	EntryType  CreditEntryType `json:"entry_type"`
	// Currency is the row's own ISO-4217 code, uppercase (ADR-129). A credit
	// pays only invoices in its currency.
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
	// BalanceAfter is the customer's balance in this row's currency after
	// the row.
	BalanceAfter int64          `json:"balance_after"`
	Description  string         `json:"description"`
	InvoiceID    string         `json:"invoice_id,omitempty"`
	ExpiresAt    *time.Time     `json:"expires_at,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	// ConsumedCents is how much of a positive block's amount is retired. It
	// counts what usage and clawback entries drained from the block, first
	// in, first out (FIFO), plus the part written off by expiry (Orb's
	// credit-block model). It means something only on positive blocks
	// (grants, positive adjustments, reversals), and it is 0 on usage and
	// expiry rows. A block's remaining amount is amount_cents -
	// consumed_cents. When consumed_cents == amount_cents, the block is used
	// up or expired, and the queries for draining and for the expiry sweep
	// leave it out. Expiry sets consumed_cents to amount_cents in the same
	// transaction as its -remaining ledger entry (ExpireGrantAtomic,
	// ADR-071). So a backdated apply can never drain a retired grant again.
	//
	// A return row (a void or a credit note handing back drawn credit,
	// ADR-132) is written with consumed_cents set to the part it restored to
	// the blocks the invoice drew from. That part is spendable on those
	// blocks, not on the return row. Its metadata names them
	// (restored_blocks).
	ConsumedCents int64 `json:"consumed_cents,omitempty"`

	// SourceSubscriptionID, SourceSubscriptionItemID, SourcePlanChangedAt and
	// SourceChangeType, when all are set, mark this entry as a proration
	// credit from an item change (a downgrade plan change, a quantity
	// decrease or an item removal). With TenantID they form the natural key
	// that deduplicates retries of a proration grant. The item id and
	// change_type tell apart simultaneous changes of different kinds that
	// share a wall-clock timestamp.
	SourceSubscriptionID     string         `json:"source_subscription_id,omitempty"`
	SourceSubscriptionItemID string         `json:"source_subscription_item_id,omitempty"`
	SourcePlanChangedAt      *time.Time     `json:"source_plan_changed_at,omitempty"`
	SourceChangeType         ItemChangeType `json:"source_change_type,omitempty"`

	// SourceCreditNoteID deduplicates the grants that credit-note Issue()
	// creates. Every grant that the credit note's refund-routing path appends
	// sets it. The partial unique index idx_credit_ledger_credit_note_dedup
	// allows one grant per (tenant, credit note). A second grant for the same
	// note hits the index, and the store returns ErrAlreadyExists.
	SourceCreditNoteID string `json:"source_credit_note_id,omitempty"`

	// SourceInvoiceReversalID deduplicates the credit reversals of an invoice
	// void. credit.ReverseForInvoiceTx (invoice void) stamps the voided
	// invoice's id here. The partial unique index
	// idx_credit_ledger_reversal_dedup allows one reversal grant per (tenant,
	// invoice). A second reversal of the same invoice hits the index, the
	// store returns ErrAlreadyExists, and ReverseForInvoiceTx returns that
	// error. The conflict aborts the caller's transaction, so the void fails
	// instead of crediting twice.
	SourceInvoiceReversalID string `json:"source_invoice_reversal_id,omitempty"`

	// GrantKind classifies a grant's cost basis (ADR-078). 'commit' is a
	// purchased prepaid commit, funded by a real invoice. 'promotional' is
	// credit explicitly marked free (marketing credit), and it drains before
	// every paid-class block (zero cost basis first). Empty or NULL is a
	// legacy or unclassified grant (proration credits, credit-note grants,
	// goodwill). Those are liabilities that came from money, so they drain in
	// the paid class with commits. Only drain order and reporting read this
	// tag. Balance math ignores the kind.
	GrantKind GrantKind `json:"grant_kind,omitempty"`

	// SourceInvoiceID links a commit grant to the invoice that funds it
	// (ADR-078). It differs from InvoiceID, which usage and reversal entries
	// stamp, with many rows per invoice. The partial unique index
	// idx_credit_ledger_commit_fund_dedup allows one commit grant per (tenant,
	// funding invoice). The index is a backstop behind the finalize
	// compare-and-swap (CAS). A violation inside the finalize coordinator's
	// transaction means an invariant broke, so it aborts loudly. It is never
	// caught inside that transaction, because the error poisons the
	// transaction: PostgreSQL refuses every later statement in it.
	SourceInvoiceID string `json:"source_invoice_id,omitempty"`
}

// GrantKind is the cost-basis class of a credit grant (ADR-078).
type GrantKind string

const (
	// GrantKindCommit marks a purchased prepaid commit, funded by a real
	// invoice (source_invoice_id set). It drains in the paid class.
	GrantKindCommit GrantKind = "commit"
	// GrantKindPromotional marks credit explicitly made free (marketing
	// spend). It drains before every paid-class block.
	GrantKindPromotional GrantKind = "promotional"
)

// CreditReturn is what one hand-back of an invoice's drawn credit did
// (ADR-132): a void, or a credit note on the invoice. The whole amount is one
// grant row on the ledger. The part the invoice drew from credit blocks goes
// back to those blocks, with their expiry and kind. The rest stays on the row
// as new credit with no expiry.
type CreditReturn struct {
	// ReturnedCents is the whole amount handed back: the row's amount_cents.
	ReturnedCents int64
	// RestoredCents is the part that went back to the blocks the invoice
	// drew from.
	RestoredCents int64
	// ExpiredOnReturnCents is the part of RestoredCents that expired at once,
	// because its block was already past its expiry.
	ExpiredOnReturnCents int64
	// CommitRetiredOnReturnCents is the part of RestoredCents that was retired
	// again, because its commit grant's funding invoice is voided.
	CommitRetiredOnReturnCents int64
	// UnattributedCents is the part handed back as new credit with no expiry.
	// It is credit drawn before drains recorded their blocks (2026-07-05),
	// and, on a paid invoice, the part the customer paid in cash.
	UnattributedCents int64
}

// CreditBalance is a customer's settled credit totals in one currency.
type CreditBalance struct {
	CustomerID string `json:"customer_id"`
	// Currency is the stored currency of the ledger rows this balance sums
	// (ADR-129). A customer with credit in two currencies has two balances.
	// They are never added together or converted.
	Currency     string `json:"currency"`
	BalanceCents int64  `json:"balance_cents"`
	TotalGranted int64  `json:"total_granted"`
	TotalUsed    int64  `json:"total_used"`
	TotalExpired int64  `json:"total_expired"`
	// TotalRetired is commit credit taken back when the invoice that funded
	// it was voided or refunded by a credit note. TotalDeducted is credit an
	// operator deducted (a negative adjustment). With TotalUsed (applied to
	// invoices) and TotalExpired, they account for every cent granted:
	// TotalGranted = TotalUsed + TotalExpired + TotalRetired + TotalDeducted
	// + BalanceCents.
	TotalRetired  int64 `json:"total_retired"`
	TotalDeducted int64 `json:"total_deducted"`
}

// CreditLiveBalance is a customer's credit position right now (#1019), with
// one entry per currency (ADR-129). billing.Engine.LiveCreditBalance computes
// it on read, and nothing is stored.
type CreditLiveBalance struct {
	CustomerID string               `json:"customer_id"`
	Balances   []CreditLiveCurrency `json:"balances"`
	ComputedAt time.Time            `json:"computed_at"`
}

// CreditLiveCurrency is the live position in one currency. BalanceCents is
// the credit that can be spent: the settled ledger balance, without any grant
// past its expiry that the sweep has not written off yet. UnbilledUsageCents
// is the usage of the current period or periods that no invoice has billed
// yet and that will be invoiced in this currency. EstimatedRemainingCents is
// the difference, floored at zero. The parts are returned beside the result,
// so a caller can see how far past zero a customer is (BalanceCents -
// UnbilledUsageCents), and so the arithmetic on a page adds up.
type CreditLiveCurrency struct {
	Currency                string `json:"currency"`
	BalanceCents            int64  `json:"balance_cents"`
	UnbilledUsageCents      int64  `json:"unbilled_usage_cents"`
	EstimatedRemainingCents int64  `json:"estimated_remaining_cents"`
}
