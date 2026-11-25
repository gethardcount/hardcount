package credit

import (
	"context"
	"database/sql"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

type Store interface {
	AppendEntry(ctx context.Context, tenantID string, entry domain.CreditLedgerEntry) (domain.CreditLedgerEntry, error)
	// AppendEntryAudited / AdjustAtomicAudited run the caller-supplied audit
	// emission on the SAME transaction as the ledger write (ADR-090 in-tx
	// emission): ledger row and audit row commit or roll back together. The
	// service builds the emission closure (it owns audit-row content); the
	// store owns the transaction and exposes it to the closure. emit
	// receives the persisted entry so the audit row can reference the
	// store-assigned id. nil emit = unaudited write.
	AppendEntryAudited(ctx context.Context, tenantID string, entry domain.CreditLedgerEntry, emit func(tx *sql.Tx, out domain.CreditLedgerEntry) error) (domain.CreditLedgerEntry, error)
	AdjustAtomicAudited(ctx context.Context, tenantID, customerID, currency, description string, amountCents int64, emit func(tx *sql.Tx, out domain.CreditLedgerEntry) error) (domain.CreditLedgerEntry, error)
	// ListGrantSummaries returns the customer's positive blocks with
	// per-block remaining (amount - consumed) — the commit/promo burndown
	// rows. includeExhausted=false returns live blocks only.
	ListGrantSummaries(ctx context.Context, tenantID, customerID string, includeExhausted bool) ([]GrantSummary, error)
	// AppendEntryTx is the in-transaction variant used by callers that
	// need to compose a credit-ledger insert atomically with other
	// writes (e.g. the subscription handler's AddItem-with-proration
	// flow inserts the sub item + the credit entry in one tx). The
	// caller owns the tx; the store just executes inside it.
	AppendEntryTx(ctx context.Context, tx *sql.Tx, tenantID string, entry domain.CreditLedgerEntry) (domain.CreditLedgerEntry, error)
	// LockCustomerCreditTx takes the customer's credit lock on tx, the lock
	// every balance writer takes; re-entrant within tx.
	LockCustomerCreditTx(ctx context.Context, tx *sql.Tx, tenantID, customerID string) error
	// ApplyToInvoiceAtomic debits credits and reduces invoice.amount_due
	// atomically. `at` stamps both the new ledger usage entry and the
	// invoice's updated_at. Zero means clock.Now(ctx).
	ApplyToInvoiceAtomic(ctx context.Context, tenantID, customerID, invoiceID, invoiceDesc string, invoiceAmountCents int64, at time.Time) (int64, error)

	// AdjustAtomic appends a manual adjustment entry in `currency` with the
	// balance check performed inside the same locked tx. Closes the TOCTOU
	// race where two concurrent deductions each observe the current balance,
	// each pass the "balance + amount >= 0" check, and both commit —
	// overdrafting the ledger. A deduction larger than the balance in that
	// currency is refused (errs.Invalid on amount_cents).
	AdjustAtomic(ctx context.Context, tenantID, customerID, currency, description string, amountCents int64) (domain.CreditLedgerEntry, error)
	// GetBalance returns one balance per currency the customer's ledger
	// holds, ordered by currency; none for an empty ledger (ADR-129).
	GetBalance(ctx context.Context, tenantID, customerID string) ([]domain.CreditBalance, error)
	// GetSpendableBalance is the balance per currency less what is left of
	// grants already past their expiry at `at` that the expiry sweep has not
	// written off: the amount a draw at that instant could use.
	GetSpendableBalance(ctx context.Context, tenantID, customerID string, at time.Time) (map[string]int64, error)
	// GetSpendableBalanceTx is GetSpendableBalance on the caller's tx.
	GetSpendableBalanceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID string, at time.Time) (map[string]int64, error)
	// ListBalances returns one page of the (customer, currency) balances the
	// filter picks ("" for all) and how many there are.
	ListBalances(ctx context.Context, tenantID string, filter domain.CreditBalanceFilter, limit, offset int) ([]domain.CreditBalance, int, error)
	ListEntries(ctx context.Context, filter ListFilter) ([]domain.CreditLedgerEntry, error)
	// CountEntries returns the TOTAL rows matching the filter (limit/
	// offset/sort ignored) so paginated consumers can be honest about
	// truncation — the ledger page's CSV silently exported the capped
	// 50-row slice as if complete before this existed.
	CountEntries(ctx context.Context, filter ListFilter) (int, error)
	// InvoiceCreditsTx adds up the invoice's credit rows per currency, on
	// the caller's transaction, with no LIMIT: what the invoice drew from
	// the balance (its usage rows) and what issued credit notes already
	// handed back for it (grants on the invoice with a credit-note source,
	// each at its whole amount, including what it restored to other
	// blocks). The void reversal reads it under the void's invoice lock and
	// hands back the difference.
	InvoiceCreditsTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID string) ([]InvoiceCredits, error)
	// ReturnCreditTx hands back credit an invoice drew, on the caller's tx
	// (ADR-132): the part the invoice drew from credit blocks goes back to
	// those blocks, last drawn first, keeping their expiry and kind; a share
	// whose grant is past expiry expires at once, by the expiry sweep's
	// rule. The whole amount is one grant row for the entry's source.
	ReturnCreditTx(ctx context.Context, tx *sql.Tx, tenantID string, in ReturnEntry) (domain.CreditLedgerEntry, domain.CreditReturn, error)

	// ExpireGrantAtomic retires one expired grant — flips its
	// consumed_cents to amount_cents and appends the -remaining expiry
	// entry in a SINGLE transaction, recomputing `remaining` under the
	// same row lock the apply/adjust paths hold. Returns the expired
	// cents; 0 with nil error when the grant was already fully consumed
	// or retired by the time the lock was acquired (clean no-op — the
	// caller's candidate snapshot is expected to go stale). The
	// consumed_cents flip is the exactly-once gate: replayed and
	// concurrent sweeps converge on one expiry entry per grant.
	ExpireGrantAtomic(ctx context.Context, tenantID, customerID, grantID string) (int64, error)
	// ListExpiredGrants lists the grants past expires_at, on the database's
	// clock, that the sweep may retire: not one an owed period close or
	// period invoice still draws on (domain.CreditDrawOwedSQL, ADR-071 §2).
	ListExpiredGrants(ctx context.Context) ([]domain.CreditLedgerEntry, error)

	// LockCommitGrantTx + RetireForTopupNoteTx are what a credit
	// note on a top-up uses (ADR-080): read the funded grant's frozen state
	// under the customer's credit lock, live or not at `at`, then retire an
	// explicit slice (bumping the telescoping anchor cn_retired_cents) with a
	// loud error on a missing grant. ReadTopupGrantAndBalance is the same
	// read with no lock, for display.
	LockCommitGrantTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, at time.Time) (commitGrantState, bool, error)
	RetireForTopupNoteTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, creditNoteID, creditNoteNumber string, slice, refundedGrossCents, grossPaidCents int64, lostDispute bool) (commitGrantState, error)
	ReadTopupGrantAndBalance(ctx context.Context, tenantID, invoiceID string, at time.Time) (commitGrantState, int64, bool, error)
}

type ListFilter struct {
	TenantID   string
	CustomerID string
	EntryType  string
	InvoiceID  string
	Limit      int
	Offset     int
	Sort       string // closed allow-list (validated in store); empty defaults to created_at
	SortDir    string // "asc" or "desc"; empty defaults to desc
}

// InvoiceCredits is one currency's credit rows on one invoice.
type InvoiceCredits struct {
	Currency string
	// UsedCents is what the invoice's usage rows drew, as a positive amount.
	UsedCents int64
	// ReturnedCents is what credit notes on the invoice handed back.
	ReturnedCents int64
}
