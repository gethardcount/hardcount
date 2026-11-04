package domain

import "time"

// TaxFacts is the 13-field bundle that one tax calculation produces. Every
// invoice writer must carry these facts unchanged from the engine's
// ApplyTaxToLineItems onto the invoice row. The struct is embedded in
// domain.Invoice, billing.TaxApplication, domain.InvoiceTaxRetryUpdate and
// subscription.ProrationTaxResult. It is untagged, so its JSON keys stay flat
// and the wire shape is the same. Each copy is one struct assignment that the
// compiler checks.
//
// One shared type keeps any writer from dropping a tax fact, which a
// field-by-field copy into a mirror type can do. A dropped provider record
// leaves tax charged but never reported upstream. A dropped reverse-charge or
// exemption fact loses the legend on the invoice. A dropped TaxErrorCode
// leaves a deferred invoice with an empty tax_error_code, which the
// tax-retry reconciler's retryable-code filter never matches, so the invoice
// is never retried. To add a tax field, add it here, and every embedder
// carries it.
//
// These are left out on purpose, because they belong to the invoice's
// lifecycle and are not products of a calculation: TaxTransactionID (written
// at Commit or finalize, not at calculation), and TaxRetryCount and
// TaxNextRetryAt (bookkeeping for the retry scheduler).
type TaxFacts struct {
	TaxAmountCents int64   `json:"tax_amount_cents"`
	TaxRate        float64 `json:"tax_rate"` // Percent rate (4-decimal precision via NUMERIC(7,4)). 7.25 = 7.25%. ADR-042/043.
	TaxName        string  `json:"tax_name,omitempty"`
	TaxCountry     string  `json:"tax_country,omitempty"`
	TaxID          string  `json:"tax_id,omitempty"`
	// These fields are a durable audit snapshot of the tax decision. They are
	// written once, when the invoice is built, and never changed. So a
	// finalized invoice can still be reconstructed after the provider's
	// tax_calculation expires (24 h for Stripe Tax). TaxCalculationID is the
	// provider reference that Commit uses to create a tax_transaction at
	// finalize.
	TaxProvider      string `json:"tax_provider,omitempty"`
	TaxCalculationID string `json:"tax_calculation_id,omitempty"`
	TaxReverseCharge bool   `json:"tax_reverse_charge,omitempty"`
	TaxExemptReason  string `json:"tax_exempt_reason,omitempty"`
	// TaxStatus gates finalize: only invoices with TaxStatus=ok can be
	// finalized. Pending and failed invoices carry no committed tax yet. They
	// wait for a retry, or for an operator to step in.
	TaxStatus        InvoiceTaxStatus `json:"tax_status,omitempty"`
	TaxDeferredAt    *time.Time       `json:"tax_deferred_at,omitempty"`
	TaxPendingReason string           `json:"tax_pending_reason,omitempty"`
	// TaxErrorCode is the typed classification of TaxPendingReason: one of
	// customer_data_invalid, jurisdiction_unsupported, provider_outage,
	// provider_auth or unknown. The operator UI branches on the cause (fix
	// the customer data, or wait for the provider), and webhook consumers
	// route alerts by it. It is also what the tax-retry reconciler's
	// retryable-code filter matches on. It is empty for invoices deferred
	// before this column existed (migration 0067).
	TaxErrorCode string `json:"tax_error_code,omitempty"`
}
