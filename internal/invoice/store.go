package invoice

import (
	"context"
	"database/sql"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Store is the invoice persistence Service uses. It lists only the methods
// Service calls; other packages reach the PostgresStore methods they need
// through their own interfaces.
type Store interface {
	// CreateNumberedAudited / AddLineItemAtomicAudited run the
	// caller-supplied audit emission on the SAME transaction as the write
	// (ADR-090 in-tx emission): the invoice mutation and its audit row commit
	// or roll back together. The service builds the emission closure (it owns
	// audit-row content — the manual-create / line-item-added intent); the
	// store owns the transaction and exposes it to the closure. emit receives
	// the PERSISTED row(s) so the audit row can reference the store-assigned
	// id, invoice number, and recomputed totals. nil emit = unaudited write.
	// A bare-header create passes no items.
	//
	// CreateNumberedAudited also takes the invoice number on that
	// transaction: number runs before the insert and its result is the
	// invoice number. A create that fails gives the number back, so the
	// tenant's numbers keep no gap.
	CreateNumberedAudited(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem, number func(tx *sql.Tx) (string, error), emit func(tx *sql.Tx, out domain.Invoice) error) (domain.Invoice, error)
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	List(ctx context.Context, filter ListFilter) ([]domain.Invoice, int, error)
	// UpdateStatusWithReversalPrior flips the status AND runs fn on the SAME
	// tx — either both commit or neither, so a void never strands the
	// customer's applied credits unrestored (Void runs the consumed-credit
	// reversal in fn). fn gets the invoice as read under the row lock
	// (ADR-090: audit rows must not stamp status_before from a racy pre-tx
	// snapshot; #975: refusals re-run there). A closure error rolls the flip
	// back.
	UpdateStatusWithReversalPrior(ctx context.Context, tenantID, id string, status domain.InvoiceStatus, fn func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error)
	// UpdateStatusWithReversalPriorTx is the same flip on the caller's tx; the
	// closure receives the invoice as read under the row lock.
	UpdateStatusWithReversalPriorTx(ctx context.Context, tx *sql.Tx, tenantID, id string, status domain.InvoiceStatus, fn func(tx *sql.Tx, prior domain.Invoice) error) (domain.Invoice, error)
	// FinalizeWithDates flips status to finalized AND re-stamps issued_at +
	// due_at to the finalize moment. Used for operator-composed (manual)
	// invoices so the issue/due dates anchor to issuance, not draft-create
	// (Stripe finalized_at semantics). Cycle drafts keep their build-time
	// dates via FinalizeDraft.
	// taxedLineIDs are the lines Finalize read before computing tax; it
	// refuses when the invoice's lines differ (#1298).
	// onFinalized runs on the finalize tx after the flip, before commit (nil:
	// nothing); its error rolls the finalize back.
	FinalizeWithDates(ctx context.Context, tenantID, id string, issuedAt, dueAt time.Time, taxedLineIDs []string, onFinalized func(tx *sql.Tx, inv domain.Invoice) error) (domain.Invoice, error)
	// FinalizeDraft is the draft→finalized flip for a cycle invoice, with the
	// same onFinalized hook.
	FinalizeDraft(ctx context.Context, tenantID, id string, onFinalized func(tx *sql.Tx, inv domain.Invoice) error) (domain.Invoice, error)
	// MarkPaid flips status='paid', payment_status='succeeded',
	// amount_paid=amount_due, amount_due=0 in one transaction. Service uses
	// it to settle an invoice with nothing left due (SettleZeroDue, and
	// RetryTax after a finalize that leaves $0 due). Record Offline Payment
	// uses MarkPaidOffline.
	MarkPaid(ctx context.Context, tenantID, id string, stripePaymentIntentID string, paidAt time.Time) (domain.Invoice, error)
	// MarkPaidOffline is MarkPaid for Record Offline Payment: it sets
	// paid_out_of_band and writes no PaymentIntent, and check runs on the
	// row under the lock and can refuse. Never for a card settlement.
	MarkPaidOffline(ctx context.Context, tenantID, id string, paidAt time.Time, check func(tx *sql.Tx, locked domain.Invoice) error) (domain.Invoice, error)
	// SettleZeroDue marks paid, under the invoice's row lock, a finalized
	// invoice whose payment is pending or failed, with nothing due, tax ok
	// and no charge hold. Anything else is returned unsettled, not as an
	// error. paid reports whether this call settled it. Service uses it for
	// a settle that holds no charge claim (SettleZeroDueWithoutClaim).
	SettleZeroDue(ctx context.Context, tenantID, id string, paidAt time.Time) (domain.Invoice, bool, error)
	// ChargeHeldTx: the charge hold — none, live or stale (ADR-127) — and
	// its value. Caller holds the row lock on tx.
	ChargeHeldTx(ctx context.Context, tx *sql.Tx, id string) (domain.ChargeHoldState, domain.ChargeHold, error)
	// ChargeHoldStateOf reads the hold outside any lock, to word a refusal.
	ChargeHoldStateOf(ctx context.Context, tenantID, id string) (domain.ChargeHoldState, error)

	// ClaimChargeForManualCollect / ReleaseChargeHold: the per-invoice charge
	// hold (HA hazard #1, ADR-127) — exactly one owner enters the charge leg
	// per invoice. The operator-collect claim shares the auto-charge sweep's
	// hold — see postgres.go (2026-07-21 snapshot-race audit). A claim
	// returns its token (the zero ChargeHold = refused); the release compares
	// it.
	ClaimChargeForManualCollect(ctx context.Context, tenantID, id string) (domain.ChargeHold, error)
	ReleaseChargeHold(ctx context.Context, tenantID, id string, hold domain.ChargeHold) error

	ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error)

	// AddLineItemAtomicAudited inserts a line item and recomputes the invoice
	// totals in one transaction. It locks the invoice row FOR UPDATE, verifies
	// draft status and that the operator made the invoice (a subscription's
	// draft is refused with invoice_not_manual), inserts the line item
	// (filling InvoiceID, TenantID, and Currency from the locked invoice),
	// recomputes the subtotal from all line items, and rewrites
	// subtotal/total/amount_due. The lock prevents lost updates when two
	// clients append lines concurrently. Returns the inserted line item and
	// the updated invoice.
	//
	// emit is the in-tx audit emission (see CreateNumberedAudited). It
	// receives the persisted line item and the rewritten invoice — the invoice
	// number it labels the row with is therefore read INSIDE the tx, under the
	// same row lock as the mutation.
	AddLineItemAtomicAudited(ctx context.Context, tenantID, invoiceID string, item domain.InvoiceLineItem, emit func(tx *sql.Tx, item domain.InvoiceLineItem, inv domain.Invoice) error) (domain.InvoiceLineItem, domain.Invoice, error)

	// ListPendingTaxRetry returns draft invoices awaiting another
	// tax-calculation attempt: tax_status pending,
	// tax_error_code is in the retryable set (the worker filters
	// further on this side), tax_next_retry_at is NULL or in the
	// past, and tax_retry_count is below the per-invoice cap.
	// Ordered by tax_next_retry_at ASC (NULLS FIRST) so newly-
	// stuck invoices process first. Cross-tenant; returned rows
	// carry their tenant_id so the caller can dispatch per-row
	// with the right RLS partition.
	//
	// livemode filters to a single mode partition. The reconciler
	// runs once per mode in the scheduler tick; without this filter
	// the cross-mode RLS-bypassed scan would return rows for the
	// other mode that fail per-row RLS lookup with "not found".
	ListPendingTaxRetry(ctx context.Context, batch int, retryableCodes []string, maxAttempts int, livemode bool) ([]domain.Invoice, error)
	// ListPendingTaxCommit finds finalized/paid/voided/uncollectible
	// stripe_tax invoices with a successful calculation (tax_status=ok,
	// tax_calculation_id set) but no persisted tax_transaction_id — the orphan
	// state from a commit that succeeded at Stripe but failed to persist
	// locally (incl. the case where finalize+auto-charge flipped the invoice
	// to 'paid' before the reconciler's first scan), or from a commit that
	// never reached Stripe. Listed until the Stripe Tax calculation expires
	// (90 days after it is made), not while a failed commit waits for its
	// next try (DeferTaxWork). Powers the commit reconciler.
	ListPendingTaxCommit(ctx context.Context, batch int, livemode bool) ([]domain.Invoice, error)
	// ListPendingTaxReversal finds voided/uncollectible stripe_tax invoices that
	// still carry a tax_transaction_id but have tax_reversed_at NULL — a tax
	// reversal that failed (or never completed) at Stripe, leaving the tenant
	// over-remitting. Powers the reversal recovery sweep (symmetric sibling of
	// the #267 commit reconciler). Not time-bounded (ADR-111): a reversal
	// stays possible for good. An invoice whose failed reversal waits for its
	// next try (DeferTaxWork) is not listed.
	ListPendingTaxReversal(ctx context.Context, batch int, livemode bool) ([]domain.Invoice, error)
	// MarkTaxReversed stamps tax_reversed_at=now() once the upstream reversal is
	// confirmed (or there was nothing to reverse), dropping the row from the
	// reversal sweep.
	MarkTaxReversed(ctx context.Context, tenantID, id string) error
	// DeferTaxWork sets tax_next_retry_at after a tax commit or reversal
	// failed in a sweep; the pending lists skip the invoice until then.
	DeferTaxWork(ctx context.Context, tenantID, id string, next time.Time) error

	// ListPendingTaxRetryForClock is the catchup-path counterpart to
	// ListPendingTaxRetry — returns clock-pinned draft invoices stuck
	// at tax_status=pending with a retryable code. ADR-029 Phase 2:
	// clock-pinned tax retries fire only on operator Advance, never on
	// the wall-clock tick. One retry per row per Advance (no backoff
	// gate) — operator-friendly: each click does something visible.
	ListPendingTaxRetryForClock(ctx context.Context, tenantID, clockID string, retryableCodes []string, maxAttempts, limit int) ([]domain.Invoice, error)

	// ListCustomerDataInvalidErrors returns draft invoices for ONE
	// customer stuck on `customer_data_invalid` — the only tax error a
	// billing-profile update can resolve. Backs the per-customer
	// retry-flush fired from customer.Service.UpsertBillingProfile,
	// mirroring the ADR-019 Stripe-reconnect flush pattern at
	// per-customer scope.
	ListCustomerDataInvalidErrors(ctx context.Context, tenantID, customerID string) ([]domain.Invoice, error)

	// ListProviderConfigErrors returns draft invoices stuck on Stripe-
	// configuration errors (tax_error_code IN provider_not_configured,
	// provider_auth) for the given (tenant, livemode). Used by the
	// tenantstripe.Connect path to fan out a one-shot retry the moment
	// fresh credentials land — operator-driven recovery, not
	// background polling. Tenant-scoped RLS via TxTenant; the per-mode
	// filter is in the WHERE clause so a test-mode connect doesn't
	// surface live-mode rows. ADR-019.
	ListProviderConfigErrors(ctx context.Context, tenantID string, livemode bool) ([]domain.Invoice, error)

	// SetPublicToken writes (or overwrites) the hosted-invoice-URL token for
	// an invoice. The rotate-public-token endpoint calls it; the finalize
	// writes mint the first token themselves. Invoice must exist and be
	// non-draft; drafts never carry a token. Returns errs.ErrNotFound if the
	// invoice is missing or still draft.
	SetPublicToken(ctx context.Context, tenantID, invoiceID, token string) error

	// GetByPublicToken resolves the hosted-invoice-URL token to its invoice
	// AND its livemode (the second return value). Runs under TxBypass because
	// the caller is unauthenticated: the public route receives a raw token
	// from the URL and must resolve it to a tenant + mode BEFORE any tenant
	// context can be set. The livemode return is what callers need to pin
	// `postgres.WithLivemode(ctx, …)` on the request context for every
	// downstream RLS-scoped read; without it, the public route defaults to
	// live and a test-mode invoice's line items / customer / settings reads
	// silently 404. The token itself is the credential (256 bits of entropy,
	// UNIQUE indexed) so cross-tenant probing isn't feasible. Returns
	// errs.ErrNotFound on miss.
	GetByPublicToken(ctx context.Context, token string) (domain.Invoice, bool, error)

	// ListChargeAttemptsByInvoice reads the ADR-102 charge-attempt facts:
	// one row per attempt, upserted by PaymentIntent id as settle paths
	// resolve the outcome (PostgresStore.RecordChargeAttempt). The timeline
	// renders each attempt exactly once via the precedence chain dunning row
	// → attempt row → stripe webhook row.
	ListChargeAttemptsByInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceChargeAttempt, error)
}

// OutstandingBalance is the customer-AR snapshot — total cents owed
// across all unpaid (pending / failed / unknown) finalized invoices,
// excluding voided + uncollectible. UnpaidCount is the cardinality
// of those invoices so the UI can show "$X across N unpaid invoices."
//
// TotalCents adds every currency together, so it is a meaningful amount only
// when ByCurrency has a single entry; it is kept for existing API callers.
// ByCurrency is what a display uses: one amount per currency, largest first.
type OutstandingBalance struct {
	TotalCents  int64            `json:"total_cents"`
	UnpaidCount int              `json:"unpaid_count"`
	ByCurrency  []CurrencyAmount `json:"by_currency"`
}

// CurrencyAmount is an amount in one currency's minor units.
type CurrencyAmount struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
}

type ListFilter struct {
	TenantID       string
	CustomerID     string
	SubscriptionID string
	Status         string
	PaymentStatus  string
	// Search matches the invoice number (case-insensitive substring, ILIKE,
	// metacharacters escaped) or an invoice of a customer in
	// SearchCustomerIDs. Empty = no filter. Backs the dashboard list search
	// box and the command palette.
	Search string
	// SearchCustomerIDs are the customers whose name or email contains
	// Search. Service.List fills it from the customer store, which matches
	// after decrypting (both columns are encrypted at rest, so SQL cannot
	// match them). Empty with a Search: the number alone matches.
	SearchCustomerIDs []string
	// CreatedFrom / CreatedTo bound created_at — inclusive on both
	// ends, matching the shared api/timefilter contract (?from / ?to).
	// Zero values = unbounded.
	CreatedFrom time.Time
	CreatedTo   time.Time
	// Overdue restricts to open invoices past their due date that
	// haven't settled and aren't mid-payment: status='finalized',
	// due_at < now(), payment_status not in (succeeded, processing).
	// Backs the dashboard's "Past due" tab (Stripe-parity segment).
	// NOTE: deliberately computed from due_at at query time — the
	// false (schema 0001 default); do not filter on it.
	Overdue bool
	// IDs scopes the result to a specific set of invoice IDs. Used by
	// other list pages (CreditNotes) to fetch exactly the invoices
	// referenced by their primary rows — avoids the
	// "list-then-client-side-join" pagination bug. Empty = no filter.
	IDs    []string
	Limit  int
	Offset int
	// Sort: column name from a closed set (validated by the store).
	// Empty string means default (created_at).
	Sort string
	// SortDir: "asc" or "desc". Empty string means desc.
	SortDir string
}
