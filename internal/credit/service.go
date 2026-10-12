package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// AuditEmitter is the narrow in-tx audit seam (ADR-090). The service builds
// the emission content at the state-transition site; the store threads the
// closure onto its transaction so ledger write and audit row share fate.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

type Service struct {
	store       Store
	resolver    clock.Resolver
	auditLogger AuditEmitter
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// SetAuditLogger wires in-tx audit emission for operator-facing credit
// mutations (grant / adjust). Moved here from the handler (ADR-090): the
// handler's post-hoc Log ran on its own tx AFTER the ledger commit, so a
// crash or disconnect between the two left a committed money mutation with
// no audit row — and a failed ledger write could never phantom-log, but a
// logged-then-failed pattern elsewhere could. In-tx emission makes both
// classes unrepresentable.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.auditLogger = a }

// SetResolver wires the unified clock.Resolver. Customer-scoped credit
// mutations (Grant, ApplyToInvoice, etc.) bind effective-now from the
// customer pin so the ledger entry's created_at lands in simulated
// time on clock-pinned customers — same architectural rule as
// subscription / invoice / dunning.
func (s *Service) SetResolver(r clock.Resolver) {
	s.resolver = r
}

func (s *Service) bindForCustomer(ctx context.Context, tenantID, customerID string) context.Context {
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, CustomerID: customerID})
	return bound
}

type GrantInput struct {
	CustomerID  string     `json:"customer_id"`
	AmountCents int64      `json:"amount_cents"`
	Description string     `json:"description"`
	InvoiceID   string     `json:"invoice_id,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`

	// SourceSubscriptionID + SourceSubscriptionItemID + SourcePlanChangedAt +
	// SourceChangeType, when all set, mark this grant as a proration credit
	// for a specific item mutation (plan downgrade, qty reduction, remove).
	// The store enforces uniqueness on the full tuple so retries return
	// ErrAlreadyExists instead of duplicating the credit — see migration 0027
	// for the unique partial index.
	SourceSubscriptionID     string                `json:"source_subscription_id,omitempty"`
	SourceSubscriptionItemID string                `json:"source_subscription_item_id,omitempty"`
	SourcePlanChangedAt      *time.Time            `json:"source_plan_changed_at,omitempty"`
	SourceChangeType         domain.ItemChangeType `json:"source_change_type,omitempty"`

	// SourceCreditNoteID dedups grants created by credit-note Issue()
	// — a retry after partial-failure hits the unique partial index
	// (migration 0093) and the store returns ErrAlreadyExists. Service
	// callers use GrantOrFetch (below) to handle the retry path.
	SourceCreditNoteID string `json:"source_credit_note_id,omitempty"`

	// At is the simulated instant the grant was earned (cancel time
	// for cancel-proration credits, plan-change time for plan-change
	// credits, operator action time for manual grants). Empty falls
	// back to clock.Now(ctx) at the postgres layer — fine for
	// operator-action paths in wall-clock. Set by engine callers
	// during catchup to keep ledger entries on simulated-time so
	// the customer's Credits tab shows per-fact chronology instead
	// of every entry stacked at advance-end frozen_time.
	At time.Time `json:"-"`

	// GrantKind classifies the grant's cost basis (ADR-078). Operator/API
	// callers may set 'promotional' (free marketing credits — drained
	// before all paid-class blocks); 'commit' is RESERVED for the invoice
	// finalize funding path (GrantCommitForInvoiceTx) and rejected here.
	// Empty = unclassified, drains in the paid class.
	GrantKind domain.GrantKind `json:"grant_kind,omitempty"`
}

func (s *Service) Grant(ctx context.Context, tenantID string, input GrantInput) (domain.CreditLedgerEntry, error) {
	if input.CustomerID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("customer_id")
	}
	ctx = s.bindForCustomer(ctx, tenantID, input.CustomerID)
	if input.AmountCents <= 0 {
		return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents", "must be greater than 0")
	}
	if input.AmountCents > 100_000_000 { // $1M cap
		return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents", "cannot exceed 1,000,000")
	}
	desc := strings.TrimSpace(input.Description)
	if desc == "" {
		return domain.CreditLedgerEntry{}, errs.Required("description")
	}
	if len(desc) > 500 {
		return domain.CreditLedgerEntry{}, errs.Invalid("description", "must be at most 500 characters")
	}
	// Reject past expires_at — a grant that's already expired at
	// creation time is dead-on-arrival: it briefly inflates the
	// balance, then the next expiry catchup retires it (with my
	// fullCycleDays/consumed-aware fixes, net effect is $0). But the
	// operator clearly didn't intend this — they likely picked the
	// wrong year/date. Industry parity: Stripe credit-grant API
	// returns 422 on past expires_at.
	//
	// Compare against clock.Now(ctx) so clock-pinned customers
	// (bindForCustomer above seeded ctx with the sim clock) evaluate
	// against simulated time, not wall-clock. Internal engine callers
	// (cancel / plan-swap relief, CN Issue) don't set
	// ExpiresAt on their grants, so this gate doesn't affect refund
	// flows — only operator-driven and SDK-driven Grant calls.
	if input.ExpiresAt != nil && !input.ExpiresAt.After(clock.Now(ctx)) {
		return domain.CreditLedgerEntry{}, errs.Invalid("expires_at",
			"must be in the future — a grant that expires at or before now is dead on arrival")
	}
	// Operators may mark a grant promotional; 'commit' is reserved for the
	// invoice-finalize funding path (ADR-078) — a hand-minted commit would
	// claim paid-class drain order and commit reporting without a funding
	// invoice behind it.
	switch input.GrantKind {
	case "", domain.GrantKindPromotional:
	default:
		return domain.CreditLedgerEntry{}, errs.Invalid("grant_kind",
			"must be empty or 'promotional' — commit grants are created by finalizing a commit invoice")
	}

	return s.store.AppendEntryAudited(ctx, tenantID, domain.CreditLedgerEntry{
		CustomerID:               input.CustomerID,
		EntryType:                domain.CreditGrant,
		AmountCents:              input.AmountCents,
		Description:              desc,
		InvoiceID:                input.InvoiceID,
		ExpiresAt:                input.ExpiresAt,
		SourceSubscriptionID:     input.SourceSubscriptionID,
		SourceSubscriptionItemID: input.SourceSubscriptionItemID,
		SourcePlanChangedAt:      input.SourcePlanChangedAt,
		SourceChangeType:         input.SourceChangeType,
		SourceCreditNoteID:       input.SourceCreditNoteID,
		GrantKind:                input.GrantKind,
		CreatedAt:                input.At,
	}, s.grantEmission(ctx))
}

// grantEmission builds the in-tx audit emission for an operator/API grant.
// Wire strings (action "grant", resource "credit") and metadata keys are
// FROZEN — the dashboard's filter vocabulary and badges key on them.
func (s *Service) grantEmission(ctx context.Context) func(tx *sql.Tx, out domain.CreditLedgerEntry) error {
	if s.auditLogger == nil {
		return nil
	}
	return func(tx *sql.Tx, out domain.CreditLedgerEntry) error {
		return s.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionGrant,
			ResourceType:  "credit",
			ResourceID:    out.ID,
			ResourceLabel: out.Description,
			Metadata: map[string]any{
				"customer_id":  out.CustomerID,
				"amount_cents": out.AmountCents,
				"description":  out.Description,
			},
		})
	}
}

// GrantCommitForInvoiceTx appends the commit grant on the CALLER's tx —
// invoice.FinalizeWithDates runs it inside the finalize coordinator tx so the
// status flip and the grant commit or roll back together (ADR-078 D2). An
// error here fails Finalize by design: it is operator-synchronous, loud, and
// retryable (the finalize CAS makes the retry clean). The fund-once partial
// unique index (migration 0136) is a structural backstop whose violation
// aborts the tx loudly — never caught here (poisoned-tx; see
// GrantForCreditNoteTx's contract). Flat args so the invoice store's
// consumer-side CommitFunder interface is satisfied directly (zero
// cross-domain imports).
func (s *Service) GrantCommitForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber string, amountCents int64, expiresAt *time.Time) (domain.CreditLedgerEntry, error) {
	if customerID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("customer_id")
	}
	if invoiceID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("invoice_id")
	}
	if amountCents <= 0 {
		return domain.CreditLedgerEntry{}, errs.Invalid("commit_granted_cents", "must be greater than 0")
	}
	if amountCents > 100_000_000 { // $1M cap, matches Grant; raise on first DP ask (ADR-078)
		return domain.CreditLedgerEntry{}, errs.Invalid("commit_granted_cents", "cannot exceed 1,000,000")
	}
	// A commit whose expiry already passed would grant dead-on-arrival
	// credits the next sweep silently retires — the customer pays and
	// receives nothing. buildLineItem validates this at compose time; this
	// re-check catches a line composed long before finalize. Loud,
	// operator-synchronous: fix the line's expiry and re-finalize.
	if expiresAt != nil && !expiresAt.After(clock.Now(ctx)) {
		return domain.CreditLedgerEntry{}, errs.Invalid("commit_expires_at",
			"already in the past — the granted credits would expire on arrival; update the commit line's expiry, then finalize")
	}
	desc := "Prepaid commit"
	if invoiceNumber != "" {
		desc = fmt.Sprintf("Prepaid commit — invoice %s", invoiceNumber)
	}
	return s.store.AppendEntryTx(ctx, tx, tenantID, domain.CreditLedgerEntry{
		CustomerID:      customerID,
		EntryType:       domain.CreditGrant,
		AmountCents:     amountCents,
		Description:     desc,
		InvoiceID:       invoiceID,
		ExpiresAt:       expiresAt,
		GrantKind:       domain.GrantKindCommit,
		SourceInvoiceID: invoiceID,
		CreatedAt:       clock.Now(ctx),
	})
}

// CommitReliefResult is the post-retire grant state the relief CN needs
// for its event payload and response (ADR-080).
type CommitReliefResult struct {
	GrantID             string
	RemainingAfterCents int64
	CNRetiredCents      int64
}

// RetireCommitSliceForReliefTx retires `slice` credits from the commit
// grant funded by invoiceID on the caller's CN coordinator tx (ADR-080
// relief leg). Loud on a missing grant; bumps the telescoping anchor K.
func (s *Service) RetireCommitSliceForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, creditNoteID string, slice, refundedGrossCents, grossPaidCents int64) (CommitReliefResult, error) {
	g, err := s.store.RetireCommitSliceForReliefTx(ctx, tx, tenantID, invoiceID, creditNoteID, slice, refundedGrossCents, grossPaidCents)
	if err != nil {
		return CommitReliefResult{}, err
	}
	return CommitReliefResult{GrantID: g.GrantID, RemainingAfterCents: g.Remaining(), CNRetiredCents: g.CNRetiredCents}, nil
}

// LockCommitGrantForReliefTx reads the funded grant's live state under the
// relief locks (customer advisory + grant FOR UPDATE) on the caller's tx —
// the read half of the relief: the cap formula's inputs (G, C, K) come from
// here and stay frozen until the tx commits. found=false when the invoice
// funded no grant.
func (s *Service) LockCommitGrantForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (granted, consumed, cnRetired int64, found bool, err error) {
	g, found, err := s.store.LockCommitGrantTx(ctx, tx, tenantID, invoiceID)
	if err != nil || !found {
		return 0, 0, 0, found, err
	}
	return g.AmountCents, g.ConsumedCents, g.CNRetiredCents, true, nil
}

// RetireCommitGrantForInvoiceTx retires the remaining balance of the commit
// grant funded by invoiceID, on the caller's tx (invoice void — ADR-078 D3).
// Clean no-op when no grant exists or nothing remains; consumed stays
// consumed.
func (s *Service) RetireCommitGrantForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (int64, error) {
	return s.store.RetireCommitGrantForInvoiceTx(ctx, tx, tenantID, invoiceID)
}

// GrantTx is the in-transaction variant used by the subscription
// handler's atomic AddItem-with-proration flow. The caller owns the
// tx; this method runs the same validation as Grant but writes the
// ledger entry via the store's AppendEntryTx so it shares the caller's
// tx. ADR-030 atomic-proration follow-through (2026-05-29).
//
// Note: skips the bindForCustomer call (the caller is expected to have
// already bound effective-now from the affected entity's pin via the
// handler's resolver — passing the bound ctx through to us).
func (s *Service) GrantTx(ctx context.Context, tx *sql.Tx, tenantID string, input GrantInput) (domain.CreditLedgerEntry, error) {
	if input.CustomerID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("customer_id")
	}
	if input.AmountCents <= 0 {
		return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents", "must be greater than 0")
	}
	if input.AmountCents > 100_000_000 {
		return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents", "cannot exceed 1,000,000")
	}
	desc := strings.TrimSpace(input.Description)
	if desc == "" {
		return domain.CreditLedgerEntry{}, errs.Required("description")
	}
	if len(desc) > 500 {
		return domain.CreditLedgerEntry{}, errs.Invalid("description", "must be at most 500 characters")
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(clock.Now(ctx)) {
		return domain.CreditLedgerEntry{}, errs.Invalid("expires_at",
			"must be in the future — a grant that expires at or before now is dead on arrival")
	}
	// Same kind gate as Grant — 'commit' stays reserved for the
	// invoice-finalize funding path (ADR-078).
	switch input.GrantKind {
	case "", domain.GrantKindPromotional:
	default:
		return domain.CreditLedgerEntry{}, errs.Invalid("grant_kind",
			"must be empty or 'promotional' — commit grants are created by finalizing a commit invoice")
	}
	out, err := s.store.AppendEntryTx(ctx, tx, tenantID, domain.CreditLedgerEntry{
		CustomerID:  input.CustomerID,
		EntryType:   domain.CreditGrant,
		AmountCents: input.AmountCents,
		Description: desc,
		InvoiceID:   input.InvoiceID,
		ExpiresAt:   input.ExpiresAt,
		// GrantKind was silently dropped here pre-2026-07-05 (the struct
		// literal omitted the field while Grant carried it), so every
		// in-tx grant minted a NULL-kind block — misclassified into the
		// paid drain class and invisible to kind-scoped reporting.
		GrantKind:                input.GrantKind,
		SourceSubscriptionID:     input.SourceSubscriptionID,
		SourceSubscriptionItemID: input.SourceSubscriptionItemID,
		SourcePlanChangedAt:      input.SourcePlanChangedAt,
		SourceChangeType:         input.SourceChangeType,
		SourceCreditNoteID:       input.SourceCreditNoteID,
		CreatedAt:                input.At,
	})
	if err != nil {
		return out, err
	}
	// ADR-090: the caller-tx twin emits on the CALLER's tx — this closes
	// the Grant/GrantTx audit divergence declared in ADR-090 §3 (CN-issue
	// grants, atomic proration grants). Same frozen content as
	// grantEmission; the error aborts the caller's whole coordinator tx.
	if s.auditLogger != nil {
		if err := s.auditLogger.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionGrant,
			ResourceType:  "credit",
			ResourceID:    out.ID,
			ResourceLabel: out.Description,
			Metadata: map[string]any{
				"customer_id":  out.CustomerID,
				"amount_cents": out.AmountCents,
				"description":  out.Description,
			},
		}); err != nil {
			return domain.CreditLedgerEntry{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

// GrantForCreditNote is the retry-safe Grant variant used by
// credit-note Issue(). Sets SourceCreditNoteID so the partial unique
// index (migration 0093) dedups retries. On ErrAlreadyExists the
// existing grant is fetched and returned — caller continues without
// double-crediting.
func (s *Service) GrantForCreditNote(ctx context.Context, tenantID, creditNoteID string, input GrantInput) (domain.CreditLedgerEntry, error) {
	if creditNoteID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("credit_note_id")
	}
	input.SourceCreditNoteID = creditNoteID
	entry, err := s.Grant(ctx, tenantID, input)
	if errors.Is(err, errs.ErrAlreadyExists) {
		existing, fetchErr := s.store.GetByCreditNoteSource(ctx, tenantID, creditNoteID)
		if fetchErr != nil {
			return domain.CreditLedgerEntry{}, fmt.Errorf("dedup hit but fetch failed: %w", fetchErr)
		}
		return existing, nil
	}
	return entry, err
}

// GrantForCreditNoteTx is GrantForCreditNote on the caller's coordinator tx: the
// ledger entry is appended via GrantTx so it commits atomically with the credit
// note's draft→issued CAS (ADR-061, creditnote.Issue). The 0093 (tenant, CN)
// dedup index still backs uniqueness, but the issue CAS makes a duplicate
// UNREACHABLE — exactly one Issue() reaches the grant per credit note. Unlike
// the non-tx GrantForCreditNote (whose own tx can be discarded on a dedup hit
// and the existing grant re-fetched), a conflict here would ABORT the shared
// coordinator tx, so we do NOT swallow ErrAlreadyExists: surfacing it correctly
// fails the whole Issue() rather than committing a poisoned tx. Given the CAS
// invariant it is dead code; if it ever fires, the invariant is broken.
func (s *Service) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, creditNoteID string, input GrantInput) (domain.CreditLedgerEntry, error) {
	if creditNoteID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("credit_note_id")
	}
	input.SourceCreditNoteID = creditNoteID
	return s.GrantTx(ctx, tx, tenantID, input)
}

// GetByProrationSource exposes the store-level source lookup. Used by the
// subscription proration path to complete an idempotent retry after
// AppendEntry returns ErrAlreadyExists.
func (s *Service) GetByProrationSource(ctx context.Context, tenantID, subscriptionID, subscriptionItemID string, changeType domain.ItemChangeType, changeAt time.Time) (domain.CreditLedgerEntry, error) {
	return s.store.GetByProrationSource(ctx, tenantID, subscriptionID, subscriptionItemID, changeType, changeAt)
}

// ApplyToInvoice deducts credits from a customer's balance AND reduces the
// invoice's amount_due_cents in a single atomic transaction. Either both
// happen or neither does — there is no window where the ledger is debited
// but the invoice still shows the pre-credit amount due (which would cause
// double-billing when the PaymentIntent charges for the full original total).
//
// Returns the amount deducted. If balance is 0 or invoice amount is 0,
// returns 0 without any writes.
func (s *Service) ApplyToInvoice(ctx context.Context, tenantID, customerID, invoiceID string, invoiceAmountCents int64, invoiceNumber ...string) (int64, error) {
	return s.ApplyToInvoiceAt(ctx, tenantID, customerID, invoiceID, invoiceAmountCents, time.Time{}, invoiceNumber...)
}

// ApplyToInvoiceAt is the simulated-time-aware variant: `at` stamps the
// ledger usage entry and the invoice's updated_at, anchoring on the
// invoice's cycle-close instant during catchup so credits applied across
// multiple periods don't all stack at advance-end frozen_time on the
// customer's Credits tab. Pass zero from operator paths to fall back to
// clock.Now(ctx) at the postgres layer.
func (s *Service) ApplyToInvoiceAt(ctx context.Context, tenantID, customerID, invoiceID string, invoiceAmountCents int64, at time.Time, invoiceNumber ...string) (int64, error) {
	desc := fmt.Sprintf("Applied to invoice %s", invoiceID)
	if len(invoiceNumber) > 0 && invoiceNumber[0] != "" {
		desc = fmt.Sprintf("Applied to invoice %s", invoiceNumber[0])
	}
	return s.store.ApplyToInvoiceAtomic(ctx, tenantID, customerID, invoiceID, desc, invoiceAmountCents, at)
}

func (s *Service) ReverseForInvoice(ctx context.Context, tenantID, customerID, invoiceID, invoiceNumber string) (int64, error) {
	return s.reverseForInvoice(ctx, tenantID, customerID, invoiceID, invoiceNumber,
		func(entry domain.CreditLedgerEntry) error {
			_, err := s.store.AppendEntry(ctx, tenantID, entry)
			return err
		})
}

// ReverseForInvoiceTx is the in-transaction variant: the reversal grant is
// appended on the caller's *sql.Tx so it commits (or rolls back) atomically
// with the invoice status flip that drives the void. invoice.Service.Void
// threads its coordinator tx through here — a reversal failure rolls the void
// back, so the invoice never lands voided with the customer's applied credits
// still consumed. The reading of usage entries runs on its own conn (those
// rows were committed when the credit was applied); only the grant INSERT
// rides the tx, and the 0106 dedup index keeps it exactly-once.
func (s *Service) ReverseForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber string) (int64, error) {
	return s.reverseForInvoice(ctx, tenantID, customerID, invoiceID, invoiceNumber,
		func(entry domain.CreditLedgerEntry) error {
			_, err := s.store.AppendEntryTx(ctx, tx, tenantID, entry)
			return err
		})
}

// reverseForInvoice is the shared body: sum the usage entries applied to the
// invoice and append a matching grant via the supplied appender (own tx vs
// caller's tx). bindForCustomer stamps the grant in simulated time when the
// customer is clock-pinned.
func (s *Service) reverseForInvoice(ctx context.Context, tenantID, customerID, invoiceID, invoiceNumber string, appendFn func(domain.CreditLedgerEntry) error) (int64, error) {
	// Bind ctx clock to the customer pin so the reversal grant entry's
	// created_at stamps in simulated time when called from the void /
	// dunning-writeoff flow on a clock-pinned customer.
	ctx = s.bindForCustomer(ctx, tenantID, customerID)
	entries, err := s.store.ListEntries(ctx, ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		InvoiceID:  invoiceID,
		EntryType:  string(domain.CreditUsage),
		Limit:      100,
	})
	if err != nil {
		return 0, err
	}

	// Sum all usage entries for this invoice (they're negative)
	var totalUsed int64
	for _, e := range entries {
		totalUsed += -e.AmountCents // Convert negative to positive
	}

	if totalUsed <= 0 {
		return 0, nil // No credits were applied to this invoice
	}

	// A credit note issued against this invoice while it was unpaid may already
	// have returned some of those credits to the balance (creditnote.Issue,
	// ADR-061 amendment 2026-10-12). Hand back only what is still applied.
	// Every credit-note grant on a voidable invoice is such a return: the other
	// kind is written for a PAID invoice, which cannot be voided.
	returned, err := s.store.SumCreditNoteGrantsForInvoice(ctx, tenantID, customerID, invoiceID)
	if err != nil {
		return 0, fmt.Errorf("read credits already returned by credit notes: %w", err)
	}
	totalUsed -= returned
	if totalUsed <= 0 {
		return 0, nil // Credit notes already returned everything that was applied
	}

	desc := fmt.Sprintf("Reversed — invoice %s voided", invoiceNumber)
	if invoiceNumber == "" {
		desc = fmt.Sprintf("Reversed — invoice %s voided", invoiceID)
	}

	err = appendFn(domain.CreditLedgerEntry{
		CustomerID:              customerID,
		EntryType:               domain.CreditGrant,
		AmountCents:             totalUsed,
		Description:             desc,
		InvoiceID:               invoiceID,
		SourceInvoiceReversalID: invoiceID,
	})
	if err != nil {
		// A second void / dunning manual-resolve of the same invoice hits the
		// partial unique index (migration 0106) and the store returns
		// ErrAlreadyExists with this code — the reversal already happened, so
		// this is an idempotent no-op rather than a double-credit.
		if errs.Code(err) == "credit_reversal_source_taken" {
			return 0, nil
		}
		return 0, err
	}

	return totalUsed, nil
}

// ExpireCredits finds unexpired grant entries past their expiry date and creates
// negative (expiry) entries to zero them out. Returns the count of expired grants
// and any errors encountered during processing.
func (s *Service) ExpireCredits(ctx context.Context) (int, []error) {
	grants, err := s.store.ListExpiredGrants(ctx)
	if err != nil {
		return 0, []error{fmt.Errorf("list expired grants: %w", err)}
	}
	return s.processExpiry(ctx, grants)
}

// ExpireCreditsForClock is the catchup-path counterpart. ADR-029
// Phase 4: clock-pinned customer grants expire only on operator
// Advance, against the clock's frozen_time, never on the wall-clock
// cron tick.
func (s *Service) ExpireCreditsForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time) (int, []error) {
	grants, err := s.store.ListExpiredGrantsForClock(ctx, tenantID, clockID, frozenTime)
	if err != nil {
		return 0, []error{fmt.Errorf("list expired grants for clock %s: %w", clockID, err)}
	}
	return s.processExpiry(ctx, grants)
}

// processExpiry is the shared per-grant body of ExpireCredits and
// ExpireCreditsForClock — the candidate list shape differs by trigger
// path; the per-grant retirement is identical.
//
// The candidate list is DISCOVERY only. All money decisions happen
// inside store.ExpireGrantAtomic: it recomputes the remaining
// (un-drained) portion under the same row lock the apply/adjust paths
// hold — the list's snapshot here is stale by construction (a
// backdated ApplyToInvoiceAt can drain a candidate between the list
// read and the retirement) — flips consumed_cents to amount_cents,
// and appends the -remaining expiry entry, all in one tx. A grant
// that turns out fully consumed under the lock is a silent no-op
// (retired == 0), so replayed or overlapping sweeps converge on
// exactly one expiry entry per grant. Expiry entries are stamped at
// the grant's own expires_at inside the store (per-fact chronology
// on the Credits tab), and retirement wins over any later backdated
// apply — a retired grant is never re-drained (ADR-071).
func (s *Service) processExpiry(ctx context.Context, grants []domain.CreditLedgerEntry) (int, []error) {
	var expired int
	var expiryErrs []error
	for _, g := range grants {
		retired, err := s.store.ExpireGrantAtomic(ctx, g.TenantID, g.CustomerID, g.ID)
		if err != nil {
			expiryErrs = append(expiryErrs, fmt.Errorf("expire grant %s: %w", g.ID, err))
			continue
		}
		if retired > 0 {
			expired++
		}
	}
	return expired, expiryErrs
}

// GrantsResponse is the burndown payload: per-grant rows plus the
// kind-subtotal rollup (the headline Credit Balance mixes free promo with
// money-backed commit liability — finance needs them split; the 2026-07-05
// reassessment made the split an explicit acceptance criterion).
type GrantsResponse struct {
	Grants []GrantSummary `json:"grants"`
	// Remaining, split by cost-basis class (ADR-078): commit = money-backed
	// (funded by a paid commit invoice), promotional = free marketing
	// credits, other = legacy/unclassified grants + positive adjustments +
	// reversals (paid drain class).
	CommitRemainingCents      int64 `json:"commit_remaining_cents"`
	PromotionalRemainingCents int64 `json:"promotional_remaining_cents"`
	OtherRemainingCents       int64 `json:"other_remaining_cents"`
}

// ListGrants returns the customer's credit blocks with drawdown state +
// kind subtotals. includeExhausted adds fully-drawn blocks (history view).
func (s *Service) ListGrants(ctx context.Context, tenantID, customerID string, includeExhausted bool) (GrantsResponse, error) {
	if customerID == "" {
		return GrantsResponse{}, errs.Required("customer_id")
	}
	grants, err := s.store.ListGrantSummaries(ctx, tenantID, customerID, includeExhausted)
	if err != nil {
		return GrantsResponse{}, err
	}
	resp := GrantsResponse{Grants: grants}
	if resp.Grants == nil {
		resp.Grants = []GrantSummary{}
	}
	for _, g := range grants {
		if g.RemainingCents <= 0 {
			continue
		}
		switch g.GrantKind {
		case string(domain.GrantKindCommit):
			resp.CommitRemainingCents += g.RemainingCents
		case string(domain.GrantKindPromotional):
			resp.PromotionalRemainingCents += g.RemainingCents
		default:
			resp.OtherRemainingCents += g.RemainingCents
		}
	}
	return resp, nil
}

func (s *Service) GetBalance(ctx context.Context, tenantID, customerID string) (domain.CreditBalance, error) {
	return s.store.GetBalance(ctx, tenantID, customerID)
}

func (s *Service) ListBalances(ctx context.Context, tenantID string) ([]domain.CreditBalance, error) {
	return s.store.ListBalances(ctx, tenantID)
}

func (s *Service) ListEntries(ctx context.Context, filter ListFilter) ([]domain.CreditLedgerEntry, error) {
	return s.store.ListEntries(ctx, filter)
}

func (s *Service) CountEntries(ctx context.Context, filter ListFilter) (int, error) {
	return s.store.CountEntries(ctx, filter)
}

type AdjustInput struct {
	CustomerID  string `json:"customer_id"`
	AmountCents int64  `json:"amount_cents"` // Positive or negative
	Description string `json:"description"`
}

func (s *Service) Adjust(ctx context.Context, tenantID string, input AdjustInput) (domain.CreditLedgerEntry, error) {
	if input.CustomerID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("customer_id")
	}
	// Bind effective-now from the customer pin so a deduct/credit on a
	// clock-pinned customer stamps the ledger entry's created_at in
	// simulated time — same shape as Grant (line 77). Without this, the
	// AdjustAtomic path falls back to clock.Now(ctx) = wall-clock and the
	// deduction row appears out-of-band on the customer's Credits tab.
	// Audit follow-up to feedback_ctx_attr_audit (2026-05-24): Grant
	// bound, Adjust didn't.
	ctx = s.bindForCustomer(ctx, tenantID, input.CustomerID)
	if input.AmountCents == 0 {
		return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents", "cannot be zero")
	}
	// Positive adjustments are grant-shaped credit inflows (drainable
	// blocks), so they carry Grant's $1M fat-finger cap. Negative
	// adjustments are naturally bounded by the balance check in
	// AdjustAtomic — no cap needed there.
	if input.AmountCents > 100_000_000 {
		return domain.CreditLedgerEntry{}, errs.Invalid("amount_cents", "cannot exceed 1,000,000")
	}
	desc := strings.TrimSpace(input.Description)
	if desc == "" {
		return domain.CreditLedgerEntry{}, errs.Required("description")
	}

	var emit func(tx *sql.Tx, out domain.CreditLedgerEntry) error
	if s.auditLogger != nil {
		// Frozen wire strings: positive adjustments audit as
		// "credit.adjustment", deductions as "credit.deduction" — the split
		// the dashboard's severity styling keys on.
		action := "credit.adjustment"
		if input.AmountCents < 0 {
			action = "credit.deduction"
		}
		emit = func(tx *sql.Tx, out domain.CreditLedgerEntry) error {
			return s.auditLogger.LogInTx(ctx, tx, audit.Entry{
				Action:        action,
				ResourceType:  "credit",
				ResourceID:    out.ID,
				ResourceLabel: out.Description,
				Metadata: map[string]any{
					"customer_id":  out.CustomerID,
					"amount_cents": out.AmountCents,
					"description":  out.Description,
				},
			})
		}
	}
	return s.store.AdjustAtomicAudited(ctx, tenantID, input.CustomerID, desc, input.AmountCents, emit)
}
