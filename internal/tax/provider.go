// Package tax defines the tenant-selectable tax calculation backend and
// three concrete implementations: NoneProvider (skip tax), ManualProvider
// (flat tenant-level rate), and StripeTaxProvider (Stripe Tax API; defers
// the invoice to tax_status=pending for retry on error).
//
// Every tenant picks one Provider via tenant_settings.tax_provider; the
// billing engine calls Calculate at invoice build time and, for
// providers that support it, Commit at invoice finalize time to durably
// record the tax decision with the upstream (e.g. create a Stripe
// tax_transaction from the earlier calculation).
package tax

import (
	"context"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Provider is the tenant-scoped tax calculation strategy.
type Provider interface {
	// Name returns the provider identifier matching the value stored in
	// tenant_settings.tax_provider and written to invoices.tax_provider.
	Name() string

	// Calculate returns the tax assessment for a Request. Implementations
	// MUST NOT persist external state here — Commit is the durable write.
	Calculate(ctx context.Context, req Request) (*Result, error)

	// Commit finalizes the calculation referenced by calcRef against the
	// named invoice (for providers that have a durable state — Stripe Tax
	// records a tax_transaction; none/manual are no-ops). Called from the
	// invoice finalize path. The returned transactionID is the upstream
	// provider's durable reference (e.g. Stripe Tax tx_xxx) which the
	// caller persists onto the invoice so a later credit note can reverse
	// the tax against the same transaction. Empty for providers without
	// durable state.
	Commit(ctx context.Context, calcRef, invoiceID string) (transactionID string, err error)

	// Reverse issues a reversal against a previously committed tax
	// transaction. Called from the credit note issue path. Providers
	// without durable upstream state (none, manual) return an empty
	// ReversalResult with no error so the credit note flow can ignore
	// the outcome without branching on provider name.
	Reverse(ctx context.Context, req ReversalRequest) (*ReversalResult, error)
}

// ReversalMode selects full-vs-partial reversal semantics. Full reverses
// the entire original transaction and ignores the amount field; partial
// reverses exactly the supplied gross amount and leaves residual liability
// on the original transaction equal to (original_total - gross_amount).
type ReversalMode string

const (
	ReversalModeFull    ReversalMode = "full"
	ReversalModePartial ReversalMode = "partial"
)

// ReversalRequest is the input to Provider.Reverse.
type ReversalRequest struct {
	// OriginalTransactionID is the upstream tax_transaction id produced
	// by the earlier Commit (Stripe: tx_xxx). Required.
	OriginalTransactionID string

	// Reference is the caller-supplied unique identifier for this
	// reversal. Stripe requires the reference be globally unique
	// across all tax_transactions in the account, so callers MUST
	// supply a value that uniquely identifies the logical reversal
	// (CN id for credit-note reversals, "inv_void_<id>" for invoice
	// void reversals). Retries with the same Reference converge.
	// Falls back to CreditNoteID when empty for back-compat.
	Reference string

	// CreditNoteID — back-compat field; populated by the credit-note
	// path. New callers should set Reference directly.
	CreditNoteID string

	// InvoiceID is for log/audit context only — the reversal targets the
	// transaction id above, not the invoice.
	InvoiceID string

	// Mode chooses full or partial. For full, GrossAmountCents is ignored.
	Mode ReversalMode

	// GrossAmountCents is the refund total INCLUDING tax, in positive
	// smallest-currency units. The provider handles sign conversion when
	// calling upstream. Only meaningful when Mode == ReversalModePartial.
	GrossAmountCents int64
}

// ReversalResult is the output of Provider.Reverse. TransactionID is the
// reversal's upstream id (Stripe: tx_xxx for the negative transaction),
// empty for providers without durable state.
type ReversalResult struct {
	TransactionID string
}

// CustomerTaxStatus is re-exported from domain so provider callers can keep
// using tax.StatusStandard / StatusExempt / StatusReverseCharge without an
// additional import. The canonical definition lives in domain because the
// billing profile struct carries the value and we want zero-cycle imports.
type CustomerTaxStatus = domain.CustomerTaxStatus

const (
	StatusStandard      = domain.TaxStatusStandard
	StatusExempt        = domain.TaxStatusExempt
	StatusReverseCharge = domain.TaxStatusReverseCharge
)

// Address is the buyer's billing address — the minimum Stripe Tax needs to
// resolve jurisdiction, and what manual mode prints on the invoice.
type Address struct {
	Line1      string
	Line2      string
	City       string
	State      string
	PostalCode string
	Country    string // ISO-3166 alpha-2
}

// Request is the input to Calculate. One request == one invoice draft.
type Request struct {
	// Currency is the ISO-4217 code of the invoice (lowercase at the Stripe
	// boundary; the provider normalizes).
	Currency string

	// Customer fields from the buyer's billing profile.
	CustomerAddress      Address
	CustomerTaxID        string
	CustomerTaxIDType    string
	CustomerStatus       CustomerTaxStatus
	CustomerExemptReason string // copied onto the invoice audit snapshot when Status == exempt

	// TaxInclusive flips the interpretation of line amounts: when true,
	// line amounts are gross (tax-inclusive) and the provider carves tax
	// out of them; when false (default), amounts are net and tax is added
	// on top.
	TaxInclusive bool

	// LineItems with their Stripe product tax codes. Exactly one provider
	// call per invoice — we send all lines together so jurisdiction-level
	// rounding can be reconciled server-side.
	LineItems []RequestLine

	// DiscountCents is an invoice-level discount that providers distribute
	// proportionally across lines before taxing. Zero when no discount.
	DiscountCents int64

	// DefaultTaxCode is the tenant's fallback product tax code used when a
	// line's TaxCode is empty (e.g. "txcd_10103001" for SaaS business).
	DefaultTaxCode string

	// OnFailure selects the provider's behaviour when a transient error
	// prevents a real calculation (Stripe API outage, missing credentials,
	// missing customer country). Post-ADR-041 only "block" is valid — the
	// provider returns the error unchanged so the engine can defer the
	// invoice to tax_status=pending for TaxRetrier to pick up. The legacy
	// "fallback_manual" value is removed; existing tenants are migrated
	// to "block" by the 0103 migration. The field is retained on Request
	// for forward-compat (e.g. future defer_with_delay shapes).
	OnFailure string
}

// OnFailureBlock is the only valid OnFailure value post-ADR-041.
const OnFailureBlock = "block"

// RequestLine is one line item passed to the provider for taxation.
type RequestLine struct {
	// Ref is a caller-opaque identifier echoed back in Result.Lines so the
	// engine can match provider responses to its own line items. Typical
	// values: "line_0", "line_1", ...
	Ref         string
	AmountCents int64
	Quantity    int64
	// TaxCode is the Stripe product tax code for this line. Empty falls
	// back to Request.DefaultTaxCode.
	TaxCode string
}

// Result is the output of Calculate for a single Request.
type Result struct {
	Provider string // "none" | "manual" | "stripe_tax"

	// CalculationID is the upstream's reference (e.g. Stripe
	// tax_calculation id). Used by Commit to create a tax_transaction at
	// finalize time. Empty for providers with no durable calculation.
	CalculationID string

	// Aggregate totals.
	TotalTaxCents int64
	EffectiveRate float64 // Percent rate (4-decimal precision). 8.875 = 8.875%. ADR-042/043.

	// Primary tax label and jurisdiction, displayed on the aggregate tax
	// row of the invoice. When the calculation produces multiple
	// jurisdictions (e.g. India CGST+SGST, US state+local), Breakdowns
	// holds the detail and this is the summary.
	TaxName    string
	TaxCountry string

	// ReverseCharge is true when the buyer self-accounts for tax in their
	// jurisdiction. Drives the invoice PDF reverse-charge legend.
	ReverseCharge bool

	// Exempt is true when the customer's status caused tax to be zero
	// (status = exempt). ExemptReason is the free-text reason propagated
	// from the customer billing profile for the invoice audit snapshot.
	Exempt       bool
	ExemptReason string

	// Per-line results, in the same order and Ref as Request.LineItems.
	Lines []ResultLine

	// Breakdowns are jurisdiction-level aggregates the invoice PDF renders
	// as a table when len > 1.
	Breakdowns []Breakdown

	// RequestRaw + ResponseRaw are the opaque bytes the engine persists to
	// tax_calculations for durable audit. Empty for NoneProvider.
	RequestRaw  []byte
	ResponseRaw []byte
}

// ResultLine is the provider's tax assessment for one input line.
type ResultLine struct {
	Ref string
	// NetAmountCents is the pre-tax amount for the line. Equals the request
	// line's AmountCents in exclusive mode; in inclusive mode it is the
	// gross amount with tax carved out so subtotal + tax == gross total.
	NetAmountCents int64
	TaxAmountCents int64
	TaxRate        float64 // Percent rate (4-decimal precision). ADR-042/043.
	TaxName        string
	Jurisdiction   string // e.g. "US-CA", "IN-MH"
	TaxCode        string
	// TaxabilityReason is the Stripe-canonical reason this line was taxed
	// the way it was (e.g. "standard_rated", "reverse_charge",
	// "not_collecting", "customer_exempt", "product_exempt", "zero_rated").
	// Empty for non-Stripe providers — none/manual don't surface a reason.
	// Treated as an opaque string at the persistence boundary; the PDF and
	// dashboard map known values to human-readable legends but tolerate
	// unknown ones for forward compatibility with future Stripe additions.
	TaxabilityReason string
}

// Breakdown is one jurisdiction's contribution to the total tax. For
// single-jurisdiction calculations (manual mode, most Stripe Tax outputs)
// len(Breakdowns) == 1. Multi-jurisdiction regimes (India CGST+SGST, EU
// cross-state, US state+local) produce multiple rows the PDF renders as a
// table.
type Breakdown struct {
	Jurisdiction string
	Name         string
	Rate         float64 // Percent rate (4-decimal precision). ADR-042/043.
	AmountCents  int64
}

// exemptResult returns a zeroed Result for an exempt or reverse-charge
// customer. Shared by all providers so PDF legends render consistently
// regardless of the configured backend. NetAmountCents stays equal to the
// request line amount so engine-side sums balance (no tax carved out).
func exemptResult(providerName string, req Request, reverseCharge bool, exemptReason string) *Result {
	lines := make([]ResultLine, len(req.LineItems))
	for i, li := range req.LineItems {
		lines[i] = ResultLine{Ref: li.Ref, NetAmountCents: li.AmountCents}
	}
	return &Result{
		Provider:      providerName,
		Lines:         lines,
		ReverseCharge: reverseCharge,
		Exempt:        !reverseCharge,
		ExemptReason:  exemptReason,
	}
}
