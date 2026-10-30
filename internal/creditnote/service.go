package creditnote

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/money"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// withoutEmpty drops empty-string values from audit metadata so optional
// fields (provider failure reasons, mostly) appear only when they carry a
// fact — an empty key in every row is noise that reads like a schema.
func withoutEmpty(md map[string]any) map[string]any {
	for k, v := range md {
		if s, ok := v.(string); ok && s == "" {
			delete(md, k)
		}
	}
	return md
}

// InvoiceReader reads invoice data for credit note validation.
type InvoiceReader interface {
	Get(ctx context.Context, tenantID, id string) (domain.Invoice, error)
	ListLineItems(ctx context.Context, tenantID, invoiceID string) ([]domain.InvoiceLineItem, error)
	ApplyCreditNote(ctx context.Context, tenantID, id string, amountCents int64) (domain.Invoice, error)
	// LockForCreditNoteTx locks the invoice row on Issue()'s coordinator tx and
	// returns it as it is under the lock; Issue() decides from that row.
	LockForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string) (domain.Invoice, error)
	// ApplyCreditNoteTx reduces amount_due by reduceCents and takes
	// creditsReturnedCents of applied customer credits back off the invoice, on
	// the caller's coordinator tx, so both commit atomically with Issue()'s
	// draft→issued CAS (ADR-061). It refuses rather than clamps when either
	// exceeds what the invoice holds.
	ApplyCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, id string, reduceCents, creditsReturnedCents int64) (domain.Invoice, error)
	// SettleZeroDueTx marks the invoice paid on Issue()'s coordinator tx when
	// nothing is left due, unless a charge hold is set; paid reports whether
	// it did. Not settling is not an error.
	SettleZeroDueTx(ctx context.Context, tx *sql.Tx, tenantID, id string, paidAt time.Time) (inv domain.Invoice, paid bool, err error)
}

// DunningResolver closes the invoice's dunning run, active or escalated, after
// a credit note has marked the invoice paid. dunning.Service implements it.
type DunningResolver interface {
	ResolveByInvoice(ctx context.Context, tenantID, invoiceID string, resolution domain.DunningResolution) error
}

// Refunder processes refunds via the payment provider.
//
// Convergence contract (ADR-063 amendment, 2026-08-04): the idempotency key
// protects only the individual request from transport-level duplication —
// Stripe v1 keys EXPIRE after ~24h, so the key alone cannot make a late
// retry converge on an earlier attempt. Durable exactly-once comes from the
// other two methods: every create stamps metadata hardcount_cn_id, GetRefund
// reads current provider truth (a read never replays a stale saved
// response), and FindRefundForCreditNote recovers a refund Hardcount lost the
// id of. Callers reconcile-then-create; they never blind-create on retry.
type Refunder interface {
	// Returns the refund id + Stripe's create-time status (mapped to a Hardcount
	// refund_status). A pending result settles later via a refund webhook.
	// creditNoteID is stamped as metadata for later adoption.
	//
	// An error says through OutcomeUnknown() (see refundOutcomeUnknown)
	// whether Stripe may have made the refund anyway. Only an error that
	// reports false records "failed"; any other records "unknown" (#965).
	CreateRefund(ctx context.Context, paymentIntentID string, amountCents int64, idempotencyKey, creditNoteID string) (string, domain.RefundStatus, error)
	// GetRefund returns the refund's CURRENT provider status plus the
	// provider's failure reason (empty unless failed).
	GetRefund(ctx context.Context, refundID string) (domain.RefundStatus, string, error)
	// FindRefundForCreditNote returns the id+status of a LIVE
	// (succeeded/pending) refund carrying this credit note's metadata on
	// the given PaymentIntent, or ("","",nil) when none exists. Failed
	// matches are never returned — adoption exists to prevent duplicates,
	// and a dead attempt returned here would shadow the create leg forever.
	FindRefundForCreditNote(ctx context.Context, paymentIntentID, creditNoteID string) (string, domain.RefundStatus, error)
}

// refundOutcomeUnknown reports whether a CreateRefund error leaves open that
// Stripe made the refund. The refunder's errors answer through
// OutcomeUnknown(), declared here because this package may not import the
// payment package. An error that does not answer counts as unknown: a refusal
// has to be proven, the way classifyStripeError treats charges, because
// recording "failed" for a refund that went through invites a second refund
// by hand.
func refundOutcomeUnknown(err error) bool {
	var oe interface{ OutcomeUnknown() bool }
	if errors.As(err, &oe) {
		return oe.OutcomeUnknown()
	}
	return true
}

// refundRequestNotRun reports whether a CreateRefund error says Stripe ran
// nothing for the request: it refused the API key before running it (a 401
// invalid key, a 403 key without refund permission), or nothing was sent.
// Stripe saves no result for such a request
// (https://docs.stripe.com/error-low-level#content-errors), so when
// RetryRefund resends an unclear request, this error says nothing about that
// earlier request.
func refundRequestNotRun(err error) bool {
	var nr interface{ RequestNotRun() bool }
	return errors.As(err, &nr) && nr.RequestNotRun()
}

// refundStillUnknownError is RetryRefund's answer when Stripe ran nothing for
// a resend: the earlier request's result is still unknown, so the refunder's
// own message ("the customer was not refunded") would be false.
type refundStillUnknownError struct{ err error }

func (e *refundStillUnknownError) Error() string {
	return "stripe refund retry did not run; the earlier request's result is still unknown: " + e.err.Error()
}
func (e *refundStillUnknownError) Unwrap() error        { return e.err }
func (e *refundStillUnknownError) OutcomeUnknown() bool { return true }
func (e *refundStillUnknownError) OperatorSafeMessage() string {
	return "Stripe refused this retry before running it, for example because of the Stripe API key, so the result of the earlier refund request is still unknown. Don't refund this payment in Stripe. Check the Stripe key, then retry the refund."
}
func (e *refundStillUnknownError) OperatorErrorCode() string { return "payment_outcome_unknown" }

// issueRefundKey is the idempotency key of Issue's refund request. Issue
// sends no other, so RetryRefund resends it for a note whose refund leg has
// no recorded outcome.
func issueRefundKey(creditNoteID string) string { return "hardcount_cn_" + creditNoteID }

// refundOutcomeWriteTimeout bounds the work that records what a Stripe refund
// call did, once it runs detached (afterRefundCall): the database write and,
// after a resend, the read of the refund's current status at Stripe. If the
// limit runs out, nothing is recorded and the note keeps its status, which
// the next retry settles by search or by the same resend.
const refundOutcomeWriteTimeout = 10 * time.Second

// afterRefundCall is the context for recording what a Stripe refund call
// did. It keeps ctx's values (tenant, livemode, test clock, actor) and drops
// its cancellation. A refund asked for through the API runs on the request
// context, which the route's Timeout middleware ends after 30 seconds
// (internal/api/router.go). The Stripe client also allows 30 seconds, but its
// clock starts later, so when Stripe hangs, the request context ends during
// the call. Stripe may have made the refund by then, and a write on the
// ended context is refused: the note would keep a status that says less, or
// something false. The card settle detaches for the same reason
// (payment/settlement.go).
func afterRefundCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), refundOutcomeWriteTimeout)
}

// CreditGranter is the credit ledger as Issue() and the commit relief use it.
type CreditGranter interface {
	// GrantForCreditNoteTx grants on the caller's coordinator tx, so the credit
	// ledger entry commits atomically with Issue()'s draft→issued CAS (ADR-061).
	// Credit the invoice drew from credit blocks goes back to those blocks,
	// with their expiry and kind (ADR-132).
	// The 0093 dedup index still backs it, but the CAS makes it unreachable: a
	// dedup conflict here would mean a second CAS winner, which cannot happen.
	GrantForCreditNoteTx(ctx context.Context, tx *sql.Tx, tenantID, creditNoteID string, input CreditGrantInput) error
	// ADR-080 relief pair. Lock reads the funded commit grant's frozen
	// state (granted G, consumed C, cn_retired K) under the relief locks
	// on the caller's tx; Retire retires an explicit slice, bumps K, and
	// enqueues credit.commit_retired in the same tx.
	LockCommitGrantForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string) (granted, consumed, cnRetired int64, found bool, err error)
	RetireCommitSliceForReliefTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID, creditNoteID string, slice, refundedGrossCents, grossPaidCents int64) (remainingAfter int64, err error)
}

// TaxReverser issues a reversal of the invoice's committed tax transaction.
// Called from Issue when the invoice has a non-empty TaxTransactionID. The
// implementation (billing.Engine.ReverseTax) resolves the tenant's tax
// provider and forwards the ReversalRequest. Optional — when nil, Issue
// proceeds without reversing upstream tax (suitable for tenants on
// none/manual providers, or for tests).
type TaxReverser interface {
	ReverseTax(ctx context.Context, tenantID string, req tax.ReversalRequest) (*tax.ReversalResult, error)
}

// NumberGenerator generates sequential credit note numbers on the
// transaction that inserts the note, so a rolled-back create leaves no gap.
type NumberGenerator interface {
	NextCreditNoteNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error)
}

type CreditGrantInput struct {
	CustomerID string
	// Currency is the invoice's: the credit is the money the note returns
	// (ADR-129).
	Currency    string
	AmountCents int64
	Description string
	InvoiceID   string
	// InvoicePaid is set on the paid branch: the amount is the note's credit
	// channel, and the customer's balance rises by exactly that amount. The
	// note's drawn share, pro rata to how the invoice was paid, goes back to
	// the grants it came from where it stays spendable (ADR-132). Unset on the
	// unpaid branch, where the amount is applied credit the note relieves.
	InvoicePaid bool
	// InvoiceTotalCents is the invoice's total, the base of that pro-rata
	// split. Set with InvoicePaid.
	InvoiceTotalCents int64
}

type Service struct {
	store    Store
	invoices InvoiceReader
	refunder Refunder
	credits  CreditGranter
	numbers  NumberGenerator
	taxRev   TaxReverser
	audit    AuditEmitter
	resolver clock.Resolver
	dunning  DunningResolver
	// Refunds made in Stripe outside Hardcount (ADR-134): see
	// SetStripeRefundRecording and SetRefundReader.
	refunds      StripeRefundStore
	piInvoices   PaymentIntentInvoiceFinder
	outbox       OutboxEnqueuer
	refundReader RefundReader
}

// AuditEmitter is the narrow in-tx audit seam (ADR-090). Issue() emits the
// credit_note.issued row on its own coordinator tx — one canonical row for
// operator issues, engine adjustments, reconciler retries, and catchup.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

func NewService(store Store, invoices InvoiceReader, refunder Refunder, credits ...CreditGranter) *Service {
	s := &Service{store: store, invoices: invoices, refunder: refunder}
	if len(credits) > 0 {
		s.credits = credits[0]
	}
	return s
}

// SetAuditLogger wires in-tx audit emission for Issue()/relief (ADR-090).
// Optional — nil skips emission (unit-test fakes).
func (s *Service) SetAuditLogger(a AuditEmitter) { s.audit = a }

// SetResolver wires the ADR-030 time provider. Without it every operator
// entry point here stamps WALL-CLOCK time onto rows that belong to a
// clock-pinned invoice — the exact leak ADR-030 exists to close ("bind
// effective-now at every operator entry point on a clock-pinned entity";
// credit notes and their CreditGrant ledger rows are named as simulated-when-
// pinned in its table).
//
// The engine-driven clawback paths were already correct because their CALLERS
// bind (billOnePeriod's in-band downgrade, and RetryPendingClawbackIssueForClock
// which is handed frozen_time explicitly). Only the operator-initiated entry
// points were unbound, which is why an engine clawback landed on the simulated
// axis while an operator credit note on the SAME invoice landed on wall-clock —
// dated weeks before the invoice it credits.
func (s *Service) SetResolver(r clock.Resolver) { s.resolver = r }

// bindForInvoice pins ctx to the source invoice's effective-now, so every
// clock.Now(ctx) below — the credit_note row, its line items, the credit-ledger
// grant, amount_due relief — lands in the invoice's own time domain. Mirrors
// invoice.Service.bindForInvoice. Unresolvable pin or unwired resolver leaves
// ctx untouched (wall-clock), which is correct for a non-simulated invoice.
func (s *Service) bindForInvoice(ctx context.Context, tenantID, invoiceID string) context.Context {
	if s.resolver == nil || invoiceID == "" {
		return ctx
	}
	bound, _ := clock.BindEffectiveNow(ctx, s.resolver, clock.Pin{TenantID: tenantID, InvoiceID: invoiceID})
	return bound
}

// SetDunningResolver wires the dunning service, so a credit note that marks
// a dunned invoice paid also closes its dunning run. Without it an escalated
// run stays escalated, because the dunning ticks skip escalated runs. Optional:
// nil leaves the run to its next due tick (unit-test fakes).
func (s *Service) SetDunningResolver(r DunningResolver) { s.dunning = r }

// SetNumberGenerator sets the sequential number generator (breaks circular dep).
func (s *Service) SetNumberGenerator(ng NumberGenerator) {
	s.numbers = ng
}

// SetTaxReverser wires the billing engine (or test stub) that issues
// upstream tax reversals when a credit note is issued. Optional — when
// unset, Issue skips the reversal step and logs.
func (s *Service) SetTaxReverser(tr TaxReverser) {
	s.taxRev = tr
}

// CreateInput is the public payload for POST /v1/credit-notes. Three
// allocation fields mirror Stripe (`refund_amount` / `credit_amount` /
// `out_of_band_amount`) and Lago (`refund_amount_cents` /
// `credit_amount_cents` / `offset_amount_cents`): the operator splits
// the total across (a) Stripe refund to the payment method,
// (b) restore-to-customer-credit-balance, and (c) handled outside
// Stripe (cash, ACH, manual adjustment).
//
// For paid invoices, the three amounts must sum to the CN total. For
// unpaid invoices the allocation is ignored (the CN reduces amount_due
// directly; if customer credits were applied to the invoice after the note
// was sized, Issue returns the credits it displaces to the balance). If the caller leaves all three fields zero on a paid
// invoice, the default is `credit_amount = total` — the safest fallback
// (no cash movement, restorable later). Legacy callers can pass
// RefundType="refund"/"credit" and the field is translated to the
// equivalent single-channel split.
type CreateInput struct {
	InvoiceID            string            `json:"invoice_id"`
	Reason               string            `json:"reason"`
	Lines                []CreditLineInput `json:"lines"`
	RefundAmountCents    int64             `json:"refund_amount_cents"`
	CreditAmountCents    int64             `json:"credit_amount_cents"`
	OutOfBandAmountCents int64             `json:"out_of_band_amount_cents"`
	// RefundType is the legacy single-channel selector ("refund" |
	// "credit"). Kept for back-compat — callers that set it have the
	// matching allocation field populated automatically when the new
	// fields are all zero. New integrations should use the three
	// explicit *_cents fields directly.
	RefundType string `json:"refund_type,omitempty"`
	AutoIssue  bool   `json:"auto_issue"` // create + issue atomically
	// IssuePending marks an AUTO-ISSUE clawback draft (migration 0121): created
	// in-tx with a subscription downgrade/removal/qty-decrease via a tx-accepting
	// create, then issued post-commit. If Issue() never runs (crash in the
	// post-commit window, status still 'draft') RetryPendingClawbackIssue
	// re-issues it; a post-flip partial failure is not auto-recovered (see that
	// method). Set only by CreateAdjustmentDraftTx; operator HTTP creates leave
	// it false. NOT a JSON/API field — callers set it in Go.
	IssuePending bool `json:"-"`
	// StripeRefundID links a refund made in Stripe outside Hardcount as this
	// note's card refund (ADR-134): refund_amount_cents must be the refund's
	// amount, the refund must be of this invoice's payment and not made by
	// Hardcount. The note's refund is then done, and Hardcount calls nothing
	// at Stripe for it.
	StripeRefundID string `json:"stripe_refund_id,omitempty"`
	// linked is the refund StripeRefundID names, as Stripe reported it; set by
	// Create after reading it.
	linked *linkedStripeRefund
}

type CreditLineInput struct {
	InvoiceLineItemID string `json:"invoice_line_item_id,omitempty"`
	Description       string `json:"description"`
	Quantity          int64  `json:"quantity"`
	UnitAmountCents   int64  `json:"unit_amount_cents"`
}

func (s *Service) Create(ctx context.Context, tenantID string, input CreateInput) (domain.CreditNote, error) {
	// ADR-030: pin ctx to the source invoice's clock before anything is
	// written, so a credit note against a clock-pinned invoice is stamped in
	// that invoice's own time domain rather than wall-clock.
	ctx = s.bindForInvoice(ctx, tenantID, input.InvoiceID)
	// In-flight payment gate. An OPERATOR credit note must not reduce an
	// invoice's amount_due while its payment is in flight (processing/unknown):
	// at settle, MarkPaid records amount_paid off the now-lower amount_due
	// (invoice/postgres.go KNOWN EDGE), under-sizing the refund cap. So block it
	// — the operator settles or cancels the payment first. Consistent with
	// Stripe's open-payment lifecycle posture (it blocks void/edit/uncollectible
	// while a payment is open) and with our own RecordPayment guard
	// (invoice/service.go: "a charge is already in flight … wait or cancel").
	//
	// payment_status is the sufficient signal: a PAID invoice reads 'succeeded'
	// and takes the refund/credit-balance branch (never reduces amount_due), so
	// it is never gated. The AUTOMATED clawback paths (ADR-050 unpaid-source:
	// CreateAndIssueAdjustment / CreateAdjustmentDraftTx) call create() DIRECTLY,
	// bypassing this 409 gate — they cannot bounce a cancel/downgrade back to a
	// human. Instead they DEFER: Issue() leaves the draft unissued while the
	// source is in-flight and the reconciler issues it once the source settles
	// (ADR-059). Not-found falls through to create(), which raises the
	// canonical not-found/validation error; any other read error fails — the
	// gate cannot be skipped on a read it could not make.
	if input.InvoiceID != "" {
		inv, err := s.invoices.Get(ctx, tenantID, input.InvoiceID)
		switch {
		case err == nil:
			if b := domain.PaymentBlocksAction(inv, domain.ActionIssueCreditNote); b.Blocked {
				return domain.CreditNote{}, errs.InvalidState(b.Message).WithCode(b.Code)
			}
		case !errors.Is(err, errs.ErrNotFound):
			return domain.CreditNote{}, fmt.Errorf("read invoice for the in-flight payment check: %w", err)
		}
	}
	input.linked = nil
	if strings.TrimSpace(input.StripeRefundID) != "" && input.InvoiceID != "" {
		input.StripeRefundID = strings.TrimSpace(input.StripeRefundID)
		link, err := s.checkLinkedStripeRefund(ctx, tenantID, input)
		if err != nil {
			return domain.CreditNote{}, err
		}
		input.linked = link
		return s.createAndIssueLinked(ctx, tenantID, input)
	}
	return s.create(ctx, tenantID, input, nil)
}

// create is Create's tx-aware core. tx==nil uses the store's own transaction
// (the operator HTTP path). A non-nil tx threads the credit-note insert into
// the CALLER's transaction (coordinator-owned *sql.Tx, ADR-056) so the note
// commits ATOMICALLY with the caller's other writes — used by
// CreateAdjustmentDraftTx so a subscription item delete and its clawback
// obligation roll back together.
func (s *Service) create(ctx context.Context, tenantID string, input CreateInput, tx *sql.Tx) (domain.CreditNote, error) {
	if input.InvoiceID == "" {
		return domain.CreditNote{}, errs.Required("invoice_id")
	}
	if strings.TrimSpace(input.Reason) == "" {
		return domain.CreditNote{}, errs.Required("reason")
	}
	if len(input.Lines) == 0 {
		return domain.CreditNote{}, errs.Invalid("lines", "at least one line item is required")
	}

	for i, line := range input.Lines {
		if line.Quantity <= 0 {
			return domain.CreditNote{}, errs.Invalid("lines", fmt.Sprintf("line %d: quantity must be positive", i+1))
		}
		if line.UnitAmountCents <= 0 {
			return domain.CreditNote{}, errs.Invalid("lines", fmt.Sprintf("line %d: amount must be greater than 0", i+1))
		}
	}

	// Verify invoice exists and is finalized or paid
	inv, err := s.invoices.Get(ctx, tenantID, input.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("invoice not found: %w", err)
	}
	if inv.Status == domain.InvoiceVoided {
		return domain.CreditNote{}, errs.InvalidState("cannot create credit notes for voided invoices")
	}
	// Two more sources are allowed, for the engine's issue_pending relief
	// note only (a cancel, plan swap or decrease crediting unused time):
	//   - a DRAFT funding invoice (pause_collection keep_as_draft). Issue
	//     holds the note until the invoice is finalized, and voids it if the
	//     draft is voided instead (ADR-118). An operator credit note still
	//     needs an issued invoice — a draft is edited, not credited.
	//   - a WRITTEN-OFF (uncollectible) funding invoice. It is billed but
	//     unpaid, so the note lowers what it asks for, as on an unpaid one, and
	//     a later payment pays only the time used (ADR-050 amendment
	//     2026-10-23). An operator credit note on a written-off invoice is
	//     still refused here.
	reliefOnDraft := input.IssuePending && inv.Status == domain.InvoiceDraft
	reliefOnWrittenOff := input.IssuePending && inv.Status == domain.InvoiceUncollectible
	if inv.Status != domain.InvoiceFinalized && inv.Status != domain.InvoicePaid && !reliefOnDraft && !reliefOnWrittenOff {
		return domain.CreditNote{}, errs.InvalidState("can only create credit notes for finalized or paid invoices")
	}
	// ADR-078 D4: commit funding invoices are cash instruments — no credit
	// notes, phase 1. Pre-payment, a concession CN would shrink the cash
	// collected while the grant stays configured full-size; post-payment, a
	// refund CN returns cash while the funded block stays drawable (and a
	// credit-settled CN would ADD a second grant on top). Unwind paths:
	// unpaid → void the invoice (retires the grant, ADR-078 D3); paid →
	// blocked until the CN-retire leg ships (phase 2 — trigger: first DP
	// commit-refund ask). Checked in the shared core so the operator path
	// and the automated clawback paths carry the same rule.
	invLines, err := s.invoices.ListLineItems(ctx, tenantID, input.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("list invoice line items: %w", err)
	}
	for _, li := range invLines {
		if li.IsCommitLine() {
			// Ordinary line-based CNs stay blocked on commit invoices in
			// BOTH states: cash != face on a discounted commit, so line-cap
			// semantics are wrong. Route by state to the correct unwind.
			if inv.Status == domain.InvoicePaid {
				return domain.CreditNote{}, errs.InvalidState(
					"this invoice funds a prepaid commit — use commit relief (POST /v1/credit-notes with a commit_relief block) to refund it; the relief derives the cash from the commit's remaining credits")
			}
			return domain.CreditNote{}, errs.InvalidState(
				"this invoice funds a prepaid commit — void the unpaid invoice to cancel the commit instead")
		}
	}

	// Calculate totals. Line amounts are interpreted as tax-inclusive
	// (gross) so the caller's sum matches the invoice's gross total the
	// caps below use as the ceiling. The tax portion is back-solved from
	// the invoice's tax ratio (see proportional tax block below) so a CN
	// that refunds half the invoice also reverses half the tax.
	// Overflow-checked: an absurd quantity × price wrapped to a negative total
	// that slipped under the over-credit cap below.
	var subtotal int64
	lineAmounts := make([]int64, len(input.Lines))
	for i, line := range input.Lines {
		amount, err := domain.MulCents(line.Quantity, line.UnitAmountCents)
		if err == nil {
			subtotal, err = domain.AddCents(subtotal, amount)
		}
		if err != nil {
			return domain.CreditNote{}, errs.Invalid("lines", fmt.Sprintf("line %d: amount is too large", i+1))
		}
		lineAmounts[i] = amount
	}

	// A line may name the invoice line it credits. The column has no foreign
	// key, so the id is checked here: it must be a line of the named invoice,
	// read on the caller's tenant and mode.
	var ownLines map[string]bool
	for i, line := range input.Lines {
		if line.InvoiceLineItemID == "" {
			continue
		}
		if ownLines == nil {
			invLines, err := s.invoices.ListLineItems(ctx, tenantID, input.InvoiceID)
			if err != nil {
				return domain.CreditNote{}, fmt.Errorf("list invoice line items: %w", err)
			}
			ownLines = make(map[string]bool, len(invLines))
			for _, li := range invLines {
				ownLines[li.ID] = true
			}
		}
		if !ownLines[line.InvoiceLineItemID] {
			return domain.CreditNote{}, errs.Invalid(fmt.Sprintf("lines[%d].invoice_line_item_id", i), "not a line of this invoice")
		}
	}

	// Line rows are built up front and committed in the SAME transaction as
	// the header (CreateUnderInvoiceLock) — pre-fix they were inserted one by
	// one after the header committed, so a partial failure left an orphan
	// credit note with a non-zero total and zero lines.
	lineRows := make([]domain.CreditNoteLineItem, 0, len(input.Lines))
	for i, line := range input.Lines {
		lineRows = append(lineRows, domain.CreditNoteLineItem{
			InvoiceLineItemID: line.InvoiceLineItemID,
			Description:       line.Description,
			Quantity:          line.Quantity,
			UnitAmountCents:   line.UnitAmountCents,
			AmountCents:       lineAmounts[i],
		})
	}

	// Build + insert the credit note under a per-invoice advisory lock so the
	// over-credit / over-refund caps are evaluated against a snapshot that
	// can't change underneath us. Two concurrent Create calls for the same
	// invoice serialize on the lock: the second sees the first's note in
	// `existingCNs` and is capped correctly, closing the TOCTOU where both
	// read the same pre-state and both inserted past the invoice total.
	if input.StripeRefundID != "" && input.linked == nil {
		// Only Create checks a linked refund against Stripe; no other caller
		// may name one.
		return domain.CreditNote{}, errs.Invalid("stripe_refund_id", "a refund made in Stripe is linked only through POST /v1/credit-notes")
	}
	// linkRow is the refund's row the note records, set by buildFn when the
	// note links a refund made in Stripe that Hardcount flagged.
	var linkRow *domain.InvoiceStripeRefund
	buildFn := func(lockTx *sql.Tx, existingCNs []domain.CreditNote) (domain.CreditNote, error) {
		// A linked refund made in Stripe (ADR-134) that a note already
		// records is refused as such, before the caps that note now fills.
		if input.linked != nil {
			var err error
			if linkRow, err = s.linkUnderLockTx(ctx, lockTx, input.InvoiceID, input.linked, existingCNs); err != nil {
				return domain.CreditNote{}, err
			}
		}
		// Validate: total credit notes cannot exceed invoice total.
		// For unpaid invoices, also cap at current amount_due.
		var existingTotal, existingTaxReversed int64
		for _, cn := range existingCNs {
			if cn.Status != domain.CreditNoteVoided {
				var err error
				if existingTotal, err = domain.SumCents(existingTotal, cn.TotalCents); err != nil {
					return domain.CreditNote{}, fmt.Errorf("credit notes on invoice %s: total: %w", input.InvoiceID, err)
				}
				if existingTaxReversed, err = domain.SumCents(existingTaxReversed, cn.TaxAmountCents); err != nil {
					return domain.CreditNote{}, fmt.Errorf("credit notes on invoice %s: tax: %w", input.InvoiceID, err)
				}
			}
		}
		if subtotal > inv.TotalAmountCents-existingTotal { // never existingTotal+subtotal: that sum can wrap
			remaining := inv.TotalAmountCents - existingTotal
			if remaining <= 0 {
				return domain.CreditNote{}, errs.InvalidState("invoice has already been fully credited")
			}
			return domain.CreditNote{}, errs.Invalid("lines", fmt.Sprintf("credit note amount exceeds remaining creditable amount (%.2f)", float64(remaining)/100))
		}
		// For unpaid invoices (a written-off one is unpaid), a credit note
		// cannot exceed what the invoice can still be relieved of: the amount
		// due plus the customer credits applied to it (Issue returns the
		// credits a note displaces).
		// This is an early refusal, not a guarantee: a draft reserves nothing,
		// and both figures can move before the draft is issued. Issue() is
		// where the note is fitted to the invoice as it then stands.
		if (inv.Status == domain.InvoiceFinalized || inv.Status == domain.InvoiceUncollectible) && subtotal > inv.UnpaidRelievableCents() {
			return domain.CreditNote{}, errs.Invalid("lines", fmt.Sprintf(
				"credit note amount (%.2f) exceeds what this invoice can still be credited (%.2f due plus %.2f of applied credits)",
				float64(subtotal)/100, float64(inv.AmountDueCents)/100, float64(inv.CreditsAppliedCents)/100))
		}

		cn, err := s.buildCreditNote(ctx, lockTx, tenantID, input, inv, subtotal, existingTotal, existingTaxReversed, existingCNs)
		if err != nil || input.linked == nil {
			return cn, err
		}
		// The note records a refund made in Stripe (ADR-134): its card refund
		// is that refund, already done.
		cn.StripeRefundID = input.linked.facts.ID
		cn.RefundStatus = input.linked.facts.Status.CreditNoteRefundStatus()
		cn.RecordedFromStripe = true
		return cn, nil
	}

	// ADR-090 in-tx emission. The `create` row rides the SAME transaction as the
	// header+lines insert — the store's own tx on the operator HTTP path, the
	// caller's coordinator tx on the in-tx clawback path — so a credit note can
	// never commit without its evidence (nor evidence without the note), and
	// both create paths carry the identical row (the own-tx/caller-tx divergence
	// ADR-090 §3 closed for grants).
	//
	// Fires once per INSERTED row: a build rejection (cap, allocation) returns
	// before the insert and records nothing. An AUTO-ISSUE create emits this one
	// `create` row here and the separate `credit_note.issued` row in Issue() —
	// two rows for two facts, never a duplicate create.
	//
	// The commit-relief coordinator (CreateAndIssueCommitRelief) deliberately
	// does NOT route through here: it creates+issues in a single tx with no draft
	// ever observable, and its `credit_note.issued` row is the canonical evidence.
	var emit func(tx *sql.Tx, created domain.CreditNote) error
	if s.audit != nil {
		emit = func(tx *sql.Tx, created domain.CreditNote) error {
			return s.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionCreate,
				ResourceType:  "credit_note",
				ResourceID:    created.ID,
				ResourceLabel: created.CreditNoteNumber,
				Metadata: withoutEmpty(map[string]any{
					"invoice_id":       created.InvoiceID,
					"customer_id":      created.CustomerID,
					"total_cents":      created.TotalCents,
					"reason":           created.Reason,
					"currency":         created.Currency,
					"stripe_refund_id": created.StripeRefundID,
				}),
			})
		}
	}
	if input.linked != nil {
		// The flagged row the note records is marked recorded on the
		// transaction that inserts the note.
		audited := emit
		emit = func(tx *sql.Tx, created domain.CreditNote) error {
			if linkRow != nil {
				if _, err := s.refunds.LinkStripeRefundTx(ctx, tx, linkRow.ID, created.ID); err != nil {
					return err
				}
			}
			if audited != nil {
				return audited(tx, created)
			}
			return nil
		}
	}

	var cn domain.CreditNote
	if tx != nil {
		// Coordinator-owned tx: the credit note commits atomically with the
		// caller's other writes (e.g. a subscription item delete).
		cn, err = s.store.CreateUnderInvoiceLockTxAudited(ctx, tx, tenantID, input.InvoiceID, lineRows, buildFn, emit)
	} else {
		cn, err = s.store.CreateUnderInvoiceLockAudited(ctx, tenantID, input.InvoiceID, lineRows, buildFn, emit)
	}
	if err != nil {
		return domain.CreditNote{}, err
	}

	return cn, nil
}

// CreateAndIssueAdjustment creates and immediately issues an adjustment credit
// note that reduces an unpaid, finalized invoice's amount_due by grossCents
// (a tax-inclusive amount). The billing engine calls it on mid-period
// subscription cancel to settle an unpaid in-advance invoice down to the
// consumed portion (#22): the unconsumed prebill is removed from the
// receivable, and because the invoice is unpaid no customer-balance credit or
// card refund is produced — Issue's unpaid branch simply lowers amount_due and
// reverses the proportional output tax. The note is a single gross-amount
// line; Create caps grossCents at the invoice's current amount_due. Returns the
// issued credit note. Idempotency is the caller's responsibility; a second call
// would create a second adjustment and is rejected by Create's amount_due cap
// once the first lands.
func (s *Service) CreateAndIssueAdjustment(ctx context.Context, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error) {
	// Calls the create() CORE directly (not the public Create) so the automated
	// ADR-050 unpaid-source clawback BYPASSES the operator in-flight 409 gate in
	// Create. Unlike the operator (who is told to settle/cancel the charge and
	// retry), the automated cancel/downgrade has no human to bounce to, so it
	// must proceed — but it DEFERS rather than reduces: Issue() leaves the draft
	// unissued while the source is in-flight (ADR-059), and the reconciler issues
	// it once the source settles. issue_pending=true makes that deferred draft
	// (and any post-commit Issue() failure) recoverable by RetryPendingClawbackIssue.
	cn, err := s.create(ctx, tenantID, CreateInput{
		InvoiceID: invoiceID,
		Reason:    reason,
		Lines: []CreditLineInput{{
			Description:     description,
			Quantity:        1,
			UnitAmountCents: grossCents,
		}},
		IssuePending: true,
	}, nil)
	if err != nil {
		return domain.CreditNote{}, err
	}
	return s.Issue(ctx, tenantID, cn.ID)
}

// CreateAdjustmentDraftTx creates the clawback adjustment credit note as a
// DRAFT inside the CALLER's transaction (coordinator-owned *sql.Tx, ADR-056),
// marked issue_pending. It commits atomically with the caller's other writes
// (e.g. a subscription item delete), so a credit-note failure rolls back the
// item change — closing the post-commit fire-and-forget gap where a removed
// item could leave the customer un-credited. The caller issues it post-commit
// via Issue(); if that fails the note stays draft+issue_pending and
// RetryPendingClawbackIssue re-issues it. Returns the DRAFT (not yet issued),
// so the caller can collect its id for the post-commit Issue.
func (s *Service) CreateAdjustmentDraftTx(ctx context.Context, tx *sql.Tx, tenantID, invoiceID string, grossCents int64, reason, description string) (domain.CreditNote, error) {
	return s.create(ctx, tenantID, CreateInput{
		InvoiceID: invoiceID,
		Reason:    reason,
		Lines: []CreditLineInput{{
			Description:     description,
			Quantity:        1,
			UnitAmountCents: grossCents,
		}},
		IssuePending: true,
	}, tx)
}

// LockInvoicesForDraftsTx takes the credit-note lock of every invoice the
// caller is about to write a draft on with CreateAdjustmentDraftTx, on the same
// transaction. Call it once, before the first draft, whenever one transaction
// credits more than one invoice: the first draft locks the tenant's number
// sequence to commit, and asking for another invoice's lock after that
// deadlocks with a concurrent credit note on that invoice.
func (s *Service) LockInvoicesForDraftsTx(ctx context.Context, tx *sql.Tx, tenantID string, invoiceIDs []string) error {
	return s.store.LockInvoicesTx(ctx, tx, tenantID, invoiceIDs)
}

// RetryPendingClawbackIssue re-issues auto-clawback drafts whose post-commit
// Issue() NEVER RAN — the note is still status='draft' AND issue_pending (the
// common case: a crash or transient error in the post-commit window before
// Issue() flipped the status). Cross-tenant + scoped to the ctx's livemode,
// mirroring RetryPendingTaxCommit. Because the scan requires status='draft',
// re-issue is safe by construction: nothing has applied yet, so Issue() runs
// fresh — no double-reverse, no double-credit. Once Issue() succeeds the note
// leaves status='draft' and drops out of the scan.
//
// No partial-INTERNAL-issue window to recover (ADR-061): Issue() is a coordinator
// tx — the draft→issued CAS AND the internal effect (balance credit / amount_due
// reduction) commit together, so a failed internal effect rolls the flip back and
// the note stays status='draft' for this scan to re-issue. The only post-commit
// leg is the EXTERNAL tax reversal, which on failure sets tax_reversal_pending and
// self-heals via RetryPendingCreditNoteTaxReversal — not something this sweep
// chases. Per-row errors are collected, not aborted-on.
func (s *Service) RetryPendingClawbackIssue(ctx context.Context, batch int) (int, []error) {
	livemode, err := postgres.Livemode(ctx)
	if err != nil {
		return 0, []error{err}
	}
	drafts, err := s.store.ListPendingClawbackDrafts(ctx, batch, livemode)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending clawback drafts: %w", err)}
	}
	var errsOut []error
	issued := 0
	for _, cn := range drafts {
		if _, err := s.Issue(ctx, cn.TenantID, cn.ID); err != nil {
			errsOut = append(errsOut, fmt.Errorf("re-issue clawback credit note %s: %w", cn.ID, err))
			continue
		}
		issued++
	}
	return issued, errsOut
}

// RetryPendingClawbackIssueForClock is the catchup counterpart to
// RetryPendingClawbackIssue (ADR-029 disjoint flows). The Advance path calls it
// per clock to issue that clock's DEFERRED simulated clawback drafts — the ones
// whose source invoice was in-flight when the downgrade ran (ADR-059 defer) and
// has since settled on a later advance. The wall-clock reconciler skips
// simulated rows (is_simulated=false), so without this a simulated clawback
// would either never complete or, pre-fix, get issued against wall-clock time.
//
// It binds the clock's frozen_time as effective-now, so every clock.Now(ctx)
// inside Issue()'s coordinator tx — issued_at, updated_at, the credit-ledger
// grant (paid source) or amount_due relief (unpaid), and the post-commit tax
// reversal — lands in SIMULATED time, mirroring the engine's in-band bind on the
// initial downgrade. Per-row errors are collected, not aborted-on.
func (s *Service) RetryPendingClawbackIssueForClock(ctx context.Context, tenantID, clockID string, frozenTime time.Time, batch int) (int, []error) {
	drafts, err := s.store.ListPendingClawbackDraftsForClock(ctx, tenantID, clockID, batch)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending clawback drafts for clock %s: %w", clockID, err)}
	}
	// WithSim: the clock id rides along with frozen_time, so the credit_note
	// / grant / tax rows Issue() emits inside its coordinator tx land on the
	// sim axis (ADR-090 §5) — this is the ONLY issuer for a simulated clawback,
	// so an unstamped row here would be invisible to the clock filter forever.
	ctx = clock.WithSim(ctx, clock.Sim{At: frozenTime, TestClockID: clockID})
	var errsOut []error
	issued := 0
	for _, cn := range drafts {
		if _, err := s.Issue(ctx, cn.TenantID, cn.ID); err != nil {
			errsOut = append(errsOut, fmt.Errorf("re-issue clawback credit note %s: %w", cn.ID, err))
			continue
		}
		issued++
	}
	return issued, errsOut
}

// RetryPendingCreditNoteTaxReversal re-drives the POST-COMMIT upstream tax
// reversal for issued credit notes whose inline attempt failed
// (tax_reversal_pending). It is the CN-path counterpart to the invoice #310
// RetryPendingTaxReversal: #310 scans voided/uncollectible invoices and keys off
// invoices.tax_reversed_at, so a CN reversal on a FINALIZED/PAID invoice (which
// stamps credit_notes.tax_transaction_id instead) is structurally invisible to
// it — leaving the tenant over-remitting until recovered. Cross-tenant + scoped
// to the ctx's livemode. Re-drive is Stripe-idempotent via the per-CN
// hardcount_tax_rev_<cn.ID> reference; on success the marker is cleared and the
// reversal transaction id stamped. Per-row errors are collected, not aborted-on.
func (s *Service) RetryPendingCreditNoteTaxReversal(ctx context.Context, batch int) (int, []error) {
	if s.taxRev == nil {
		return 0, nil
	}
	livemode, err := postgres.Livemode(ctx)
	if err != nil {
		return 0, []error{err}
	}
	pending, err := s.store.ListPendingCreditNoteTaxReversal(ctx, batch, livemode)
	if err != nil {
		return 0, []error{fmt.Errorf("list pending credit-note tax reversals: %w", err)}
	}
	var errsOut []error
	reversed := 0
	for _, cn := range pending {
		// Already reversed upstream (a prior sweep stamped the tx id but failed
		// to clear the marker): just clear it so it leaves the scan.
		if cn.TaxTransactionID != "" {
			if err := s.store.SetTaxReversalPending(ctx, cn.TenantID, cn.ID, false); err != nil {
				errsOut = append(errsOut, fmt.Errorf("clear tax_reversal_pending for %s: %w", cn.ID, err))
				continue
			}
			reversed++
			continue
		}
		inv, err := s.invoices.Get(ctx, cn.TenantID, cn.InvoiceID)
		if err != nil {
			errsOut = append(errsOut, fmt.Errorf("get invoice for pending tax reversal %s: %w", cn.ID, err))
			continue
		}
		// Nothing to reverse, in either of its two shapes: no upstream
		// transaction at all (provider changed / legacy), or a transaction
		// that carried ZERO tax — reversing it is a no-op that returns no
		// transaction id, so tax_transaction_id stays empty and the row
		// re-qualifies through the scan's structural arm on the very next
		// tick. Without the zero-tax half this re-drove a pointless Stripe
		// call every tick until the 24h window aged it out (~288 calls per
		// affected credit note) and reported advanced=1 the whole time.
		if inv.TaxTransactionID == "" || inv.TaxAmountCents == 0 {
			if cn.TaxReversalPending {
				if err := s.store.SetTaxReversalPending(ctx, cn.TenantID, cn.ID, false); err != nil {
					errsOut = append(errsOut, fmt.Errorf("clear stale tax_reversal_pending for %s: %w", cn.ID, err))
				}
			}
			continue
		}
		res, err := s.taxRev.ReverseTax(ctx, cn.TenantID, tax.ReversalRequest{
			OriginalTransactionID: inv.TaxTransactionID,
			CreditNoteID:          cn.ID,
			InvoiceID:             cn.InvoiceID,
			Mode:                  tax.ReversalModePartial,
			GrossAmountCents:      cn.TotalCents,
		})
		if err != nil {
			errsOut = append(errsOut, fmt.Errorf("re-drive tax reversal for credit note %s: %w", cn.ID, err))
			s.promoteOrphan(ctx, cn, &errsOut)
			continue
		}
		if res == nil || res.TransactionID == "" {
			// A tax-bearing invoice (the gate above already excluded the
			// no-transaction and zero-tax shapes) whose provider returned no
			// reversal transaction has NOT been reversed — typically the
			// engine resolving no provider for the tenant. Treat it as a
			// failure: never clear the marker, never count it, promote it so
			// it survives the 24h detection window (HA-13, 2026-08-30).
			errsOut = append(errsOut, fmt.Errorf("re-drive tax reversal for credit note %s: provider returned no reversal transaction", cn.ID))
			s.promoteOrphan(ctx, cn, &errsOut)
			continue
		}
		if err := s.store.SetTaxTransaction(ctx, cn.TenantID, cn.ID, res.TransactionID); err != nil {
			errsOut = append(errsOut, fmt.Errorf("persist reversal tx for %s: %w", cn.ID, err))
			continue
		}
		// Only write when the marker is actually set. This used to run
		// unconditionally, and rows reached here via the SECOND arm of
		// ListPendingCreditNoteTaxReversal's predicate — tax_transaction_id
		// empty, marker already false — which made it a no-op UPDATE that
		// still bumped updated_at. That defeated the 24h window the scan
		// relies on to age a row out: a zero-tax reversal returns no
		// transaction id, so tax_transaction_id stayed empty, the row
		// requalified next tick, and the sweep refreshed its own
		// eligibility. Observed live as advanced=1 on every tick forever
		// with zero rows actually pending, re-filing a reversal at Stripe
		// every 5 minutes (deduped there by the per-CN reference, so no
		// money moved) and leaving the metric unable to ever read healthy.
		if cn.TaxReversalPending {
			if err := s.store.SetTaxReversalPending(ctx, cn.TenantID, cn.ID, false); err != nil {
				errsOut = append(errsOut, fmt.Errorf("clear tax_reversal_pending for %s: %w", cn.ID, err))
				continue
			}
		}
		reversed++
	}
	return reversed, errsOut
}

// promoteOrphan moves a marker-less orphan (issued CN, no reversal stamped,
// marker false — the compound-failure shape where both the inline reversal
// and its marker write failed) onto the UNBOUNDED marker branch on its first
// failed re-drive. Without this, the structural branch's 24h freshness
// window bounded the OBLIGATION, not just detection: a >24h provider outage
// coinciding with the compound failure aged the row out and the tenant
// over-remitted forever (ADR-062 amendment 2026-07-06; the $46.69 class).
// Never rewrites a true marker — the real store bumps updated_at on every
// write, and a needless write would refresh the row's own window.
func (s *Service) promoteOrphan(ctx context.Context, cn domain.CreditNote, errsOut *[]error) {
	if cn.TaxReversalPending {
		return
	}
	if err := s.store.SetTaxReversalPending(ctx, cn.TenantID, cn.ID, true); err != nil {
		*errsOut = append(*errsOut, fmt.Errorf("promote marker-less orphan %s to tax_reversal_pending: %w", cn.ID, err))
	}
}

// buildCreditNote runs the tax-breakout, three-channel allocation, refund cap,
// and number generation, returning the credit note to insert. Called inside
// CreateUnderInvoiceLock with the lock-stable `existingCNs` snapshot; tx is
// that lock's transaction, which the number is allocated on.
func (s *Service) buildCreditNote(ctx context.Context, tx *sql.Tx, tenantID string, input CreateInput, inv domain.Invoice, subtotal, existingTotal, existingTaxReversed int64, existingCNs []domain.CreditNote) (domain.CreditNote, error) {
	// Break out proportional tax from the gross subtotal. The invoice's
	// tax_amount / total_amount ratio tells us what fraction of the gross
	// was tax; apply the same ratio to the CN's gross subtotal so a
	// partial credit reverses the same fraction of tax the invoice
	// originally collected. Zero-tax invoices (tax_amount==0 or no
	// provider) produce zero tax on the CN, preserving legacy behaviour.
	//
	// Cumulative residual true-up (2026-05-25). This is a SEQUENTIAL/temporal
	// reconciliation, distinct from the per-line apportionment in ADR-046:
	// integer-division proportional tax accumulates floor() residuals across
	// multiple CNs issued over time, so the sum of per-CN tax can fall short
	// of the invoice tax by up to (N−1) cents. Because the CNs are issued one
	// at a time (not distributed across peer lines at once), the correct fix
	// is a true-up on the LAST event: the CN that EXHAUSTS the invoice
	// (cumulative CN total reaches the invoice total) absorbs the residual so
	// the cumulative tax reversed equals the original tax exactly. Non-
	// exhausting CNs use the pure proportional formula. The persisted
	// `tax_amount_cents` on the CN drives both the dashboard display and the
	// upstream Stripe Tax reversal Reference shape, so both surfaces converge
	// on the same total.
	var taxAmount, netSubtotal int64
	netSubtotal = subtotal
	if inv.TotalAmountCents > 0 && inv.TaxAmountCents > 0 {
		if subtotal == inv.TotalAmountCents-existingTotal {
			// This CN exhausts the invoice — absorb the residual.
			taxAmount = inv.TaxAmountCents - existingTaxReversed
		} else {
			// tax × subtotal can exceed int64 on a large invoice; big.Int
			// division truncates exactly as int64 "/" did, without the wrap.
			taxAmount = new(big.Int).Quo(
				new(big.Int).Mul(big.NewInt(inv.TaxAmountCents), big.NewInt(subtotal)),
				big.NewInt(inv.TotalAmountCents),
			).Int64()
		}
		netSubtotal = subtotal - taxAmount
	}

	// Three-channel allocation (Stripe + Lago shape, verified
	// 2026-05-24). The operator allocates the CN total across:
	//   refund_amount_cents     → Stripe refund to the original PM
	//   credit_amount_cents     → restored to customer's credit balance
	//   out_of_band_amount_cents → handled externally (cash, ACH,
	//                              manual adjustment); audit-trail only
	// The three values must sum to subtotal. New integrations should
	// set the explicit fields. The legacy RefundType ("refund" |
	// "credit") is honored when the new fields are all zero — the
	// equivalent single-channel allocation is filled in.
	//
	// Refund cap: refund_amount_cents ≤ AmountPaidCents − already
	// refunded by prior CNs. Hardcount cannot push more cash back to
	// the PM than the customer actually paid via card.
	//
	// Default when paid invoice + all three zero + no legacy
	// RefundType: full credit to balance (safest — no cash
	// movement, restorable later).
	var refundAmount, creditAmount, outOfBandAmount int64
	if inv.Status == domain.InvoicePaid {
		refundAmount = input.RefundAmountCents
		creditAmount = input.CreditAmountCents
		outOfBandAmount = input.OutOfBandAmountCents

		// Legacy compatibility: if the caller used the old
		// RefundType field and left all three explicit amounts
		// zero, translate into the equivalent single-channel
		// allocation. Removes the field's surface area without
		// breaking existing SDK callers mid-flight.
		if refundAmount == 0 && creditAmount == 0 && outOfBandAmount == 0 {
			switch input.RefundType {
			case "refund":
				refundAmount = subtotal
			case "credit", "":
				creditAmount = subtotal
			}
		}

		if refundAmount < 0 || creditAmount < 0 || outOfBandAmount < 0 {
			return domain.CreditNote{}, errs.Invalid("allocation", "refund_amount_cents, credit_amount_cents, and out_of_band_amount_cents must all be ≥ 0")
		}
		// The three are each >= 0 (checked above); their sum is checked too.
		allocated, aerr := domain.SumCents(refundAmount, creditAmount)
		if aerr == nil {
			allocated, aerr = domain.SumCents(allocated, outOfBandAmount)
		}
		if aerr != nil {
			return domain.CreditNote{}, errs.Invalid("allocation", fmt.Sprintf(
				"refund_amount_cents + credit_amount_cents + out_of_band_amount_cents must equal credit note total %.2f",
				float64(subtotal)/100))
		}
		if allocated != subtotal {
			return domain.CreditNote{}, errs.Invalid("allocation", fmt.Sprintf(
				"refund_amount_cents (%.2f) + credit_amount_cents (%.2f) + out_of_band_amount_cents (%.2f) = %.2f, must equal credit note total %.2f",
				float64(refundAmount)/100, float64(creditAmount)/100, float64(outOfBandAmount)/100,
				float64(allocated)/100, float64(subtotal)/100,
			))
		}

		// Refund cap: cannot push more cash to PM than was paid
		// via PM (less any prior refunds). Matches Stripe — its
		// `refund_amount` validation rejects over-refund with
		// `amount_too_large`.
		var existingRefunds int64
		for _, cn := range existingCNs {
			if cn.Status == domain.CreditNoteVoided {
				continue
			}
			var err error
			if existingRefunds, err = domain.SumCents(existingRefunds, cn.RefundAmountCents); err != nil {
				return domain.CreditNote{}, fmt.Errorf("credit notes on invoice %s: refunds: %w", input.InvoiceID, err)
			}
		}
		pmRefundable := inv.CardRefundableCents(existingRefunds)
		if refundAmount > pmRefundable {
			// With no card behind the payment (offline, credits) the over-cap
			// wording ("exceeds 0.00 paid via card") would misdescribe the
			// cause. Name the rail instead — the operator's fix is to
			// re-route the money, not to lower it.
			if !inv.HasCardPayment() {
				return domain.CreditNote{}, errs.Invalid("refund_amount_cents", fmt.Sprintf(
					"this invoice was not paid by card, so there is no card to refund (%.2f requested) — route the amount to credit_amount_cents or out_of_band_amount_cents.",
					float64(refundAmount)/100,
				))
			}
			return domain.CreditNote{}, errs.Invalid("refund_amount_cents", fmt.Sprintf(
				"refund amount (%.2f) exceeds payment-method refundable (%.2f) — %.2f paid via card, %.2f already refunded by prior credit notes. Reduce refund_amount_cents and route the rest to credit_amount_cents or out_of_band_amount_cents.",
				float64(refundAmount)/100,
				float64(pmRefundable)/100,
				float64(inv.AmountPaidCents)/100,
				float64(existingRefunds)/100,
			))
		}
	}

	// Credit-note numbers are a strictly monotonic per-tenant sequence, same
	// contract as invoice numbers. No fallback: the previous synthesized
	// CN-YYYYMM-<unixnano%1e6> number was non-monotonic and collision-prone,
	// and swallowing the allocator error hid the misconfiguration — a
	// duplicate document number corrupts accounting downstream, while a
	// failed Create just retries.
	if s.numbers == nil {
		return domain.CreditNote{}, fmt.Errorf("credit-note number generator not wired (call SetNumberGenerator)")
	}
	cnNumber, err := s.numbers.NextCreditNoteNumberTx(ctx, tx, tenantID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("allocate credit note number: %w", err)
	}
	if cnNumber == "" {
		return domain.CreditNote{}, fmt.Errorf("credit-note number generator returned an empty number")
	}

	return domain.CreditNote{
		InvoiceID:            input.InvoiceID,
		CustomerID:           inv.CustomerID,
		CreditNoteNumber:     cnNumber,
		Status:               domain.CreditNoteDraft,
		Reason:               strings.TrimSpace(input.Reason),
		SubtotalCents:        netSubtotal,
		TaxAmountCents:       taxAmount,
		TotalCents:           subtotal,
		RefundAmountCents:    refundAmount,
		CreditAmountCents:    creditAmount,
		OutOfBandAmountCents: outOfBandAmount,
		Currency:             inv.Currency,
		RefundStatus:         domain.RefundNone,
		// Simulated iff the SOURCE INVOICE is — the clock belongs to the
		// entity, not to whoever acted on it. This used to also require
		// input.IsSimulated, i.e. "operator HTTP issuance is always
		// wall-clock", which contradicted ADR-030: its only always-wall-clock
		// carve-out is the audit log, and its table names credit notes and
		// their CreditGrant rows as simulated-when-pinned.
		//
		// The old rule produced a row stamped in simulated time (once the
		// entry points bound ctx) but flagged is_simulated=false — so it
		// rendered in the "Real times — not the test clock's dates" lane while
		// displaying the test clock's date, stayed visible to wall-clock
		// reconcilers that skip simulated rows, and would survive the ADR-086
		// teardown that deletes a clock's data.
		IsSimulated: inv.IsSimulated,
		// IssuePending: an auto-issue clawback draft to be issued post-commit and
		// retried by the reconciler if Issue() fails (migration 0121). Only the
		// in-tx clawback create (CreateAdjustmentDraftTx) sets this.
		IssuePending: input.IssuePending,
	}, nil
}

// CommitReliefInput is the operator request for a paid-commit refund
// (ADR-080). Retirement is denominated in CREDITS; the cash is always
// server-derived from the telescoping anchor — never operator-supplied.
type CommitReliefInput struct {
	InvoiceID string `json:"invoice_id"`
	// RetireCents: how many commit credits to retire (1..remaining).
	// Ignored when RetireAll — which resolves to the LIVE remaining inside
	// the tx, so a racing drawdown shrinks the relief instead of failing it.
	RetireCents int64  `json:"retire_cents"`
	RetireAll   bool   `json:"retire_all"`
	Reason      string `json:"reason"`
	// Allocation across the two permitted channels. nil = default: full
	// refund when the invoice has a PaymentIntent, else out_of_band
	// (offline-paid commits — the migration-month norm). The credit
	// channel is structurally forbidden: paying relief out of the
	// customer's balance would refund the very block being retired.
	RefundAmountCents    *int64 `json:"refund_amount_cents,omitempty"`
	OutOfBandAmountCents *int64 `json:"out_of_band_amount_cents,omitempty"`
}

// commitReliefCash is the telescoping anchor (ADR-080, panel-settled):
// f(k) = RoundHalfToEven(grossPaid × k, granted). A relief retiring r
// credits refunds f(K+r) − f(K), so ANY partial sequence sums to exactly
// f(K_final) ≤ f(granted) = grossPaid — per-slice independent rounding
// would over-refund (grossPaid=2, granted=3, three 1-credit slices: naive
// pays 3 on 2 collected; telescoping pays exactly 2).
//
// THE ANCHOR HAS EXACTLY ONE CALL SITE COMPUTING CN CASH — a second
// computation site reintroduces the per-slice rounding bug. Keep it here.
func commitReliefCash(grossPaid, granted, k, r int64) int64 {
	f := func(x int64) int64 {
		if x <= 0 {
			return 0
		}
		if x >= granted {
			return grossPaid
		}
		// 0 < x < granted, so the quotient is below grossPaid and fits;
		// the product is worked in 128 bits.
		v, _ := money.MulDivRoundHalfToEven(grossPaid, x, granted)
		return v
	}
	return f(k+r) - f(k)
}

// CreateAndIssueCommitRelief refunds a PAID commit invoice by retiring
// commit credits (ADR-080 — the ADR-078 phase-2 relief leg). ONE
// coordinator tx creates the credit note, flips it issued, and retires the
// credits: a crash pre-commit leaves nothing; post-commit only the
// external legs (Stripe refund / tax reversal) are outstanding and both
// self-heal — credits are always retired at-or-before cash leaves, never
// refunded-but-drawable. Deliberately create-and-issue (no draft window):
// operator-initiated, idempotency-keyed at the HTTP layer, and a draft
// would neither reserve the cap nor survive Issue()'s credit-channel
// re-derive default — the exact hazards the panel rejected.
func (s *Service) CreateAndIssueCommitRelief(ctx context.Context, tenantID string, input CommitReliefInput) (domain.CreditNote, error) {
	if input.InvoiceID == "" {
		return domain.CreditNote{}, errs.Required("invoice_id")
	}
	if !input.RetireAll && input.RetireCents <= 0 {
		return domain.CreditNote{}, errs.Invalid("retire_cents", "must be a positive number of credits, or set retire_all")
	}
	if s.numbers == nil {
		return domain.CreditNote{}, fmt.Errorf("credit-note number generator not wired (call SetNumberGenerator)")
	}
	if s.credits == nil {
		return domain.CreditNote{}, fmt.Errorf("credit granter not wired (pass it to NewService)")
	}

	// ADR-030: relief rows belong to the source invoice's time domain.
	ctx = s.bindForInvoice(ctx, tenantID, input.InvoiceID)

	inv, err := s.invoices.Get(ctx, tenantID, input.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, err
	}

	// Shape gate first: a relief request against a non-commit invoice is
	// wrong regardless of status — route to the regular CN flow.
	invLines, err := s.invoices.ListLineItems(ctx, tenantID, input.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("list invoice line items: %w", err)
	}
	commitLines := 0
	for _, li := range invLines {
		if li.IsCommitLine() {
			commitLines++
		}
	}
	if commitLines == 0 {
		return domain.CreditNote{}, errs.Invalid("invoice_id", "not a commit invoice — use a regular credit note")
	}
	if commitLines != len(invLines) || commitLines != 1 {
		// Mixed commit+other invoices: relief semantics for the non-commit
		// share are undefined in phase 2 — gate loudly. Trigger: first DP
		// mixed-invoice refund ask.
		return domain.CreditNote{}, errs.InvalidState("commit relief supports single-line commit invoices only — this invoice mixes commit and non-commit lines")
	}

	// Status gates. Paid is terminal for commit invoices (void-of-paid is
	// blocked), so these pre-tx reads cannot regress under us; the money
	// inputs are frozen later by the grant row lock.
	switch {
	case inv.Status == domain.InvoiceVoided || inv.Status == domain.InvoiceUncollectible:
		return domain.CreditNote{}, errs.InvalidState("cannot relieve a voided or uncollectible commit invoice")
	case domain.PaymentBlocksAction(inv, domain.ActionIssueCreditNote).Blocked:
		// Same rule, same source — this one told the operator to "cancel it",
		// which has never been an action the product offers.
		return domain.CreditNote{}, errs.InvalidState(domain.PaymentBlocksAction(inv, domain.ActionIssueCreditNote).Message).
			WithCode(domain.PaymentBlocksAction(inv, domain.ActionIssueCreditNote).Code)
	case inv.Status != domain.InvoicePaid || inv.PaymentStatus != domain.PaymentSucceeded:
		return domain.CreditNote{}, errs.InvalidState("commit relief applies to PAID commit invoices — void the unpaid invoice to cancel the commit instead")
	}

	grossPaid := inv.TotalAmountCents
	if grossPaid <= 0 {
		return domain.CreditNote{}, errs.InvalidState("commit invoice has a zero total — nothing to relieve")
	}

	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("begin commit relief tx: %w", err)
	}
	defer postgres.Rollback(tx)

	// Captured by the build closure for the post-insert retire.
	var retireSlice, reliefCash int64

	cn, err := s.store.CreateUnderInvoiceLockDynamicTx(ctx, tx, tenantID, input.InvoiceID, func(existing []domain.CreditNote) (domain.CreditNote, []domain.CreditNoteLineItem, error) {
		// Grant lock AFTER the per-invoice CN lock — the ADR-080 order
		// (invoice-CN → customer advisory → ledger rows). G, C, K are
		// frozen from here to commit.
		granted, consumed, k, found, gerr := s.credits.LockCommitGrantForReliefTx(ctx, tx, tenantID, input.InvoiceID)
		if gerr != nil {
			return domain.CreditNote{}, nil, gerr
		}
		if !found {
			return domain.CreditNote{}, nil, fmt.Errorf("invoice %s has a commit line but no funded grant — finalize funds the grant in the same tx, so this is corrupted state; refusing to move cash", input.InvoiceID)
		}
		remaining := granted - consumed
		if remaining <= 0 {
			return domain.CreditNote{}, nil, errs.InvalidState("commit fully consumed (or lapsed per its term) — nothing refundable")
		}
		r := input.RetireCents
		if input.RetireAll {
			r = remaining
		}
		if r > remaining {
			return domain.CreditNote{}, nil, errs.Invalid("retire_cents", fmt.Sprintf(
				"%d credits requested but only %d remain on the commit (max refundable now: %.2f) — a drawdown may have landed since you looked; re-check and retry",
				r, remaining, float64(commitReliefCash(grossPaid, granted, k, remaining))/100))
		}

		x := commitReliefCash(grossPaid, granted, k, r)
		if x == 0 {
			// Deep-discount degenerate slice: the credits round to zero
			// cash. A zero-total CN would silently donate the credits —
			// refuse; retiring a bigger slice loses nothing (telescoping).
			return domain.CreditNote{}, nil, errs.Invalid("retire_cents", "this slice rounds to zero refundable cash — retire more credits in one relief")
		}

		// Document caps vs existing CNs. Ordinary CNs are blocked on commit
		// invoices, so `existing` are prior relief CNs; the telescoping
		// anchor guarantees Σ totals ≤ grossPaid — keep the check as a
		// defensive invariant, not a reachable branch.
		var existingTotal, existingTaxReversed, existingRefunds int64
		for _, e := range existing {
			if e.Status == domain.CreditNoteVoided {
				continue
			}
			var serr error
			if existingTotal, serr = domain.SumCents(existingTotal, e.TotalCents); serr == nil {
				if existingTaxReversed, serr = domain.SumCents(existingTaxReversed, e.TaxAmountCents); serr == nil {
					existingRefunds, serr = domain.SumCents(existingRefunds, e.RefundAmountCents)
				}
			}
			if serr != nil {
				return domain.CreditNote{}, nil, fmt.Errorf("credit notes on invoice %s: %w", input.InvoiceID, serr)
			}
		}
		if x > inv.TotalAmountCents-existingTotal { // never existingTotal+x: that sum can wrap
			return domain.CreditNote{}, nil, fmt.Errorf("commit relief invariant violated: existing CN total %d + relief %d exceeds invoice total %d", existingTotal, x, inv.TotalAmountCents)
		}

		// Proportional tax — same discipline as buildCreditNote: floor
		// ratio per CN, and the EXHAUSTING relief absorbs the cumulative
		// residual so Σ reversed tax equals the invoice tax exactly.
		var taxAmount int64
		netSubtotal := x
		if inv.TotalAmountCents > 0 && inv.TaxAmountCents > 0 {
			if x == inv.TotalAmountCents-existingTotal {
				taxAmount = inv.TaxAmountCents - existingTaxReversed
			} else {
				// tax × x is worked in 128 bits; the quotient truncates
				// exactly as int64 "/" did, and is below the tax (x < total).
				q, _, ok := money.MulDivRem(inv.TaxAmountCents, x, inv.TotalAmountCents)
				if !ok {
					return domain.CreditNote{}, nil, fmt.Errorf("commit relief tax share: %w", domain.ErrAmountOverflow)
				}
				taxAmount = q
			}
			netSubtotal = x - taxAmount
		}

		// Allocation: the two permitted channels only (requirement f).
		var refundAmount, outOfBand int64
		// An offline-paid commit (RecordOfflinePayment) has no PaymentIntent:
		// there is no card to refund behind it.
		if input.RefundAmountCents == nil && input.OutOfBandAmountCents == nil {
			if inv.HasCardPayment() {
				refundAmount = x
			} else {
				outOfBand = x // offline-paid commit: the CN records the obligation; cash moves outside
			}
		} else {
			if input.RefundAmountCents != nil {
				refundAmount = *input.RefundAmountCents
			}
			if input.OutOfBandAmountCents != nil {
				outOfBand = *input.OutOfBandAmountCents
			}
			if refundAmount < 0 || outOfBand < 0 {
				return domain.CreditNote{}, nil, errs.Invalid("allocation", "refund_amount_cents and out_of_band_amount_cents must be ≥ 0")
			}
			allocated, aerr := domain.SumCents(refundAmount, outOfBand)
			if aerr != nil {
				return domain.CreditNote{}, nil, errs.Invalid("allocation", fmt.Sprintf(
					"allocation must sum to the derived relief amount %.2f", float64(x)/100))
			}
			if allocated != x {
				return domain.CreditNote{}, nil, errs.Invalid("allocation", fmt.Sprintf(
					"allocation must sum to the derived relief amount %.2f (refund %.2f + out_of_band %.2f = %.2f)",
					float64(x)/100, float64(refundAmount)/100, float64(outOfBand)/100, float64(allocated)/100))
			}
		}
		// PM refund cap: never push more cash to the card than was paid
		// minus prior refunds — an out-of-band Stripe-dashboard refund may
		// have drained the PI. Zero when no card backs the payment at all, so
		// an EXPLICIT refund allocation on an offline-paid invoice is refused
		// here rather than reaching Stripe with no PaymentIntent.
		pmRefundable := inv.CardRefundableCents(existingRefunds)
		if refundAmount > pmRefundable {
			if !inv.HasCardPayment() {
				return domain.CreditNote{}, nil, errs.Invalid("refund_amount_cents", fmt.Sprintf(
					"this invoice was not paid by card, so there is no card to refund (%.2f requested) — route the amount to out_of_band_amount_cents.",
					float64(refundAmount)/100))
			}
			return domain.CreditNote{}, nil, errs.Invalid("refund_amount_cents", fmt.Sprintf(
				"refund amount (%.2f) exceeds payment-method refundable (%.2f) — route the rest to out_of_band_amount_cents",
				float64(refundAmount)/100, float64(pmRefundable)/100))
		}

		cnNumber, nerr := s.numbers.NextCreditNoteNumberTx(ctx, tx, tenantID)
		if nerr != nil {
			return domain.CreditNote{}, nil, fmt.Errorf("allocate credit note number: %w", nerr)
		}
		if cnNumber == "" {
			return domain.CreditNote{}, nil, fmt.Errorf("credit-note number generator returned an empty number")
		}

		reason := strings.TrimSpace(input.Reason)
		if reason == "" {
			reason = "commit_relief"
		}
		retireSlice, reliefCash = r, x

		header := domain.CreditNote{
			InvoiceID:            input.InvoiceID,
			CustomerID:           inv.CustomerID,
			CreditNoteNumber:     cnNumber,
			Status:               domain.CreditNoteDraft, // flipped issued below, same tx
			Reason:               reason,
			SubtotalCents:        netSubtotal,
			TaxAmountCents:       taxAmount,
			TotalCents:           x,
			RefundAmountCents:    refundAmount,
			CreditAmountCents:    0, // structurally forbidden channel
			OutOfBandAmountCents: outOfBand,
			CommitRetiredCents:   r,
			Currency:             inv.Currency,
			RefundStatus:         domain.RefundNone,
			// Same rule as buildCreditNote: the source invoice's pin decides.
			// This path never set the flag at all, so a commit-relief note on a
			// clock-pinned invoice was left unflagged.
			IsSimulated: inv.IsSimulated,
		}
		line := domain.CreditNoteLineItem{
			Description:     fmt.Sprintf("Commit relief — retired %d of %d commit credits", r, granted),
			Quantity:        1,
			UnitAmountCents: x,
			AmountCents:     x,
		}
		return header, []domain.CreditNoteLineItem{line}, nil
	})
	if err != nil {
		return domain.CreditNote{}, err
	}

	// Flip issued on the SAME tx — trivially won on a fresh row, but kept
	// as the single issued-state gate (and it stamps issued_at).
	won, err := s.store.TransitionStatusTx(ctx, tx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("claim relief issue transition: %w", err)
	}
	if !won {
		return domain.CreditNote{}, fmt.Errorf("commit relief: freshly-created draft lost its issue CAS — concurrent writer on a row created this tx (impossible; investigate)")
	}

	// Retire the credits IN the tx (the internal money effect). The grant
	// row is already locked by this tx, so the re-read is consistent.
	if _, err := s.credits.RetireCommitSliceForReliefTx(ctx, tx, tenantID, input.InvoiceID, cn.ID, retireSlice, reliefCash, grossPaid); err != nil {
		return domain.CreditNote{}, fmt.Errorf("retire commit slice: %w", err)
	}

	// ADR-090 in-tx emission — mirrors Issue()'s row so relief CNs are as
	// visible as ordinary issues.
	if s.audit != nil {
		if auditErr := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        "credit_note.issued",
			ResourceType:  "credit_note",
			ResourceID:    cn.ID,
			ResourceLabel: cn.CreditNoteNumber,
			Metadata: map[string]any{
				"credit_note_number":       cn.CreditNoteNumber,
				"invoice_id":               cn.InvoiceID,
				"customer_id":              cn.CustomerID,
				"total_cents":              cn.TotalCents,
				"refund_amount_cents":      cn.RefundAmountCents,
				"credit_amount_cents":      cn.CreditAmountCents,
				"out_of_band_amount_cents": cn.OutOfBandAmountCents,
				"reason":                   cn.Reason,
				"currency":                 cn.Currency,
			},
		}); auditErr != nil {
			return domain.CreditNote{}, fmt.Errorf("audit emission: %w", auditErr)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.CreditNote{}, fmt.Errorf("commit relief tx: %w", err)
	}

	// External legs (Stripe refund / tax reversal) — shared with Issue(),
	// idempotency-keyed and self-healing.
	return s.runPostIssueExternalLegs(ctx, tenantID, cn.ID, cn, inv)
}

// RefundInput describes a direct refund against a paid invoice. It bypasses
// the usual line-item ceremony by synthesizing a single-line credit note from
// the caller's amount + reason. Used by the invoice /refund endpoint (FEAT-2).
type RefundInput struct {
	InvoiceID   string
	AmountCents int64 // 0 = refund the full remaining refundable amount
	Reason      string
	Description string // optional line-item description; defaults based on reason
}

// CreateRefund creates and issues a refund credit note for a paid invoice in
// one call. This is the "direct refund" entry point exposed at
// POST /invoices/{id}/refund — operators who just want to issue a refund don't
// need to know about credit notes as a data model.
//
// Delegates all validation (invoice must be paid, refund must not exceed paid
// amount, must not exceed invoice total) to Service.Create by shaping a
// synthetic CreateInput. On Issue failure, voids the draft credit note so the
// caller sees a clean rollback rather than an orphan draft.
func (s *Service) CreateRefund(ctx context.Context, tenantID string, input RefundInput) (domain.CreditNote, error) {
	if input.InvoiceID == "" {
		return domain.CreditNote{}, errs.Required("invoice_id")
	}
	if strings.TrimSpace(input.Reason) == "" {
		return domain.CreditNote{}, errs.Required("reason")
	}

	// ADR-030: refund rows belong to the source invoice's time domain.
	ctx = s.bindForInvoice(ctx, tenantID, input.InvoiceID)

	inv, err := s.invoices.Get(ctx, tenantID, input.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("invoice not found: %w", err)
	}
	if inv.Status != domain.InvoicePaid {
		return domain.CreditNote{}, errs.InvalidState("can only refund paid invoices")
	}
	// No card behind the payment: say which kind of payment it was.
	if !inv.HasCardPayment() {
		if !inv.PaidOutOfBand {
			return domain.CreditNote{}, errs.InvalidState("invoice has no associated payment to refund")
		}
		return domain.CreditNote{}, errs.InvalidState("invoice was paid outside Stripe — there is no card to refund; issue a credit note allocated to out_of_band_amount_cents instead")
	}

	// No amount: refund the most Create's two caps allow, what was paid by
	// card less the refunds on notes not voided (a failed refund counts: its
	// note keeps the amount for retry-refund), but no more than the invoice
	// can still be credited (a note to the customer's balance or settled
	// outside Stripe uses up part of the invoice too). Create checks both
	// again under the invoice lock.
	amount := input.AmountCents
	if amount == 0 {
		sums, err := s.store.InvoiceSums(ctx, tenantID, input.InvoiceID)
		if err != nil {
			return domain.CreditNote{}, err
		}
		refundable := inv.CardRefundableCents(sums.RefundCents)
		if refundable <= 0 {
			return domain.CreditNote{}, errs.InvalidState("invoice has already been fully refunded")
		}
		creditable := inv.TotalAmountCents - sums.CreditedCents
		if creditable <= 0 {
			return domain.CreditNote{}, errs.InvalidState("invoice has already been fully credited")
		}
		amount = min(refundable, creditable)
	}
	if amount <= 0 {
		return domain.CreditNote{}, errs.Invalid("amount_cents", "must be greater than 0")
	}

	description := strings.TrimSpace(input.Description)
	if description == "" {
		description = "Refund: " + strings.TrimSpace(input.Reason)
	}

	cn, err := s.Create(ctx, tenantID, CreateInput{
		InvoiceID:         input.InvoiceID,
		Reason:            input.Reason,
		RefundAmountCents: amount,
		Lines: []CreditLineInput{{
			Description:     description,
			Quantity:        1,
			UnitAmountCents: amount,
		}},
	})
	if err != nil {
		return domain.CreditNote{}, err
	}

	issued, err := s.Issue(ctx, tenantID, cn.ID)
	if err != nil {
		// Issue failed after Create succeeded — void the draft so we don't
		// leave an unusable record. Best-effort: Void deliberately refuses
		// when the Stripe refund leg already executed (that cash must stay
		// counted in the over-refund cap), and any other void failure is
		// non-actionable here. Either way, surface the original issue error.
		_, _ = s.Void(ctx, tenantID, cn.ID)
		return domain.CreditNote{}, err
	}
	return issued, nil
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	return s.store.Get(ctx, tenantID, id)
}

// GetWithLineItems fetches a credit note and its line items in one call.
// Used by the PDF download handler and any UI view that needs both.
func (s *Service) GetWithLineItems(ctx context.Context, tenantID, id string) (domain.CreditNote, []domain.CreditNoteLineItem, error) {
	cn, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.CreditNote{}, nil, err
	}
	items, err := s.store.ListLineItems(ctx, tenantID, id)
	if err != nil {
		return domain.CreditNote{}, nil, err
	}
	return cn, items, nil
}

func (s *Service) List(ctx context.Context, filter ListFilter) ([]domain.CreditNote, error) {
	return s.store.List(ctx, filter)
}

// ListIssuedForInvoice returns every issued credit note on the invoice,
// newest first, with no page limit, for the invoice PDF.
func (s *Service) ListIssuedForInvoice(ctx context.Context, tenantID, invoiceID string) ([]domain.CreditNote, error) {
	return s.store.ListIssuedByInvoice(ctx, tenantID, invoiceID)
}

// Count counts the credit notes the filter matches, ignoring its limit and
// offset.
func (s *Service) Count(ctx context.Context, filter ListFilter) (int, error) {
	return s.store.Count(ctx, filter)
}

// Stats counts every credit note by status and sums the issued ones per
// currency.
func (s *Service) Stats(ctx context.Context, tenantID string) (Stats, error) {
	return s.store.Stats(ctx, tenantID)
}

// UnreliefedClawbackCents sums the credit notes on the invoice that were
// voided before they were issued: relief the invoice was owed and never got.
// Issue() is what applies relief, so such a note never reduced amount_due, and
// the invoice asks for more than is owed by exactly this sum.
//
// Since 2026-10-23 a write-off no longer voids a relief draft (it issues on
// the written-off invoice, ADR-050 amendment), so on a written-off invoice
// this is a draft an operator voided before it issued. The invoice read and an
// offline payment on a written-off invoice warn with it
// (domain.RecoveryWarnsOnOfflinePayment).
//
// The test is the status and issued_at, not issue_pending: the status
// transition clears issue_pending in the same statement
// (creditnote/postgres.go), so no voided row still holds it.
func (s *Service) UnreliefedClawbackCents(ctx context.Context, tenantID, invoiceID string) (int64, error) {
	sums, err := s.store.InvoiceSums(ctx, tenantID, invoiceID)
	if err != nil {
		return 0, err
	}
	return sums.UnrelievedCents, nil
}

// CreditedCents returns the total of every credit note on the invoice that is
// not voided, drafts included — i.e. how much of the invoice has been credited. The
// remaining creditable headroom is inv.TotalAmountCents - CreditedCents. The
// billing engine's headroom-aware multi-invoice credit fan-out uses it so a
// share never overruns an invoice whose creditable amount a prior credit note
// (e.g. an earlier downgrade clawback) already consumed.
func (s *Service) CreditedCents(ctx context.Context, tenantID, invoiceID string) (int64, error) {
	sums, err := s.store.InvoiceSums(ctx, tenantID, invoiceID)
	if err != nil {
		return 0, err
	}
	return sums.CreditedCents, nil
}

// IssuedCreditedCents sums the ISSUED credit notes against an invoice — the
// ones whose tax reversal is filed under their own reference. A void reverses
// the rest of the invoice's tax, so a still-pending draft must not be counted:
// it reverses nothing and is voided with the invoice (the orphan guard in
// Issue). Counting it under-reversed the tenant's output tax by its share.
func (s *Service) IssuedCreditedCents(ctx context.Context, tenantID, invoiceID string) (int64, error) {
	sums, err := s.store.InvoiceSums(ctx, tenantID, invoiceID)
	if err != nil {
		return 0, err
	}
	return sums.IssuedCents, nil
}

func (s *Service) Issue(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	cn, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if cn.Status != domain.CreditNoteDraft {
		return domain.CreditNote{}, errs.InvalidState("can only issue draft credit notes")
	}
	// ADR-030: Issue is where the money moves (ledger grant / amount_due
	// relief / tax reversal), so it must run in the source invoice's time
	// domain. Bound here rather than at the top because the invoice id comes
	// off the credit note we just loaded.
	ctx = s.bindForInvoice(ctx, tenantID, cn.InvoiceID)

	// A first read of the source invoice, outside any transaction. It only
	// spares a transaction when the note plainly cannot issue yet; the row
	// locked inside the coordinator tx below is what every decision is made
	// from.
	inv, err := s.invoices.Get(ctx, tenantID, cn.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("get invoice: %w", err)
	}
	if issueHeld(inv) {
		return s.holdIssue(ctx, tenantID, cn, inv)
	}

	// Coordinator transaction (ADR-056 / ADR-061). The draft→issued CAS and the
	// INTERNAL money effect — amount_due reduction (unpaid) or credit grant
	// (paid) — commit ATOMICALLY on this one tx:
	//   - crash mid-tx  → both roll back; the note stays 'draft' and the
	//     reconciler (RetryPendingClawbackIssue) re-drives cleanly.
	//   - crash post-commit → the note is 'issued' AND the effect is applied
	//     together; a re-entry loses the CAS (won=false) and never re-applies.
	// So the reduction/grant is idempotent BY CONSTRUCTION — there is exactly
	// ONE caller of each (here), gated by the CAS, so a second application is
	// impossible without a second CAS win, which cannot happen. (This is why
	// PR2 carries NO source-dedup row; ADR-061 records it as the deferred first
	// brick of the amount_due-derived-ledger. A reviewer adding a second caller
	// of ApplyCreditNote/GrantForCreditNoteTx MUST reintroduce the dedup.)
	// External effects (Stripe refund, upstream tax reversal) cannot share a DB
	// tx, so they run POST-commit, idempotency-keyed and recoverable.
	tx, err := s.store.BeginTx(ctx, tenantID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("begin issue tx: %w", err)
	}
	defer postgres.Rollback(tx)

	// Lock the invoice row first (ADR-078 order: invoice row, then the customer
	// advisory lock and ledger rows the grant below takes) and decide from it.
	// Between the read above and this lock a payment may have settled, credits
	// may have been applied, or the invoice may have been voided; each changes
	// which branch is right or how much is still due.
	inv, err = s.invoices.LockForCreditNoteTx(ctx, tx, tenantID, cn.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("lock invoice: %w", err)
	}
	if issueHeld(inv) {
		// Release the row before the hold's own write (the orphan void).
		postgres.Rollback(tx)
		return s.holdIssue(ctx, tenantID, cn, inv)
	}

	won, err := s.store.TransitionStatusTx(ctx, tx, tenantID, id, domain.CreditNoteDraft, domain.CreditNoteIssued)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("claim issue transition: %w", err)
	}
	if !won {
		// A concurrent/retried Issue() already claimed the transition. Return
		// the current credit note rather than re-applying side effects.
		return s.store.Get(ctx, tenantID, id)
	}

	// What the note did to an unpaid invoice, for the audit row below.
	var dueReduced, creditsReturned int64
	// Whether the note left nothing due and so marked the invoice paid.
	var settled bool
	if inv.PaymentStatus == domain.PaymentSucceeded {
		// Re-derive the three-channel allocation from the CURRENT invoice
		// state. The allocation persisted at Create is frozen against the
		// invoice status AT THAT TIME: a CN created while the invoice was
		// still unpaid (InvoiceFinalized) skips Create's paid-invoice
		// block entirely, so all three channels land at zero. If that
		// invoice is then paid BEFORE the CN is issued, this Issue path
		// sees a paid invoice but a CN with zero allocations — neither the
		// refund leg nor the credit-grant leg fires, and the refund/credit
		// silently vanishes (the CN issues as a no-op). Mirror Create's
		// paid-invoice default: grant the full CN total to credit balance.
		// Persisted IN the coordinator tx so it commits with the CAS.
		if cn.RefundAmountCents == 0 && cn.CreditAmountCents == 0 && cn.OutOfBandAmountCents == 0 && cn.TotalCents > 0 {
			cn.CreditAmountCents = cn.TotalCents
			if err := s.store.UpdateAllocationTx(ctx, tx, tenantID, id, cn.RefundAmountCents, cn.CreditAmountCents, cn.OutOfBandAmountCents); err != nil {
				return domain.CreditNote{}, fmt.Errorf("re-derive credit-note allocation at issue: %w", err)
			}
		}

		// INTERNAL effect — grant credit to the customer's balance IN the
		// coordinator tx (ADR-061). A grant failure rolls the draft→issued CAS
		// back together, so the issued-but-ungranted orphan (formerly a deferred
		// liveness gap) structurally cannot exist. The Stripe refund leg is
		// EXTERNAL and runs post-commit below.
		if cn.CreditAmountCents > 0 && s.credits != nil {
			if err := s.credits.GrantForCreditNoteTx(ctx, tx, tenantID, cn.ID, CreditGrantInput{
				CustomerID:  cn.CustomerID,
				Currency:    inv.Currency, // the money refunded is the invoice's (ADR-129)
				AmountCents: cn.CreditAmountCents,
				Description: fmt.Sprintf("Credit note %s — %s", cn.CreditNoteNumber, cn.Reason),
				InvoiceID:   cn.InvoiceID,
				// The balance rises by this whole amount; its drawn share goes
				// back to the grants it came from where it stays spendable
				// (ADR-132).
				InvoicePaid:       true,
				InvoiceTotalCents: inv.TotalAmountCents,
			}); err != nil {
				return domain.CreditNote{}, fmt.Errorf("grant credit: %w", err)
			}
		}
	} else {
		dueReduced, creditsReturned, settled, err = s.issueUnpaidTx(ctx, tx, tenantID, cn, inv)
		if err != nil {
			return domain.CreditNote{}, err
		}
	}

	if err := s.emitIssuedTx(ctx, tx, cn, dueReduced, creditsReturned, settled); err != nil {
		return domain.CreditNote{}, err
	}

	// Commit the CAS + internal effect atomically. From here the credit note is
	// durably issued; the remaining steps are EXTERNAL and recover independently.
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return domain.CreditNote{}, fmt.Errorf("commit issue tx: %w", err)
		}
	}

	// The invoice is paid: close its dunning run now. An active run would
	// close at its next retry, days away; an escalated one never would, since
	// the dunning ticks skip escalated runs. Best-effort, after the commit,
	// like every other settle path's resolve.
	if settled && s.dunning != nil {
		if err := s.dunning.ResolveByInvoice(ctx, tenantID, cn.InvoiceID, domain.ResolutionPaymentRecovered); err != nil {
			slog.WarnContext(ctx, "credit note marked the invoice paid but closing its dunning run failed; an active run closes at its next retry, an escalated one needs Resolve",
				"credit_note_id", cn.ID, "invoice_id", cn.InvoiceID, "error", err)
		}
	}

	// inv is the invoice as locked, before this note: the refund leg reads
	// its payment status, and a note that marked it paid must not look like a
	// note on a paid invoice there.
	return s.runPostIssueExternalLegs(ctx, tenantID, id, cn, inv)
}

// issueUnpaidTx is Issue's unpaid branch on the caller's tx, after the
// draft→issued CAS: it lowers what the invoice asks for, returns credits the
// note relieves beyond that, and marks the invoice paid when nothing is left
// due. It is the one caller of ApplyCreditNoteTx (ADR-061), shared by Issue
// and finalize's IssueHeldReliefTx.
func (s *Service) issueUnpaidTx(ctx context.Context, tx *sql.Tx, tenantID string, cn domain.CreditNote, inv domain.Invoice) (dueReduced, creditsReturned int64, settled bool, err error) {
	// Invoice not yet paid — reduce amount_due IN the coordinator tx. The
	// draft→issued CAS guarantees exactly one caller reaches here per credit
	// note, so the reduction is idempotent by construction (see Issue's
	// coordinator-tx comment): a concurrent/retried Issue() can't double-reduce.
	//
	// The note was sized when it was drafted, and customer credits may have
	// been applied to the invoice since, so less may be due now than the
	// note's total. The note reduces what is still due; the rest of it
	// relieves the part those credits covered, so that many credits come
	// off the invoice and go back to the customer's balance on this same
	// tx. Without this the invoice was relieved by more than its total and
	// the customer's credits were spent on nothing (ADR-061 amendment,
	// 2026-10-12).
	dueReduced = min(cn.TotalCents, inv.AmountDueCents)
	creditsReturned = cn.TotalCents - dueReduced
	if creditsReturned > 0 && s.credits == nil {
		return 0, 0, false, fmt.Errorf("credit note %s returns %d credits to the balance but no credit granter is wired", cn.ID, creditsReturned)
	}
	applied, err := s.invoices.ApplyCreditNoteTx(ctx, tx, tenantID, cn.InvoiceID, dueReduced, creditsReturned)
	if err != nil {
		return 0, 0, false, fmt.Errorf("reduce invoice amount: %w", err)
	}
	if creditsReturned > 0 {
		if err := s.credits.GrantForCreditNoteTx(ctx, tx, tenantID, cn.ID, CreditGrantInput{
			CustomerID:  cn.CustomerID,
			Currency:    inv.Currency, // the credits came off this invoice, in its currency
			AmountCents: creditsReturned,
			Description: fmt.Sprintf("Credit note %s — credits returned from invoice %s", cn.CreditNoteNumber, inv.InvoiceNumber),
			InvoiceID:   cn.InvoiceID,
		}); err != nil {
			return 0, 0, false, fmt.Errorf("return credits to balance: %w", err)
		}
	}
	// A note that leaves nothing due marks the invoice paid on this tx,
	// with invoice.paid (ADR-061 amendment 2026-10-19; Stripe: a credit
	// note that "reduces the balance of an open invoice to 0" makes it
	// paid). Before, the invoice stayed open at $0: no charge path takes a
	// $0 invoice, and dunning closed the run as recovered with the invoice
	// still unpaid, or ran its final action on it. The store leaves the
	// invoice open while a charge hold is set, the hold's owner included.
	if applied.AmountDueCents == 0 {
		if _, settled, err = s.invoices.SettleZeroDueTx(ctx, tx, tenantID, cn.InvoiceID, clock.Now(ctx)); err != nil {
			return 0, 0, false, fmt.Errorf("mark the invoice paid: %w", err)
		}
	}

	return dueReduced, creditsReturned, settled, nil
}

// emitIssuedTx writes the credit_note.issued audit row on the issuing tx,
// on the CAS-won path only (ADR-090).
func (s *Service) emitIssuedTx(ctx context.Context, tx *sql.Tx, cn domain.CreditNote, dueReduced, creditsReturned int64, settled bool) error {
	// ADR-090 in-tx emission — rides the coordinator tx with the CAS and
	// the internal effect, and only on the CAS-won path (lost CAS, defers,
	// and orphan-void produce no new issue fact). One canonical row for
	// operator issues, engine adjustments, reconciler retries, and catchup
	// — the background paths previously left no audit trail at all.
	if s.audit != nil {
		if auditErr := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        "credit_note.issued",
			ResourceType:  "credit_note",
			ResourceID:    cn.ID,
			ResourceLabel: cn.CreditNoteNumber,
			Metadata: map[string]any{
				"credit_note_number":       cn.CreditNoteNumber,
				"invoice_id":               cn.InvoiceID,
				"customer_id":              cn.CustomerID,
				"total_cents":              cn.TotalCents,
				"refund_amount_cents":      cn.RefundAmountCents,
				"credit_amount_cents":      cn.CreditAmountCents,
				"out_of_band_amount_cents": cn.OutOfBandAmountCents,
				"amount_due_reduced_cents": dueReduced,
				"credits_returned_cents":   creditsReturned,
				"marked_invoice_paid":      settled, // the invoice's paid row names this note
				"reason":                   cn.Reason,
				"currency":                 cn.Currency,
			},
		}); auditErr != nil {
			return fmt.Errorf("audit emission: %w", auditErr)
		}
	}
	return nil
}

// HeldRelief is a relief note IssueHeldReliefTx issued on finalize's tx.
// FinishHeldRelief runs what has to wait for that tx to commit.
type HeldRelief struct {
	CreditNoteID string
	// Settled: the note left nothing due and marked the invoice paid.
	Settled bool
}

// IssueHeldReliefTx issues the relief notes held on a draft invoice (a cancel,
// plan swap or decrease while collection was paused, ADR-118) on the finalize
// transaction: after the flip to finalized, before commit. The invoice then
// becomes chargeable only at the relieved amount. Before, finalize charged the
// full amount at once and the reconciler issued the note up to an hour later
// on a paid invoice, so the relief became account credit instead of coming off
// the charge (release plan row 13).
//
// inv is the invoice as finalized, locked by the caller's tx. Each note goes
// through the same CAS and unpaid branch as Issue, so the reconciler can never
// apply it a second time. Once a note leaves nothing due, the rest stay held:
// the invoice is paid, and the reconciler issues them on the paid branch, as
// account credit.
func (s *Service) IssueHeldReliefTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice) ([]HeldRelief, error) {
	notes, err := s.store.ListHeldReliefTx(ctx, tx, tenantID, inv.ID)
	if err != nil {
		return nil, err
	}
	var issued []HeldRelief
	cur := inv
	for _, cn := range notes {
		won, err := s.store.TransitionStatusTx(ctx, tx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
		if err != nil {
			return nil, fmt.Errorf("claim issue of held relief %s: %w", cn.CreditNoteNumber, err)
		}
		if !won {
			continue
		}
		dueReduced, creditsReturned, settled, err := s.issueUnpaidTx(ctx, tx, tenantID, cn, cur)
		if err != nil {
			return nil, fmt.Errorf("issue held relief %s: %w", cn.CreditNoteNumber, err)
		}
		if err := s.emitIssuedTx(ctx, tx, cn, dueReduced, creditsReturned, settled); err != nil {
			return nil, err
		}
		issued = append(issued, HeldRelief{CreditNoteID: cn.ID, Settled: settled})
		if settled {
			break
		}
		// The next note sees what this one left due.
		if cur, err = s.invoices.LockForCreditNoteTx(ctx, tx, tenantID, inv.ID); err != nil {
			return nil, fmt.Errorf("read the invoice after held relief %s: %w", cn.CreditNoteNumber, err)
		}
	}
	return issued, nil
}

// FinishHeldRelief runs, after finalize commits, what Issue runs after its
// own commit for each note IssueHeldReliefTx issued: the external legs (the
// upstream tax reversal; an unpaid-branch note has no refund leg) and, when a
// note marked the invoice paid, closing its dunning run. Best-effort like
// Issue's: each leg is recorded and recovered by its own sweep.
func (s *Service) FinishHeldRelief(ctx context.Context, tenantID string, issued []HeldRelief) {
	for _, h := range issued {
		cn, err := s.store.Get(ctx, tenantID, h.CreditNoteID)
		if err != nil {
			slog.WarnContext(ctx, "held relief issued at finalize, but reading it back for its tax reversal failed; the reversal sweep retries it",
				"credit_note_id", h.CreditNoteID, "error", err)
			continue
		}
		ctx := s.bindForInvoice(ctx, tenantID, cn.InvoiceID)
		inv, err := s.invoices.Get(ctx, tenantID, cn.InvoiceID)
		if err != nil {
			slog.WarnContext(ctx, "held relief issued at finalize, but reading its invoice failed; the reversal sweep retries it",
				"credit_note_id", cn.ID, "error", err)
			continue
		}
		if h.Settled && s.dunning != nil {
			if err := s.dunning.ResolveByInvoice(ctx, tenantID, cn.InvoiceID, domain.ResolutionPaymentRecovered); err != nil {
				slog.WarnContext(ctx, "held relief marked the invoice paid but closing its dunning run failed",
					"credit_note_id", cn.ID, "invoice_id", cn.InvoiceID, "error", err)
			}
		}
		if _, err := s.runPostIssueExternalLegs(ctx, tenantID, cn.ID, cn, inv); err != nil {
			slog.WarnContext(ctx, "held relief issued at finalize; its external legs failed and are retried by their sweeps",
				"credit_note_id", cn.ID, "error", err)
		}
	}
}

// issueHeld reports whether a draft credit note cannot issue against its
// source invoice as it stands: the source was voided, its charge is still in
// flight, or it is still a draft. holdIssue carries out the matching outcome.
//
// A written-off (uncollectible) source is not held: it is billed but unpaid,
// so the note issues on the unpaid branch and lowers what it asks for (ADR-050
// amendment 2026-10-23). A write-off reverses no tax (ADR-111), so the note's
// own tax reversal is the first for its slice. Until 2026-10-23 the note was
// voided here as an orphan, and a customer who later paid the written-off
// invoice paid for the time the note was for.
func issueHeld(inv domain.Invoice) bool {
	return inv.Status == domain.InvoiceVoided ||
		inv.PaymentInFlight() || inv.Status == domain.InvoiceDraft
}

// holdIssue is what Issue() does instead of issuing when issueHeld(inv): void
// an orphaned draft, or leave the draft for the reconciler to re-drive. It runs
// outside the coordinator tx.
func (s *Service) holdIssue(ctx context.Context, tenantID string, cn domain.CreditNote, inv domain.Invoice) (domain.CreditNote, error) {
	// Orphan guard. The source was voided after this draft was created — Void
	// already reversed the invoice's tax and zeroed its receivable, so there
	// is nothing left to relieve. Issuing now would (unpaid branch) re-reverse
	// the same tax transaction (double under-remit) against a terminal
	// invoice. Void the draft so it leaves the reconciler scan and never
	// applies. Pure status flip on a never-applied draft — no money movement,
	// no tax call.
	if inv.Status == domain.InvoiceVoided {
		// The draft→voided flip is a REAL committed mutation on a money
		// document — it carries its evidence in the same tx (ADR-090;
		// previously this path had zero audit rows once the handler's
		// post-hoc row was deleted).
		var emit func(tx *sql.Tx) error
		if s.audit != nil {
			emit = func(tx *sql.Tx) error {
				return s.audit.LogInTx(ctx, tx, audit.Entry{
					Action:        domain.AuditActionVoid,
					ResourceType:  "credit_note",
					ResourceID:    cn.ID,
					ResourceLabel: cn.CreditNoteNumber,
					Metadata: map[string]any{
						"action":        "orphan_draft_voided",
						"invoice_id":    cn.InvoiceID,
						"customer_id":   cn.CustomerID,
						"source_status": string(inv.Status),
					},
				})
			}
		}
		if _, terr := s.store.TransitionStatusAudited(ctx, tenantID, cn.ID, domain.CreditNoteDraft, domain.CreditNoteVoided, emit); terr != nil {
			return domain.CreditNote{}, fmt.Errorf("void orphaned clawback draft (source %s %s): %w", cn.InvoiceID, inv.Status, terr)
		}
		slog.InfoContext(ctx, "voided orphaned clawback draft (source annulled before issue)",
			"credit_note_id", cn.ID, "invoice_id", cn.InvoiceID, "source_status", string(inv.Status))
		return s.store.Get(ctx, tenantID, cn.ID)
	}

	// Defer-until-settle gate (ADR-059). The source's charge is still in flight
	// (processing/unknown) — its captured amount is unknown. Issuing now would
	// either reduce amount_due before MarkPaid records the captured amount
	// (under-record / undersized refund cap) or relieve a charge that may yet
	// succeed. Leave the draft status='draft' + issue_pending so the reconciler
	// re-drives it once the source reaches a terminal payment state, at which
	// point the paid/unpaid branch below selects the correct channel. This is
	// the single chokepoint for ALL issue triggers (engine CreateAndIssueAdjustment,
	// the post-commit issueClawbackDrafts, and the reconciler), so the automated
	// clawback paths defer here rather than each gating their own create site.
	if inv.PaymentInFlight() {
		slog.InfoContext(ctx, "deferring clawback issue until source payment settles",
			"credit_note_id", cn.ID, "invoice_id", cn.InvoiceID, "payment_status", string(inv.PaymentStatus))
		return cn, nil
	}
	// Same defer for a source that is still a DRAFT (a relief note written by
	// a cancel or plan swap while collection was paused, ADR-118): nothing is
	// owed on a draft yet, so the note waits — the reconciler re-drives it,
	// and it applies once the invoice is finalized (or the orphan guard above
	// voids it if the draft is voided).
	if inv.Status == domain.InvoiceDraft {
		slog.InfoContext(ctx, "deferring clawback issue until the source invoice is finalized",
			"credit_note_id", cn.ID, "invoice_id", cn.InvoiceID)
		return cn, nil
	}
	return cn, nil
}

// runPostIssueExternalLegs runs the POST-COMMIT external effects shared by
// Issue() and CreateAndIssueCommitRelief (ADR-080): the Stripe refund leg
// (idempotency-keyed hardcount_cn_<id>, refund_status persisted, RetryRefund
// recovers) and the upstream tax reversal (partial mode, sweep-recovered).
// Both are idempotent + recoverable, so a crash after the coordinator tx
// commits self-heals. Returns the current row with the leg statuses.
func (s *Service) runPostIssueExternalLegs(ctx context.Context, tenantID, id string, cn domain.CreditNote, inv domain.Invoice) (domain.CreditNote, error) {

	// Paid-branch Stripe refund (EXTERNAL — cannot share the DB tx, per "no
	// network I/O in a DB transaction"). Idempotency contract: the deterministic
	// `hardcount_cn_<cn_id>` key makes a retry after a partial failure return the
	// original refund_id rather than double-refund; refund_status is persisted
	// immediately, and a failed, unknown or pending leg recovers via RetryRefund.
	// With no PaymentIntent there is nothing to hand Stripe, and the leg could
	// never succeed. Validation refuses such an allocation up front; this
	// keeps rows written before that guard from reaching Stripe.
	if inv.PaymentStatus == domain.PaymentSucceeded && cn.RefundAmountCents > 0 && cn.StripeRefundID == "" {
		unknownKey := ""
		if s.refunder != nil && inv.HasCardPayment() {
			idempotencyKey := issueRefundKey(cn.ID)
			refundID, refStatus, err := s.refunder.CreateRefund(ctx, inv.StripePaymentIntentID, cn.RefundAmountCents, idempotencyKey, cn.ID)
			switch {
			case err != nil && refundOutcomeUnknown(err):
				// No clear answer: Stripe may have refunded the customer.
				// "failed" here would invite a second refund by hand in
				// Stripe (#965). Keep the key: RetryRefund resends this
				// request with it.
				slog.ErrorContext(ctx, "stripe refund result unknown: the customer may have been refunded; the credit note's refund is 'unknown' until Retry refund checks Stripe",
					"credit_note_id", cn.ID, "idempotency_key", idempotencyKey, "error", err)
				cn.RefundStatus = domain.RefundUnknown
				unknownKey = idempotencyKey
			case err != nil:
				slog.Warn("stripe refused the refund, credit note will be issued with failed refund status",
					"credit_note_id", cn.ID, "error", err)
				cn.RefundStatus = domain.RefundFailed
			default:
				cn.StripeRefundID = refundID
				// Record what Stripe actually said — succeeded OR pending (async).
				// A pending refund settles later via the refund webhook; recording
				// a blanket "succeeded" here is the false-success bug this fixes.
				cn.RefundStatus = refStatus
			}
		} else if !inv.HasCardPayment() {
			// Reachable only for a row written before the create-time guard:
			// there is no card, so this leg can never execute. "pending" would
			// claim a refund is in progress that cannot start, and would reach
			// the needs-attention queue only via the >72h rule. Say failed, and
			// say it loudly — RetryRefund answers with the void-and-re-issue
			// instruction rather than another doomed Stripe call.
			slog.Warn("credit note allocates a card refund on an invoice not paid by card; refund leg cannot execute",
				"credit_note_id", cn.ID, "invoice_id", inv.ID, "paid_out_of_band", inv.PaidOutOfBand)
			cn.RefundStatus = domain.RefundFailed
		} else {
			cn.RefundStatus = domain.RefundPending
		}
		if cn.RefundStatus != domain.RefundNone {
			// Record the refund leg's OUTCOME on the same write (ADR-090).
			// The credit_note.issued row carries the refund AMOUNT, but not
			// whether the money actually went back: an issue-time `failed`
			// leg means the customer was NOT refunded, which is exactly the
			// alertable fact an operator greps for — and the webhook and
			// retry paths both record it. Only a real state move emits.
			//
			// The write runs on afterRefundCall's context: the caller's
			// context may have ended during the Stripe call.
			wctx, cancel := afterRefundCall(ctx)
			defer cancel()
			var emit func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error
			if s.audit != nil {
				emit = func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error {
					if !changed {
						return nil
					}
					return s.audit.LogInTx(wctx, tx, audit.Entry{
						Action:        domain.AuditActionUpdate,
						ResourceType:  "credit_note",
						ResourceID:    updated.ID,
						ResourceLabel: updated.CreditNoteNumber,
						Metadata: map[string]any{
							"action":              "refund_status_changed",
							"refund_status":       string(updated.RefundStatus),
							"prior_refund_status": string(prior),
							"stripe_refund_id":    updated.StripeRefundID,
							"invoice_id":          updated.InvoiceID,
							"customer_id":         updated.CustomerID,
						},
					})
				}
			}
			if err := s.store.UpdateRefundStatusAudited(wctx, tenantID, id, RefundStatusWrite{Status: cn.RefundStatus, StripeRefundID: cn.StripeRefundID, ExpectedPriorRefundID: "", UnknownKey: unknownKey}, emit); err != nil {
				// The note is issued (committed) and the refund leg already ran, so
				// failing Issue now would only confuse its callers. The row stays
				// refund_status='none' with a refund amount, which the needs-attention
				// list surfaces after an hour; RetryRefund then adopts the refund
				// Stripe minted (metadata search) instead of creating a second one.
				slog.ErrorContext(ctx, "failed to persist refund status after the refund leg ran; the credit note will show in refunds needing attention — retry the refund",
					"credit_note_id", cn.ID, "refund_status", cn.RefundStatus,
					"stripe_refund_id", cn.StripeRefundID, "error", err)
			}
		}
	}

	// Reverse the invoice's upstream tax liability so the tenant's tax reports
	// reflect the reduced revenue (EU VAT Directive Art. 90, UK VATA 1994, India
	// CGST §34). EXTERNAL call → post-commit. Preconditions: taxRev wired, the
	// invoice has a committed upstream transaction, and this CN hasn't already
	// reversed. Partial mode with the CN's gross total so multi-CN invoices each
	// reverse only their slice.
	//
	// On failure this is NO LONGER fire-and-forget: set tax_reversal_pending so
	// RetryPendingCreditNoteTaxReversal re-drives with the same per-CN key, and
	// raise the log to ERROR (the #310 void-path sibling's alertable signal).
	// #310 itself cannot recover this — it scans voided/uncollectible invoices,
	// while a CN reversal lands on a finalized/paid invoice and stamps
	// credit_notes.tax_transaction_id, structurally invisible to that sweep.
	if s.taxRev != nil && inv.TaxTransactionID != "" && cn.TaxTransactionID == "" && cn.TotalCents > 0 {
		res, err := s.taxRev.ReverseTax(ctx, tenantID, tax.ReversalRequest{
			OriginalTransactionID: inv.TaxTransactionID,
			CreditNoteID:          cn.ID,
			InvoiceID:             cn.InvoiceID,
			Mode:                  tax.ReversalModePartial,
			GrossAmountCents:      cn.TotalCents,
		})
		if err != nil {
			slog.ErrorContext(ctx, "tax reversal failed; marked pending for sweep recovery",
				"credit_note_id", cn.ID,
				"invoice_id", cn.InvoiceID,
				"tax_transaction_id", inv.TaxTransactionID,
				"error", err)
			if merr := s.store.SetTaxReversalPending(ctx, tenantID, id, true); merr != nil {
				// Even if this fast-path marker write fails, the sweep still
				// recovers the reversal: RetryPendingCreditNoteTaxReversal also
				// derives eligibility structurally (issued CN, no reversal stamped,
				// tax-bearing source) and PROMOTES the row onto the unbounded
				// marker branch on its first failed re-drive — so the 24h window
				// bounds detection latency (one healthy leader tick), never the
				// obligation.
				slog.ErrorContext(ctx, "failed to set tax_reversal_pending marker; sweep recovers structurally regardless",
					"credit_note_id", cn.ID, "error", merr)
			}
		} else if res != nil && res.TransactionID != "" {
			if err := s.store.SetTaxTransaction(ctx, tenantID, id, res.TransactionID); err != nil {
				slog.Warn("tax reversal succeeded upstream but local persist failed",
					"credit_note_id", cn.ID,
					"reversal_transaction_id", res.TransactionID,
					"error", err)
			}
		}
	}

	// Status was already flipped to issued by the CAS; return the current row
	// (with the refund/tax fields persisted above). Read detached too: the
	// note is issued and its refund leg recorded, so an error here would tell
	// the caller nothing happened when something did.
	rctx, cancel := afterRefundCall(ctx)
	defer cancel()
	return s.store.Get(rctx, tenantID, id)
}

// RetryRefund re-attempts the Stripe refund for an already-issued
// credit note whose refund_status landed at `failed` (Stripe refused),
// `unknown` (Stripe gave no clear answer, #965) or `pending` (no refunder
// configured / no PI at Issue time, e.g. Stripe was wired up
// afterwards). The credit-note itself stays issued — only the Stripe-
// refund leg retries.
//
// Industry parity: Stripe's own `credit_notes.refunds` array is
// append-only; refunds have their own status independent of the CN's
// status. Hardcount tracks a single refund per CN (no array), but the
// retry contract matches: the CN is a financial document that stays
// in its final state while the cash-back leg can be re-driven by the
// operator.
//
// Idempotency: convergence rests on looking the refund up at Stripe
// before any create (see the numbered contract below). The key moves
// only after an outcome is recorded: from `unknown`, the retry resends
// the unclear request under its own key (refund_unknown_key); from
// `none`, it resends Issue's request (hardcount_cn_<id>).
//
// Rejects when:
//   - CN status != issued (drafts use Issue(); voided CNs are terminal)
//   - refund_status not in (failed, unknown, pending, none) — succeeded is final
//   - refund_amount_cents == 0 (credit-only CN; no Stripe leg exists)
//   - refunder not wired (Stripe not connected)
//   - invoice has no PaymentIntent (paid via credits only)
func (s *Service) RetryRefund(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	cn, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.CreditNote{}, err
	}
	// ADR-030: same time domain as the credit note being retried.
	ctx = s.bindForInvoice(ctx, tenantID, cn.InvoiceID)
	if cn.Status != domain.CreditNoteIssued {
		return domain.CreditNote{}, errs.InvalidState(fmt.Sprintf(
			"can only retry refund on issued credit notes (current status: %s)", cn.Status))
	}
	switch cn.RefundStatus {
	case domain.RefundFailed, domain.RefundPending:
		// retry-eligible
	case domain.RefundUnknown:
		// Stripe gave no clear answer to the last request. The steps below
		// look for the refund first, then resend that same request.
		if cn.RefundUnknownKey == "" {
			// The store writes the key with the status, so this is a row
			// written outside it. Resending under any other key could start
			// a second refund, so refuse.
			return domain.CreditNote{}, fmt.Errorf("credit note %s: refund_status unknown with no stored request key; check the payment in Stripe before refunding", cn.ID)
		}
	case domain.RefundNone:
		// An issued CN with a refund allocation but NO recorded outcome is
		// the Issue-crash window: the external leg ran (or was about to)
		// and the process died before the persist. The metadata search
		// below recovers a minted refund; finding none, a create is the
		// leg running for the first time. Refusing this state stranded the
		// exact lost-response class the adoption machinery exists for.
		// Issue's request may also still be running, so the create is a
		// resend of it, under its key (see below).
		if cn.RefundAmountCents <= 0 {
			return domain.CreditNote{}, errs.InvalidState("credit-only credit note has no refund leg to retry")
		}
	case domain.RefundSucceeded:
		return domain.CreditNote{}, errs.InvalidState("refund already succeeded — nothing to retry")
	default:
		return domain.CreditNote{}, errs.InvalidState(fmt.Sprintf(
			"cannot retry refund with status %q (refund leg did not run on this CN)", cn.RefundStatus))
	}
	if cn.RefundAmountCents <= 0 {
		return domain.CreditNote{}, errs.InvalidState("credit-only credit note has no refund leg to retry")
	}
	if s.refunder == nil {
		return domain.CreditNote{}, errs.InvalidState("refunder not configured — connect Stripe before retrying refund")
	}
	inv, err := s.invoices.Get(ctx, tenantID, cn.InvoiceID)
	if err != nil {
		return domain.CreditNote{}, fmt.Errorf("get invoice: %w", err)
	}
	// No path may refund a customer twice: a refund made in Stripe on this
	// invoice that is not settled yet may already be what this retry would
	// send (ADR-134).
	if err := s.refundBlockedByStripeRefund(ctx, tenantID, cn.InvoiceID); err != nil {
		return domain.CreditNote{}, err
	}
	// The operator-facing dead end this closes: a refund leg written against an
	// offline-paid invoice can NEVER succeed, because no Stripe payment backs
	// it. Before this guard the retry failed at Stripe and surfaced as an
	// opaque 500 — leaving the row in the "refunds needing attention" queue
	// with no way out. Answer it as the un-retryable state it is, so the
	// operator voids the note and re-issues against out_of_band instead of
	// clicking forever.
	if !inv.HasCardPayment() {
		if !inv.PaidOutOfBand {
			return domain.CreditNote{}, errs.InvalidState("invoice has no PaymentIntent — refund cannot be processed via Stripe")
		}
		return domain.CreditNote{}, errs.InvalidState("invoice was paid outside Stripe — this refund can never succeed; void this credit note and re-issue it allocated to out-of-band instead")
	}

	// ADR-090 in-tx emission for the operator retry. Emitted UNCONDITIONALLY —
	// including when the persisted state doesn't move. The audit-worthy fact
	// here is not the state transition but the OPERATOR ACTION: this call
	// issues a real Stripe refund request against the customer's payment. An
	// idempotent re-drive (Stripe returns the same refund id and status) still
	// happened, and a compliance log that omits repeated cash-back attempts
	// because they converged is lying by silence. `changed` rides the row so a
	// reader can tell a state-moving retry from a converged one.
	//
	// This is deliberately the OPPOSITE of ApplyRefundWebhook's gate: there, a
	// no-op redelivery is a non-event Stripe happened to send twice, so
	// recording it would fabricate a transition that never occurred.
	// retryEmit builds the per-persist emission, merging call-site extras —
	// which is where provider failure reasons finally get a durable home:
	// before this, Stripe's rejection reason lived only in process logs, so
	// "why did last quarter's refunds fail" was unanswerable retroactively.
	// ectx is the context of the write it rides.
	retryEmit := func(ectx context.Context, extra map[string]any) func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error {
		if s.audit == nil {
			return nil
		}
		return func(tx *sql.Tx, updated domain.CreditNote, prior domain.RefundStatus, changed bool) error {
			md := map[string]any{
				"action":              "refund_retried",
				"refund_status":       string(updated.RefundStatus),
				"prior_refund_status": string(prior),
				"status_changed":      changed,
				"stripe_refund_id":    updated.StripeRefundID,
				"invoice_id":          updated.InvoiceID,
				"customer_id":         updated.CustomerID,
			}
			for k, v := range extra {
				if s, isStr := v.(string); isStr && s == "" {
					continue // don't write empty reason keys
				}
				md[k] = v
			}
			return s.audit.LogInTx(ectx, tx, audit.Entry{
				Action:        domain.AuditActionUpdate,
				ResourceType:  "credit_note",
				ResourceID:    updated.ID,
				ResourceLabel: updated.CreditNoteNumber,
				Metadata:      md,
			})
		}
	}

	// ── Reconcile with provider truth BEFORE any create (ADR-063 amendment,
	// 2026-08-04). The old contract — re-call Create with the eternal
	// hardcount_cn_<id> key and let Stripe dedup — was broken twice over:
	// Stripe v1 idempotency keys EXPIRE after ~24h, while our own
	// needs-attention alert tells the operator to retry at 72h (a retried
	// stuck PARTIAL refund past the horizon minted a second live refund);
	// and within the horizon a key replay returns the SAVED create-time
	// response, so a webhook-recorded terminal `failed` could be overwritten
	// by a stale replayed `pending` that no webhook would ever correct.
	//
	// New contract, in order:
	//   1. Known refund id  → GetRefund (a READ — always current, never a
	//      replay) and persist what the provider says. succeeded → done
	//      (this also recovers a missed terminal webhook, the stand-in
	//      ADR-063 deferred the poller for). pending → refuse to mint a
	//      second refund while one is in flight. failed → fall through and
	//      create a genuinely new refund.
	//   2. No refund id (the create leg errored) → FindRefundForCreditNote
	//      by metadata hardcount_cn_id: if Stripe DID mint one despite the
	//      error, ADOPT it instead of duplicating (search-and-adopt,
	//      ADR-108's pattern). Found → persist and stop; the operator sees
	//      the adopted state and a second click walks path 1.
	//   3. Only when the provider confirms nothing viable exists → create.
	//      From `unknown` and `none`, the create is a resend of the last
	//      request under its own key (#965).
	//
	// Path 1 is skipped for `unknown`. Its stored id, if any, is a refund
	// Stripe already reported failed (a create runs only past a failed
	// one); the unclear request came after it. Reading that refund answers
	// "failed", which says nothing about the later request, and writing it
	// would put back the false "failed" that `unknown` replaced.
	if cn.StripeRefundID != "" && cn.RefundStatus != domain.RefundUnknown {
		provStatus, provReason, gerr := s.refunder.GetRefund(ctx, cn.StripeRefundID)
		if gerr != nil {
			return domain.CreditNote{}, fmt.Errorf("reconcile refund with provider: %w", gerr)
		}
		reconcileEmit := retryEmit(ctx, map[string]any{"reconciled_from_provider": true, "provider_failure_reason": provReason})
		if perr := s.store.UpdateRefundStatusAudited(ctx, tenantID, id, RefundStatusWrite{Status: provStatus, StripeRefundID: cn.StripeRefundID, ExpectedPriorRefundID: cn.StripeRefundID, ProviderRead: true}, reconcileEmit); perr != nil {
			return domain.CreditNote{}, fmt.Errorf("persist reconciled refund status: %w", perr)
		}
		switch provStatus {
		case domain.RefundSucceeded:
			return s.store.Get(ctx, tenantID, id)
		case domain.RefundPending:
			return domain.CreditNote{}, errs.InvalidState(fmt.Sprintf(
				"refund %s is still processing at Stripe — no new refund was created; it settles via webhook, or retry later", cn.StripeRefundID))
		}
		// Provider-confirmed failed → a new refund is legitimate. Fall
		// through to the search: a previous re-drive may have minted a
		// LIVE refund whose response was lost, and the stored id would
		// still point at the dead one. The search adopts live matches
		// only, so the dead id (provider-failed, hence not live) can
		// never be re-adopted — which is also what makes a pile of dead
		// prior attempts unable to shadow the create leg (the finder
		// review caught an exclude-one-id version of this livelocking
		// between two dead twins).
	}

	// Search-and-adopt (ADR-108's pattern) before any create, in BOTH lanes:
	// with no stored id (the create leg errored — Stripe may have minted one
	// anyway) and past a dead stored id (a lost re-drive may have too). Every
	// create stamps metadata hardcount_cn_id, so a lost refund is always
	// findable; adopting it is what makes convergence independent of the
	// idempotency key's 24h lifetime.
	foundID, foundStatus, ferr := s.refunder.FindRefundForCreditNote(ctx, inv.StripePaymentIntentID, cn.ID)
	if ferr != nil {
		return domain.CreditNote{}, fmt.Errorf("search provider refunds: %w", ferr)
	}
	if foundID != "" {
		adoptEmit := retryEmit(ctx, map[string]any{"adopted_from_provider": true})
		if perr := s.store.UpdateRefundStatusAudited(ctx, tenantID, id, RefundStatusWrite{Status: foundStatus, StripeRefundID: foundID, ExpectedPriorRefundID: cn.StripeRefundID, ProviderRead: true}, adoptEmit); perr != nil {
			return domain.CreditNote{}, fmt.Errorf("persist adopted refund: %w", perr)
		}
		return s.store.Get(ctx, tenantID, id)
	}

	// The idempotency key scopes to the CN's current STATE GENERATION, not
	// eternally to the CN: two operators double-clicking Retry read the same
	// row snapshot, derive the same key, and Stripe collapses them into one
	// refund — while a retry after a persisted state change (which bumps
	// updated_at) gets a fresh key instead of replaying a dead attempt's
	// saved response. Durable convergence never rests on the key: the
	// metadata stamp above makes any lost refund findable and adoptable.
	gen := sha256.Sum256([]byte(cn.StripeRefundID + "|" + string(cn.RefundStatus)))
	idempotencyKey := fmt.Sprintf("hardcount_cn_%s_r_%s", cn.ID, hex.EncodeToString(gen[:8]))
	// The exceptions are `unknown` and `none`: no outcome was recorded for
	// the last request, so the key must not move (money-path playbook, gate
	// 2). From `unknown` that request's key is stored; from `none` it is
	// Issue's, the only key Issue sends. The search above finds the refund
	// once Stripe has made it; it cannot see one Stripe is still making,
	// which Stripe's incident recovery can do later. Resending under the
	// same key covers that: while the first request runs Stripe answers
	// 409, after it Stripe returns its result, and only a key Stripe never
	// ran makes a new refund. A new key "can start a second operation"
	// (https://docs.stripe.com/error-low-level#content-errors).
	resend := false
	switch cn.RefundStatus {
	case domain.RefundUnknown:
		idempotencyKey, resend = cn.RefundUnknownKey, true
	case domain.RefundNone:
		idempotencyKey, resend = issueRefundKey(cn.ID), true
	}
	refundID, refStatus, refundErr := s.refunder.CreateRefund(ctx, inv.StripePaymentIntentID, cn.RefundAmountCents, idempotencyKey, cn.ID)
	// Stripe may have refunded the customer from here on, so what follows
	// records it on afterRefundCall's context: the caller's may have ended
	// during the call.
	wctx, cancel := afterRefundCall(ctx)
	defer cancel()
	if refundErr != nil {
		// A refusal is "failed"; no clear answer is "unknown", which keeps
		// this request's key for the next retry to resend (#965). On a
		// resend, an error Stripe raised before running it (an invalid API
		// key, a key without refund permission) is about this request only:
		// the earlier one's result is still unknown, so the note becomes or
		// stays unknown with that request's key. Stripe saves no result for
		// such an error, and says to retry "the same operation" with the same
		// key (https://docs.stripe.com/error-low-level#content-errors).
		status, unknownKey := domain.RefundFailed, ""
		switch {
		case refundOutcomeUnknown(refundErr):
			status, unknownKey = domain.RefundUnknown, idempotencyKey
			slog.ErrorContext(ctx, "stripe refund retry result unknown: the customer may have been refunded; retry again later, it resends this request",
				"credit_note_id", id, "idempotency_key", idempotencyKey, "error", refundErr)
		case resend && refundRequestNotRun(refundErr):
			status, unknownKey = domain.RefundUnknown, idempotencyKey
			refundErr = &refundStillUnknownError{err: refundErr}
			slog.WarnContext(ctx, "stripe did not run the refund resend; the earlier request's result is still unknown",
				"credit_note_id", id, "idempotency_key", idempotencyKey, "error", refundErr)
		}
		// Persist the still-failed state so the next retry has the latest error
		// context and the dashboard surfaces accurate status. It rides the same
		// audited write, and that emission is UNCONDITIONAL — a failed→failed
		// re-drive still records a row, carrying status_changed=false.
		//
		// That is deliberate, and it is the one place the "emit only on a
		// genuinely-mutated path" rule is inverted on purpose: the operator's
		// retry IS the fact. It hit Stripe. A row saying "the operator retried
		// this refund and it failed again" is exactly the money-path evidence an
		// auditor needs, and suppressing it because the STATUS did not move would
		// erase a real operator action. (The opposite of a webhook no-op
		// redelivery, which mutates nothing and must NOT emit.) The store REPORTS
		// whether state moved; this caller DECIDES that the action is the fact —
		// see the registry note on POST /v1/credit-notes/{id}/retry-refund.
		//
		// The persist error is LOGGED, never discarded, and the Stripe error stays
		// primary.
		failEmit := retryEmit(wctx, map[string]any{"provider_error": refundErr.Error()})
		if perr := s.store.UpdateRefundStatusAudited(wctx, tenantID, id, RefundStatusWrite{Status: status, StripeRefundID: cn.StripeRefundID, ExpectedPriorRefundID: cn.StripeRefundID, ProviderRead: true, UnknownKey: unknownKey}, failEmit); perr != nil {
			slog.ErrorContext(ctx, "persist refund status after a failed retry",
				"credit_note_id", id, "refund_status", status, "error", perr)
		}
		return domain.CreditNote{}, fmt.Errorf("stripe refund retry: %w", refundErr)
	}

	// A resend can be answered from Stripe's saved response to the first
	// request (https://docs.stripe.com/api/idempotent_requests), so its
	// status may be days old: a refund that has since failed would read
	// "succeeded". Read the refund's current status instead. If the read
	// fails, record nothing: the row keeps its status (unknown or none), and
	// the next retry finds the refund by search (if live) or gets it back
	// from the resend again.
	providerRead := false
	if resend {
		cur, _, gerr := s.refunder.GetRefund(wctx, refundID)
		if gerr != nil {
			return domain.CreditNote{}, fmt.Errorf("refund %s exists at Stripe, but reading its status failed; retry again: %w", refundID, gerr)
		}
		refStatus, providerRead = cur, true
	}

	// Record Stripe's actual status — a re-driven refund can come back `pending`
	// (still settling), not necessarily succeeded; the refund webhook settles it.
	// Recording a blanket succeeded here would re-introduce the false-success lie
	// and permanently 409 a legitimate later retry.
	if err := s.store.UpdateRefundStatusAudited(wctx, tenantID, id, RefundStatusWrite{Status: refStatus, StripeRefundID: refundID, ExpectedPriorRefundID: cn.StripeRefundID, ProviderRead: providerRead}, retryEmit(wctx, nil)); err != nil {
		// Stripe call succeeded but local persist (or its audit emission —
		// shared fate) failed. Convergence is the METADATA, not the key: the
		// next retry's FindRefundForCreditNote finds this refund by
		// hardcount_cn_id and adopts it — no duplicate even after the key ages out.
		return domain.CreditNote{}, fmt.Errorf("persist refund status: %w", err)
	}

	return s.store.Get(wctx, tenantID, id)
}

// ApplyRefundWebhook applies an async refund-webhook status (already mapped to a
// Hardcount refund_status by the payment layer) to the credit note carrying
// stripeRefundID, monotonically (terminal wins; a stale 'pending' never clobbers
// a terminal). This is the source of truth for the async refund outcome
// (pending→succeeded/failed) that the create-call cannot observe. Returns
// ErrNotFound for an unknown/foreign refund id — the caller decides ack vs retry.
func (s *Service) ApplyRefundWebhook(ctx context.Context, tenantID, stripeRefundID string, status domain.RefundStatus, failureReason string) error {
	// ADR-090 in-tx emission: the refund-status transition and its audit
	// row commit together; the store fires the emission ONLY when the
	// monotonic guard actually flipped a row, so stale redeliveries never
	// fabricate evidence. `update` + metadata action discriminator keeps
	// the wire vocabulary frozen (renders via the FE's update branch); the
	// failed flip is the alertable value an operator greps for.
	var emit func(tx *sql.Tx, cn domain.CreditNote) error
	if s.audit != nil {
		emit = func(tx *sql.Tx, cn domain.CreditNote) error {
			return s.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionUpdate,
				ResourceType:  "credit_note",
				ResourceID:    cn.ID,
				ResourceLabel: cn.CreditNoteNumber,
				Metadata: withoutEmpty(map[string]any{
					"action":                  "refund_status_changed",
					"refund_status":           string(status),
					"stripe_refund_id":        stripeRefundID,
					"invoice_id":              cn.InvoiceID,
					"customer_id":             cn.CustomerID,
					"provider_failure_reason": failureReason,
				}),
			})
		}
	}
	return s.store.ApplyRefundWebhookStatusAudited(ctx, tenantID, stripeRefundID, status, emit)
}

func (s *Service) Void(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	cn, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.CreditNote{}, err
	}
	// ADR-030: the void's updated_at belongs to the invoice's time domain.
	ctx = s.bindForInvoice(ctx, tenantID, cn.InvoiceID)
	if cn.Status == domain.CreditNoteVoided {
		return domain.CreditNote{}, errs.InvalidState("credit note is already voided")
	}
	// Industry standard: only draft credit notes can be voided.
	// Once issued, a credit note is a financial document — side effects
	// (invoice reduction, credit grants, Stripe refunds) have already occurred.
	// To correct a mistake on an issued CN, create a new invoice or adjustment.
	if cn.Status == domain.CreditNoteIssued {
		return domain.CreditNote{}, errs.InvalidState("cannot void an issued credit note — issued credit notes are final financial documents")
	}
	// Refuse to void a draft CN whose Stripe refund leg already executed.
	// CreateRefund() voids the draft as a best-effort rollback when Issue()
	// returns an error — but Issue() persists the Stripe refund_id BEFORE
	// the credit-grant / tax-reversal steps, so an error after the refund
	// leg succeeded leaves a draft CN that ALREADY pushed cash back to the
	// payment method. The over-refund cap in Create()/CreateRefund() sums
	// RefundAmountCents only over non-voided CNs, so voiding this CN would
	// drop its executed refund from that ceiling — a later CN could then
	// refund the same money again (double refund). Keep the CN un-voided so
	// it stays counted; the operator reconciles the partially-issued CN
	// (e.g. RetryRefund / re-Issue) instead.
	if cn.StripeRefundID != "" || cn.RefundStatus == domain.RefundSucceeded {
		return domain.CreditNote{}, errs.InvalidState("cannot void a credit note whose refund has already been processed — voiding would drop the executed refund from the over-refund cap and allow a duplicate refund. Reconcile the existing refund instead.")
	}

	// ADR-090 in-tx emission. The flip is a CAS (draft→voided), not the former
	// blind `UPDATE … WHERE id=$1`: an audit row may only evidence a transition
	// that PROVABLY happened, and only the CAS can prove it — a zero-row flip
	// records nothing. draft is the sole voidable state (the two guards above
	// reject issued and voided), so it is the only legal from-status.
	var emit func(tx *sql.Tx) error
	if s.audit != nil {
		emit = func(tx *sql.Tx) error {
			return s.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionVoid,
				ResourceType:  "credit_note",
				ResourceID:    cn.ID,
				ResourceLabel: cn.CreditNoteNumber,
				Metadata: map[string]any{
					"credit_note_number": cn.CreditNoteNumber,
					"invoice_id":         cn.InvoiceID,
					"customer_id":        cn.CustomerID,
				},
			})
		}
	}
	won, err := s.store.TransitionStatusAudited(ctx, tenantID, id, domain.CreditNoteDraft, domain.CreditNoteVoided, emit)
	if err != nil {
		return domain.CreditNote{}, err
	}
	if !won {
		// The note left 'draft' between the guard reads above and the flip (a
		// concurrent Issue() or Void()). Nothing was written and nothing was
		// recorded — re-read and return the canonical error for its CURRENT
		// state rather than reporting a void that didn't happen.
		cur, gerr := s.store.Get(ctx, tenantID, id)
		if gerr != nil {
			return domain.CreditNote{}, gerr
		}
		switch cur.Status {
		case domain.CreditNoteVoided:
			return domain.CreditNote{}, errs.InvalidState("credit note is already voided")
		case domain.CreditNoteIssued:
			return domain.CreditNote{}, errs.InvalidState("cannot void an issued credit note — issued credit notes are final financial documents")
		default:
			return domain.CreditNote{}, errs.InvalidState(fmt.Sprintf("credit note is no longer voidable (current status: %s)", cur.Status))
		}
	}
	return s.store.Get(ctx, tenantID, id)
}
