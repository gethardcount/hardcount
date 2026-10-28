package credit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// AuditEmitter is the narrow in-tx audit seam (ADR-090). The service builds
// the emission content at the state-transition site; the store threads the
// closure onto its transaction so ledger write and audit row share fate.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// CustomerReader fetches a customer by id on a tenant- and mode-scoped
// transaction. Implemented by *customer.PostgresStore, wired in router.go.
type CustomerReader interface {
	Get(ctx context.Context, tenantID, customerID string) (domain.Customer, error)
}

// LiveBalanceReader computes a customer's live credit figure: the settled
// balance less usage not yet billed. Implemented by *billing.Engine, which
// owns the computation because it must price usage the way the period close
// does; wired in router.go. Defined here so credit does not import billing.
type LiveBalanceReader interface {
	LiveCreditBalance(ctx context.Context, tenantID, customerID string) (domain.CreditLiveBalance, error)
}

type Service struct {
	store       Store
	resolver    clock.Resolver
	auditLogger AuditEmitter
	customers   CustomerReader
	live        LiveBalanceReader
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

// SetCustomerReader wires the customer read that Grant and Adjust run before
// writing. When unwired (narrow unit tests) the read is skipped.
func (s *Service) SetCustomerReader(r CustomerReader) { s.customers = r }

// requireCustomer confirms the customer exists in the caller's tenant and
// mode. The ledger's foreign key to customers is on the id alone and
// foreign-key checks do not apply row-level security, so without this read
// the insert accepts any existing customer id, including another tenant's or
// the other mode's. Called from Grant, Adjust and the balance, ledger and
// grants reads, which the operator routes and the public cost dashboard use:
// engine callers pass a customer id they read from a row in scope. On the
// reads it is what turns
// an unknown id into 404 instead of 200 with zeros, since row-level security
// hides every ledger row of a customer the caller cannot see.
func (s *Service) requireCustomer(ctx context.Context, tenantID, customerID string) error {
	if s.customers == nil {
		return nil
	}
	if _, err := s.customers.Get(ctx, tenantID, customerID); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return errs.NotFound("customer")
		}
		return fmt.Errorf("get customer: %w", err)
	}
	return nil
}

func (s *Service) bindForCustomer(ctx context.Context, tenantID, customerID string) context.Context {
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, CustomerID: customerID})
	return bound
}

type GrantInput struct {
	CustomerID string `json:"customer_id"`
	// Currency is the grant's ISO-4217 code (ADR-129). Required.
	Currency    string     `json:"currency"`
	AmountCents int64      `json:"amount_cents"`
	Description string     `json:"description"`
	InvoiceID   string     `json:"invoice_id,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`

	// SourceCreditNoteID dedups grants created by credit-note Issue()
	// (GrantForCreditNoteTx sets it): a second grant for the same credit
	// note hits the unique partial index (migration 0093) and the store
	// returns ErrAlreadyExists.
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

// normalizeCurrency validates a grant or adjustment currency the way every
// currency input is validated (domain.ValidateCurrency: required, on the
// allowlist, no zero-decimal code) and returns it uppercase (ADR-129).
func normalizeCurrency(currency string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if err := domain.ValidateCurrency(c); err != nil {
		return "", err
	}
	return c, nil
}

func (s *Service) Grant(ctx context.Context, tenantID string, input GrantInput) (domain.CreditLedgerEntry, error) {
	if input.CustomerID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("customer_id")
	}
	currency, err := normalizeCurrency(input.Currency)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	if err := s.requireCustomer(ctx, tenantID, input.CustomerID); err != nil {
		return domain.CreditLedgerEntry{}, err
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
		CustomerID:         input.CustomerID,
		EntryType:          domain.CreditGrant,
		Currency:           currency,
		AmountCents:        input.AmountCents,
		Description:        desc,
		InvoiceID:          input.InvoiceID,
		ExpiresAt:          input.ExpiresAt,
		SourceCreditNoteID: input.SourceCreditNoteID,
		GrantKind:          input.GrantKind,
		CreatedAt:          input.At,
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
			Metadata:      grantAuditMetadata(out),
		})
	}
}

// grantAuditMetadata is the audit metadata of a grant or adjustment row. The
// keys are FROZEN (the dashboard's audit log reads them); currency lets the
// log show the amount in the row's own currency (ADR-129).
func grantAuditMetadata(out domain.CreditLedgerEntry) map[string]any {
	return map[string]any{
		"customer_id":  out.CustomerID,
		"currency":     out.Currency,
		"amount_cents": out.AmountCents,
		"description":  out.Description,
	}
}

// GrantCommitForInvoiceTx appends the commit grant on the CALLER's tx, in the
// funding invoice's currency (ADR-129) —
// invoice.FinalizeWithDates runs it inside the finalize coordinator tx so the
// status flip and the grant commit or roll back together (ADR-078 D2). An
// error here fails Finalize by design: it is operator-synchronous, loud, and
// retryable (the finalize CAS makes the retry clean). The fund-once partial
// unique index (migration 0136) is a structural backstop whose violation
// aborts the tx loudly — never caught here (poisoned-tx; see
// GrantForCreditNoteTx's contract). Flat args so the invoice store's
// consumer-side CommitFunder interface is satisfied directly (zero
// cross-domain imports).
func (s *Service) GrantCommitForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber, currency string, amountCents int64, expiresAt *time.Time) (domain.CreditLedgerEntry, error) {
	if customerID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("customer_id")
	}
	if invoiceID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("invoice_id")
	}
	if strings.TrimSpace(currency) == "" {
		// The funding invoice always has a currency; an empty one here is a
		// caller bug. The store refuses it too.
		return domain.CreditLedgerEntry{}, fmt.Errorf("commit grant for invoice %s has no currency", invoiceID)
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
		Currency:        currency,
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
	currency, err := normalizeCurrency(input.Currency)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
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
		Currency:    currency,
		AmountCents: input.AmountCents,
		Description: desc,
		InvoiceID:   input.InvoiceID,
		ExpiresAt:   input.ExpiresAt,
		// GrantKind was silently dropped here pre-2026-07-05 (the struct
		// literal omitted the field while Grant carried it), so every
		// in-tx grant minted a NULL-kind block — misclassified into the
		// paid drain class and invisible to kind-scoped reporting.
		GrantKind:          input.GrantKind,
		SourceCreditNoteID: input.SourceCreditNoteID,
		CreatedAt:          input.At,
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
			Metadata:      grantAuditMetadata(out),
		}); err != nil {
			return domain.CreditLedgerEntry{}, fmt.Errorf("audit emission: %w", err)
		}
	}
	return out, nil
}

// GrantForCreditNoteTx is the credit-note grant, on the caller's coordinator
// tx: it sets SourceCreditNoteID and appends via GrantTx, so the ledger entry
// commits atomically with the credit note's draft→issued CAS (ADR-061,
// creditnote.Issue). The 0093 (tenant, CN) dedup index still backs uniqueness,
// but the issue CAS makes a duplicate UNREACHABLE — exactly one Issue() reaches
// the grant per credit note. A conflict here would ABORT the shared
// coordinator tx, so we do NOT swallow ErrAlreadyExists: surfacing it correctly
// fails the whole Issue() rather than committing a poisoned tx. Given the CAS
// invariant it never fires; if it does, the invariant is broken.
func (s *Service) GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, creditNoteID string, input GrantInput) (domain.CreditLedgerEntry, error) {
	if creditNoteID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("credit_note_id")
	}
	input.SourceCreditNoteID = creditNoteID
	return s.GrantTx(ctx, tx, tenantID, input)
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

// ReverseForInvoiceTx hands back the credits applied to a voided invoice. The
// reversal grant is appended on the caller's *sql.Tx so it commits (or rolls
// back) atomically with the invoice status flip that drives the void. invoice.Service.Void
// threads its coordinator tx through here — a reversal failure rolls the void
// back, so the invoice never lands voided with the customer's applied credits
// still consumed. The credits are added up on that tx too, in one SQL sum
// over every usage row of the invoice (no page). The void holds the invoice
// row lock, and a credit application takes that lock first, so no application
// can land between the sum and the grant.
//
// The 0106 dedup index allows one reversal per invoice. The void refuses an
// invoice that is already voided, on the locked row, so a second reversal is
// not expected. If one comes, the index refuses the INSERT, which aborts the
// caller's tx, so the error (code credit_reversal_source_taken) is returned,
// not swallowed: the caller must fail, not carry on with a dead tx.
//
// bindForCustomer stamps the grant in simulated time when the customer is
// clock-pinned.
func (s *Service) ReverseForInvoiceTx(ctx context.Context, tx *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber string) (int64, error) {
	ctx = s.bindForCustomer(ctx, tenantID, customerID)
	sums, err := s.store.InvoiceCreditsTx(ctx, tx, tenantID, customerID, invoiceID)
	if err != nil {
		return 0, fmt.Errorf("reverse credits for invoice %s: %w", invoiceID, err)
	}

	// The reversal hands the credit back in the currency it was drawn in
	// (ADR-129). The drain draws only in the invoice's currency, so every
	// usage row of one invoice shares it, and so do the credit notes that
	// returned part of it; two currencies mean the ledger is corrupt, and
	// handing back either would be a guess.
	var used *InvoiceCredits
	for i := range sums {
		if sums[i].UsedCents == 0 && sums[i].ReturnedCents == 0 {
			continue
		}
		if used != nil {
			return 0, fmt.Errorf("reverse credits for invoice %s: its credit rows are in %s and %s; refusing to guess the currency to return", invoiceID, used.Currency, sums[i].Currency)
		}
		used = &sums[i]
	}
	if used == nil || used.UsedCents <= 0 {
		return 0, nil // No credits were applied to this invoice
	}

	// A credit note issued against this invoice while it was unpaid may already
	// have returned some of those credits to the balance (creditnote.Issue,
	// ADR-061 amendment 2026-10-12). Hand back only what is still applied.
	// Every credit-note grant on a voidable invoice is such a return: the other
	// kind is written for a PAID invoice, which cannot be voided.
	totalUsed := used.UsedCents - used.ReturnedCents
	if totalUsed <= 0 {
		return 0, nil // Credit notes already returned everything that was applied
	}

	desc := fmt.Sprintf("Reversed — invoice %s voided", invoiceNumber)
	if invoiceNumber == "" {
		desc = fmt.Sprintf("Reversed — invoice %s voided", invoiceID)
	}

	if _, err := s.store.AppendEntryTx(ctx, tx, tenantID, domain.CreditLedgerEntry{
		CustomerID:              customerID,
		EntryType:               domain.CreditGrant,
		Currency:                used.Currency,
		AmountCents:             totalUsed,
		Description:             desc,
		InvoiceID:               invoiceID,
		SourceInvoiceReversalID: invoiceID,
	}); err != nil {
		return 0, err
	}

	return totalUsed, nil
}

// ExpireCredits finds unexpired grant entries past their expiry date and creates
// negative (expiry) entries to zero them out. A grant that an owed period close
// or period invoice still draws on is skipped until that draw is done
// (ADR-071 §2). Returns the count of expired grants and any errors encountered
// during processing.
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
// reassessment made the split an explicit acceptance criterion). The rollup
// is per currency (ADR-129).
type GrantsResponse struct {
	Grants              []GrantSummary      `json:"grants"`
	RemainingByCurrency []CurrencyRemaining `json:"remaining_by_currency"`
}

// CurrencyRemaining is what is left of the customer's credit in one currency,
// split by cost-basis class (ADR-078): commit = money-backed (funded by a paid
// commit invoice), promotional = free marketing credits, other =
// legacy/unclassified grants + positive adjustments + reversals (paid drain
// class).
type CurrencyRemaining struct {
	Currency                  string `json:"currency"`
	CommitRemainingCents      int64  `json:"commit_remaining_cents"`
	PromotionalRemainingCents int64  `json:"promotional_remaining_cents"`
	OtherRemainingCents       int64  `json:"other_remaining_cents"`
}

// ListGrants returns the customer's credit blocks with drawdown state +
// kind subtotals. includeExhausted adds fully-drawn blocks (history view).
func (s *Service) ListGrants(ctx context.Context, tenantID, customerID string, includeExhausted bool) (GrantsResponse, error) {
	if customerID == "" {
		return GrantsResponse{}, errs.Required("customer_id")
	}
	if err := s.requireCustomer(ctx, tenantID, customerID); err != nil {
		return GrantsResponse{}, err
	}
	grants, err := s.store.ListGrantSummaries(ctx, tenantID, customerID, includeExhausted)
	if err != nil {
		return GrantsResponse{}, err
	}
	resp := GrantsResponse{Grants: grants, RemainingByCurrency: []CurrencyRemaining{}}
	if resp.Grants == nil {
		resp.Grants = []GrantSummary{}
	}
	byCurrency := map[string]*CurrencyRemaining{}
	for _, g := range grants {
		if g.RemainingCents <= 0 {
			continue
		}
		r := byCurrency[g.Currency]
		if r == nil {
			r = &CurrencyRemaining{Currency: g.Currency}
			byCurrency[g.Currency] = r
		}
		var sum *int64
		switch g.GrantKind {
		case string(domain.GrantKindCommit):
			sum = &r.CommitRemainingCents
		case string(domain.GrantKindPromotional):
			sum = &r.PromotionalRemainingCents
		default:
			sum = &r.OtherRemainingCents
		}
		if *sum, err = domain.SumCents(*sum, g.RemainingCents); err != nil {
			return GrantsResponse{}, fmt.Errorf("remaining credit in %s: %w", g.Currency, err)
		}
	}
	for _, r := range byCurrency {
		resp.RemainingByCurrency = append(resp.RemainingByCurrency, *r)
	}
	sort.Slice(resp.RemainingByCurrency, func(i, j int) bool {
		return resp.RemainingByCurrency[i].Currency < resp.RemainingByCurrency[j].Currency
	})
	return resp, nil
}

// GetBalance returns the customer's balances, one per currency (ADR-129).
func (s *Service) GetBalance(ctx context.Context, tenantID, customerID string) ([]domain.CreditBalance, error) {
	if err := s.requireCustomer(ctx, tenantID, customerID); err != nil {
		return nil, err
	}
	return s.store.GetBalance(ctx, tenantID, customerID)
}

// SpendableBalance is the credit a draw could use right now, per currency:
// the ledger balance less any grant past its expiry that the sweep has not
// written off yet. "Now" is the customer's own clock (the frozen time of a
// test clock the customer is pinned to), the same instant a draw would judge
// expiry at.
func (s *Service) SpendableBalance(ctx context.Context, tenantID, customerID string) (map[string]int64, error) {
	ctx = s.bindForCustomer(ctx, tenantID, customerID)
	return s.store.GetSpendableBalance(ctx, tenantID, customerID, clock.Now(ctx))
}

// SetLiveBalanceReader wires the live figure behind LiveBalance.
func (s *Service) SetLiveBalanceReader(r LiveBalanceReader) { s.live = r }

// LiveBalance is the customer's credit position right now: settled balance,
// usage not yet billed, and the estimate of what is left (issue #1019).
//
// The customer read comes first. Every row behind the figure is hidden by
// row-level security for a customer of another tenant or the other mode, so
// without it such an id would answer 200 with zeros instead of 404.
func (s *Service) LiveBalance(ctx context.Context, tenantID, customerID string) (domain.CreditLiveBalance, error) {
	if s.live == nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live balance reader not wired")
	}
	if err := s.requireCustomer(ctx, tenantID, customerID); err != nil {
		return domain.CreditLiveBalance{}, err
	}
	return s.live.LiveCreditBalance(ctx, tenantID, customerID)
}

func (s *Service) ListBalances(ctx context.Context, tenantID string, limit, offset int) ([]domain.CreditBalance, int, error) {
	return s.store.ListBalances(ctx, tenantID, limit, offset)
}

func (s *Service) ListEntries(ctx context.Context, filter ListFilter) ([]domain.CreditLedgerEntry, error) {
	if err := s.requireCustomer(ctx, filter.TenantID, filter.CustomerID); err != nil {
		return nil, err
	}
	return s.store.ListEntries(ctx, filter)
}

func (s *Service) CountEntries(ctx context.Context, filter ListFilter) (int, error) {
	return s.store.CountEntries(ctx, filter)
}

type AdjustInput struct {
	CustomerID string `json:"customer_id"`
	// Currency is the balance the adjustment changes (ADR-129). Required.
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"` // Positive or negative
	Description string `json:"description"`
}

func (s *Service) Adjust(ctx context.Context, tenantID string, input AdjustInput) (domain.CreditLedgerEntry, error) {
	if input.CustomerID == "" {
		return domain.CreditLedgerEntry{}, errs.Required("customer_id")
	}
	currency, err := normalizeCurrency(input.Currency)
	if err != nil {
		return domain.CreditLedgerEntry{}, err
	}
	if err := s.requireCustomer(ctx, tenantID, input.CustomerID); err != nil {
		return domain.CreditLedgerEntry{}, err
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
				Metadata:      grantAuditMetadata(out),
			})
		}
	}
	return s.store.AdjustAtomicAudited(ctx, tenantID, input.CustomerID, currency, desc, input.AmountCents, emit)
}
