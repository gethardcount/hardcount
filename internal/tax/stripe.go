package tax

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// stripeTaxOutcomes counts non-happy-path outcomes from StripeTaxProvider
// by outcome class and reason. Operators alert on it to catch Stripe Tax
// becoming unusable — the engine defers the invoice to tax_status=pending
// and the TaxRetrier reconciler picks it up on the next tick.
//
// Labels:
//
//	outcome ∈ {deferred}            (post-ADR-041; legacy "fallback" cut 2026-05-30)
//	reason  ∈ {no_country, no_client_for_mode, api_error}
//
// Happy-path calculations (successful Stripe Tax calls, exempt, reverse-charge)
// are not counted here — this vector is intentionally a failure-mode signal.
var stripeTaxOutcomes *prometheus.CounterVec

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_tax_outcome_total",
		Help: "Count of non-happy Stripe tax outcomes, by outcome (deferred) and reason.",
	}, []string{"outcome", "reason"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			stripeTaxOutcomes = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		stripeTaxOutcomes = c
	}
}

// StripeClientResolver returns the *stripe.Client for the caller's context.
// Satisfied by *payment.StripeClients, which derives per-tenant credentials
// from ctx (tenant_id + livemode). Decoupled as an interface so the tax
// package does not depend on the payment package.
type StripeClientResolver interface {
	ForCtx(ctx context.Context) *stripe.Client
}

// StripeTaxProvider calls the Stripe Tax API. Calculate creates a
// tax_calculation; Commit creates a tax_transaction from the earlier
// calculation reference at invoice-finalize time. The transaction is what
// makes the tax decision durable upstream (calculations expire in 24 hours,
// transactions are permanent and show up in Stripe's tax reporting).
//
// On any Stripe API error Calculate returns the error so the engine can
// defer the invoice to tax_status=pending; the TaxRetrier reconciler picks
// it up on the next scheduler tick. The legacy "fallback to ManualProvider"
// branch was cut 2026-05-30 per ADR-041 (it silently substituted zero tax
// when no manual rate matched the jurisdiction, overriding operator intent).
//
// Multi-tenant: the Stripe client is resolved per ctx, so each tenant's
// calculation and commit hit their own Stripe account. A calculation itself
// doesn't mutate external state, but a tax_transaction does, so routing to
// the correct account matters.
type StripeTaxProvider struct {
	clients StripeClientResolver
}

// NewStripeTaxProvider wires a per-tenant Stripe client resolver. A nil
// resolver or a resolver that returns nil for a given ctx causes Calculate
// to defer the invoice — operator gets an actionable signal via the
// invoice's Attention banner instead of a silent zero-tax invoice.
func NewStripeTaxProvider(clients StripeClientResolver) *StripeTaxProvider {
	return &StripeTaxProvider{clients: clients}
}

func (*StripeTaxProvider) Name() string { return "stripe_tax" }

func (p *StripeTaxProvider) clientForCtx(ctx context.Context) *stripe.Client {
	if p.clients == nil {
		return nil
	}
	return p.clients.ForCtx(ctx)
}

func (p *StripeTaxProvider) Calculate(ctx context.Context, req Request) (*Result, error) {
	switch req.CustomerStatus {
	case StatusExempt:
		return exemptResult("stripe_tax", req, false, req.CustomerExemptReason), nil
	case StatusReverseCharge:
		return exemptResult("stripe_tax", req, true, ""), nil
	}
	if len(req.LineItems) == 0 {
		return &Result{Provider: "stripe_tax"}, nil
	}

	// Provider-connection check runs BEFORE customer-data validation.
	// Reason: when a tenant has tax_provider=stripe_tax but no Stripe
	// credentials connected for the active mode, the underlying issue
	// is configuration, not customer data — and the operator sees the
	// correct, actionable reason ("provider_not_configured") instead
	// of a misleading "no_country" or other downstream failure that
	// only fires because the client never got built. Also avoids any
	// pretence of touching Stripe — clientForCtx returns nil from a
	// pure local credential lookup when nothing is connected.
	client := p.clientForCtx(ctx)
	if client == nil {
		return p.handleFailure(ctx, req, "no_client_for_mode",
			fmt.Errorf("stripe tax: no Stripe credentials connected for livemode=%v — connect Stripe in Settings → Payments or change tax provider", postgres.Livemode(ctx)))
	}

	// Stripe Tax needs at minimum a country to resolve jurisdiction.
	if req.CustomerAddress.Country == "" {
		return p.handleFailure(ctx, req, "no_country",
			fmt.Errorf("stripe tax: customer has no country on billing profile"))
	}

	params := p.buildParams(req)

	// Serialize the outbound params before the call so we can persist a
	// faithful audit snapshot even on error. Stripe's SDK types marshal
	// cleanly enough for the tax_calculations.request JSONB column.
	reqRaw, _ := json.Marshal(params)

	// A scoped request (one billing step of one subscription) is idempotent at
	// Stripe: the key hashes the scope AND the exact request, so a retry of a
	// failed step that sends the same lines reuses the first calculation
	// instead of paying for another, while any change — new usage, a new
	// address — is a different key and a fresh calculation. Stripe replays a
	// key for 24h; the calculation itself expires after 24h too.
	if scope := calculationScope(ctx); scope != "" {
		sum := sha256.Sum256(append([]byte(scope+"\x00"), reqRaw...))
		params.IdempotencyKey = stripe.String("velox_tax_calc_" + hex.EncodeToString(sum[:20]))
	}

	calc, err := client.V1TaxCalculations.Create(ctx, params)
	if err != nil {
		return p.handleFailure(ctx, req, "api_error", fmt.Errorf("stripe tax: %w", err))
	}
	// The expanded line_items is a paged list: an invoice with more lines
	// than its first page comes back has_more=true. Page the rest from the
	// calculation's line-items endpoint so every line gets its tax.
	if calc.LineItems != nil && calc.LineItems.HasMore {
		all := make([]*stripe.TaxCalculationLineItem, 0, len(req.LineItems))
		for li, lerr := range client.V1TaxCalculations.ListLineItems(ctx, &stripe.TaxCalculationListLineItemsParams{Calculation: stripe.String(calc.ID)}) {
			if lerr != nil {
				return p.handleFailure(ctx, req, "api_error", fmt.Errorf("stripe tax: list calculation line items: %w", lerr))
			}
			all = append(all, li)
		}
		calc.LineItems.Data, calc.LineItems.HasMore = all, false
	}

	result, err := p.mapResult(calc, req)
	if err != nil {
		return nil, err
	}
	result.RequestRaw = reqRaw
	result.ResponseRaw, _ = json.Marshal(calc)
	return result, nil
}

// handleFailure defers the invoice for retry when Stripe Tax can't
// produce a real calculation. The engine routes the returned error
// into tax_status=pending so the TaxRetrier reconciler picks it up
// on the next scheduler tick. Per ADR-041 (2026-05-30) this is the
// only behavior — the legacy "fallback to ManualProvider" branch
// was cut because it silently produced zero tax when no manual rate
// matched the customer's jurisdiction, overriding the operator's
// intent (charge SOME tax) with a wrong answer (charge zero).
// Operators who genuinely want manual-rate billing set
// tax_provider=manual at the tenant level; mixed Stripe+manual is no
// longer expressible.
func (p *StripeTaxProvider) handleFailure(ctx context.Context, _ Request, reason string, failErr error) (*Result, error) {
	slog.Warn("stripe tax failed, deferring invoice for retry",
		"reason", reason, "error", failErr,
		"livemode", postgres.Livemode(ctx),
	)
	stripeTaxOutcomes.WithLabelValues("deferred", reason).Inc()
	return nil, failErr
}

// Commit creates a tax_transaction from an earlier calculation. Called at
// invoice finalize time, and again by the commit reconciler when the original
// finalize created the Stripe transaction but failed to persist its id locally.
//
// Two dedup layers, both keyed on the invoice:
//   - Reference uniqueness (invoice id): Stripe rejects a second transaction
//     with the same reference, so a duplicate can never be created.
//   - Idempotency-Key (invoice id): on a retry WITHIN Stripe's 24h window,
//     Stripe returns the CACHED response — i.e. the ORIGINAL transaction's id —
//     instead of the reference-already-used error. This is what lets the
//     reconciler RECOVER the transaction id of a commit that succeeded upstream
//     but whose local SetTaxTransaction write failed. (Beyond 24h the
//     calculation itself has expired and the engine's expiry guard blocks the
//     re-commit before it reaches Stripe — so there is never a late duplicate;
//     the invoice just stays uncommitted for manual re-calc.) Same pattern as
//     payment.StripeRefunder and the Reverse path below.
func (p *StripeTaxProvider) Commit(ctx context.Context, calcRef, invoiceID string) (string, error) {
	if calcRef == "" {
		return "", nil
	}
	client := p.clientForCtx(ctx)
	if client == nil {
		// A calculation exists, so Stripe WAS configured when it was made:
		// no client now means the credentials are gone or could not be read.
		// Either way nothing was committed — returning ("", nil) read as
		// success and the commit-retry sweep counted it as recovered while
		// the tax was never reported to Stripe.
		return "", fmt.Errorf("stripe tax: commit calculation %s for invoice %s: no Stripe client for livemode=%t (credentials missing or unreadable)",
			calcRef, invoiceID, postgres.Livemode(ctx))
	}
	params := &stripe.TaxTransactionCreateFromCalculationParams{
		Calculation: stripe.String(calcRef),
		Reference:   stripe.String(invoiceID),
	}
	params.IdempotencyKey = stripe.String("velox_tax_commit_" + invoiceID)
	txn, err := client.V1TaxTransactions.CreateFromCalculation(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe tax: commit calculation %s for invoice %s: %w", calcRef, invoiceID, err)
	}
	return txn.ID, nil
}

// Reverse issues a reversal against a previously committed tax
// transaction. Called from the credit note issue path. The reference is
// derived from the credit note id so a retried issue does not create
// duplicate reversals — Stripe enforces reference uniqueness across all
// transactions in the account.
func (p *StripeTaxProvider) Reverse(ctx context.Context, req ReversalRequest) (*ReversalResult, error) {
	if req.OriginalTransactionID == "" {
		return nil, fmt.Errorf("stripe tax: reverse: original transaction id required")
	}
	ref := req.Reference
	if ref == "" {
		ref = req.CreditNoteID
	}
	if ref == "" {
		return nil, fmt.Errorf("stripe tax: reverse: reference required")
	}
	client := p.clientForCtx(ctx)
	if client == nil {
		return nil, fmt.Errorf("stripe tax: reverse: no client for context (livemode=%t)",
			postgres.Livemode(ctx))
	}
	mode := req.Mode
	if mode == "" {
		mode = ReversalModeFull
	}
	params := &stripe.TaxTransactionCreateReversalParams{
		OriginalTransaction: stripe.String(req.OriginalTransactionID),
		Reference:           stripe.String(ref),
		Mode:                stripe.String(string(mode)),
	}
	if mode == ReversalModePartial {
		if req.GrossAmountCents <= 0 {
			return nil, fmt.Errorf("stripe tax: reverse: partial mode requires positive gross amount")
		}
		// Stripe requires the amount in negative smallest-currency units.
		params.FlatAmount = stripe.Int64(-req.GrossAmountCents)
	}
	// Defense-in-depth idempotency: Reference uniqueness gives Stripe-
	// side semantic dedup, but a transient network failure between
	// Stripe accepting and the SDK returning could otherwise leave the
	// caller in retry-with-unknown-state. Idempotency-Key on the
	// request makes Stripe return the cached response on retry. Same
	// pattern as payment.StripeRefunder. Key shape derived from the
	// reference so retries of the same logical reversal converge.
	params.IdempotencyKey = stripe.String("velox_tax_rev_" + ref)
	txn, err := client.V1TaxTransactions.CreateReversal(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("stripe tax: reverse transaction %s for credit note %s: %w",
			req.OriginalTransactionID, req.CreditNoteID, err)
	}
	return &ReversalResult{TransactionID: txn.ID}, nil
}

func (p *StripeTaxProvider) buildParams(req Request) *stripe.TaxCalculationCreateParams {
	taxBehavior := "exclusive"
	if req.TaxInclusive {
		taxBehavior = "inclusive"
	}

	lineItems := make([]*stripe.TaxCalculationCreateLineItemParams, len(req.LineItems))
	for i, li := range req.LineItems {
		ref := li.Ref
		if ref == "" {
			ref = fmt.Sprintf("line_%d", i)
		}
		code := li.TaxCode
		if code == "" {
			code = req.DefaultTaxCode
		}
		params := &stripe.TaxCalculationCreateLineItemParams{
			Amount:      stripe.Int64(li.AmountCents),
			Quantity:    stripe.Int64(max(li.Quantity, 1)),
			Reference:   stripe.String(ref),
			TaxBehavior: stripe.String(taxBehavior),
		}
		if code != "" {
			params.TaxCode = stripe.String(code)
		}
		lineItems[i] = params
	}

	params := &stripe.TaxCalculationCreateParams{
		Currency: stripe.String(strings.ToLower(req.Currency)),
		CustomerDetails: &stripe.TaxCalculationCreateCustomerDetailsParams{
			AddressSource: stripe.String("billing"),
			Address: &stripe.AddressParams{
				Country:    stripe.String(req.CustomerAddress.Country),
				PostalCode: stripe.String(req.CustomerAddress.PostalCode),
			},
		},
		LineItems: lineItems,
	}
	if req.CustomerAddress.Line1 != "" {
		params.CustomerDetails.Address.Line1 = stripe.String(req.CustomerAddress.Line1)
	}
	if req.CustomerAddress.Line2 != "" {
		params.CustomerDetails.Address.Line2 = stripe.String(req.CustomerAddress.Line2)
	}
	if req.CustomerAddress.City != "" {
		params.CustomerDetails.Address.City = stripe.String(req.CustomerAddress.City)
	}
	if req.CustomerAddress.State != "" {
		params.CustomerDetails.Address.State = stripe.String(req.CustomerAddress.State)
	}

	// Tax IDs enable reverse-charge validation and B2B VAT exemption. When
	// present we attach them so Stripe can flip the calculation to
	// reverse-charge on its own, in addition to our explicit StatusReverseCharge
	// routing upstream.
	if req.CustomerTaxID != "" && req.CustomerTaxIDType != "" {
		params.CustomerDetails.TaxIDs = []*stripe.TaxCalculationCreateCustomerDetailsTaxIDParams{{
			Type:  stripe.String(req.CustomerTaxIDType),
			Value: stripe.String(req.CustomerTaxID),
		}}
	}

	// Expand line_items for the per-line amount_tax. We deliberately do NOT
	// expand line_items.data.tax_breakdown: Stripe leaves the per-line breakdown
	// null for single-rate calcs regardless, and mapResult seeds each line's
	// rate + jurisdiction from the document-level tax_breakdown instead (see
	// docRate there). Revisit if true per-line rates on genuinely
	// multi-jurisdiction invoices become a requirement.
	params.AddExpand("line_items")
	return params
}

func (p *StripeTaxProvider) mapResult(calc *stripe.TaxCalculation, req Request) (*Result, error) {
	totalTax := calc.TaxAmountExclusive
	if req.TaxInclusive {
		totalTax = calc.TaxAmountInclusive
	}

	subtotal := int64(0)
	for _, li := range req.LineItems {
		subtotal += li.AmountCents
	}
	effectiveRate := float64(0)
	if subtotal > 0 {
		// Precise effective rate as percent (4-decimal precision).
		// ADR-042/043 stored as the only rate column.
		effectiveRate = float64(totalTax) * 100 / float64(subtotal)
	}

	taxName := ""
	taxCountry := ""
	reverseCharge := false
	lastReason := ""
	var breakdowns []Breakdown
	for _, tb := range calc.TaxBreakdown {
		if tb == nil {
			continue
		}
		name := ""
		rate := float64(0)
		juris := ""
		if tb.TaxRateDetails != nil {
			// Document-level breakdown has no display_name (stripe-go v82), so
			// map the tax_type enum to a clean label.
			name = taxTypeDisplayName("", string(tb.TaxRateDetails.TaxType))
			if tb.TaxRateDetails.Country != "" {
				juris = tb.TaxRateDetails.Country
				if tb.TaxRateDetails.State != "" {
					juris = tb.TaxRateDetails.Country + "-" + tb.TaxRateDetails.State
				}
				if taxCountry == "" {
					taxCountry = tb.TaxRateDetails.Country
				}
			}
			if pct := tb.TaxRateDetails.PercentageDecimal; pct != "" {
				rate = parseStripeRate(pct)
			}
		}
		if taxName == "" {
			taxName = name
		}
		if tb.TaxabilityReason == "reverse_charge" {
			reverseCharge = true
		}
		lastReason = string(tb.TaxabilityReason)
		breakdowns = append(breakdowns, Breakdown{
			Jurisdiction: juris,
			Name:         name,
			Rate:         rate,
			AmountCents:  tb.Amount,
		})
	}

	// Single document-level rate, for the per-line fallback below. Stripe puts
	// the verbatim percentage_decimal + jurisdiction in the DOCUMENT-level
	// tax_breakdown; the PER-LINE tax_breakdown is frequently null (Stripe only
	// populates it when line_items.data.tax_breakdown is expanded, and not
	// reliably even then). When there is exactly one document-level rate it
	// applies uniformly to every taxed line, so we seed the line from it —
	// otherwise the line falls back to the rounded effectiveRate with an empty
	// jurisdiction, silently dropping Stripe's true rate (e.g. NYC 8.8750
	// displayed as 8.88, jurisdiction lost). Mixed-rate invoices (len > 1) can't
	// attribute one rate per line here and still rely on the per-line breakdown.
	docRate, docJuris, docReason := float64(0), "", ""
	if len(breakdowns) == 1 {
		docRate, docJuris, docReason = breakdowns[0].Rate, breakdowns[0].Jurisdiction, lastReason
	}

	// Map per-line results back to input Ref so the engine matches them to
	// its own line items independent of Stripe's ordering.
	lines := make([]ResultLine, len(req.LineItems))
	for i, li := range req.LineItems {
		lines[i] = ResultLine{
			Ref:            li.Ref,
			NetAmountCents: li.AmountCents, // default; overwritten for inclusive below
			TaxRate:        effectiveRate,
			TaxName:        taxName,
		}
	}
	indexByRef := make(map[string]int, len(req.LineItems))
	for i, li := range req.LineItems {
		indexByRef[li.Ref] = i
	}
	// Every requested line must come back mapped. A line left unmapped (an
	// unrecognised reference, or a truncated list) would keep zero tax while the
	// invoice total includes it, so per-line tax would not sum to the total and
	// a credit note's per-line reversal would under-reverse. Fail instead.
	mapped := make([]bool, len(lines))
	if calc.LineItems != nil {
		for _, sli := range calc.LineItems.Data {
			if sli == nil {
				continue
			}
			idx, ok := indexByRef[sli.Reference]
			if !ok {
				idx = parseLineRef(sli.Reference)
			}
			if idx < 0 || idx >= len(lines) {
				continue // unrecognised reference: its requested line stays unmapped and fails below
			}
			mapped[idx] = true
			lines[idx].TaxAmountCents = sli.AmountTax
			if req.TaxInclusive {
				// In inclusive mode Stripe returns the gross sent in as Amount
				// and the carved tax. amount_tax + net == amount; derive net.
				lines[idx].NetAmountCents = sli.Amount - sli.AmountTax
			}
			if len(sli.TaxBreakdown) > 0 {
				bd := sli.TaxBreakdown[0]
				if bd.TaxRateDetails != nil {
					if n := taxTypeDisplayName(bd.TaxRateDetails.DisplayName, string(bd.TaxRateDetails.TaxType)); n != "" {
						lines[idx].TaxName = n
					}
					if bd.TaxRateDetails.PercentageDecimal != "" {
						lines[idx].TaxRate = parseStripeRate(bd.TaxRateDetails.PercentageDecimal)
					}
				}
				if bd.Jurisdiction != nil && bd.Jurisdiction.Country != "" {
					if bd.Jurisdiction.State != "" {
						lines[idx].Jurisdiction = bd.Jurisdiction.Country + "-" + bd.Jurisdiction.State
					} else {
						lines[idx].Jurisdiction = bd.Jurisdiction.Country
					}
				}
				// Persist Stripe's structured taxability_reason so the PDF and
				// dashboard can distinguish two zero-tax outcomes that read
				// identically on the totals row but require different legends:
				// reverse_charge needs the EU Art. 196 disclosure, while
				// not_collecting (merchant has no registration in this
				// jurisdiction) needs no legend at all. Treated as an opaque
				// string — Stripe may add new reasons over time.
				lines[idx].TaxabilityReason = string(bd.TaxabilityReason)
			} else {
				if docRate > 0 && sli.AmountTax != 0 {
					// Per-line breakdown absent (see docRate note above): seed this
					// taxed line from the single document-level rate so Stripe's
					// verbatim percentage_decimal + jurisdiction are preserved
					// instead of the rounded effectiveRate / empty jurisdiction.
					lines[idx].TaxRate = docRate
					if docJuris != "" {
						lines[idx].Jurisdiction = docJuris
					}
				}
				// The reason copies INDEPENDENT of the taxed-line gate above:
				// zero-tax reasons (not_collecting, zero-rated) carry rate 0 and
				// amount_tax 0 by definition, so gating them on docRate>0 &&
				// AmountTax!=0 meant the one class the field exists to
				// disambiguate never round-tripped (found live: a GB customer on
				// an unregistered account returned doc-level not_collecting but
				// the line's tax_reason stayed empty — FLOW B10 leg 10).
				if docReason != "" {
					lines[idx].TaxabilityReason = docReason
				}
			}
			if li := req.LineItems[idx]; li.TaxCode != "" {
				lines[idx].TaxCode = li.TaxCode
			} else if req.DefaultTaxCode != "" {
				lines[idx].TaxCode = req.DefaultTaxCode
			}
		}
	}
	for i, ok := range mapped {
		if !ok {
			return nil, fmt.Errorf("stripe tax: calculation %s returned no line for requested line %q (%d of %d mapped)", calc.ID, lines[i].Ref, countTrue(mapped), len(mapped))
		}
	}

	return &Result{
		Provider:      "stripe_tax",
		CalculationID: calc.ID,
		TotalTaxCents: totalTax,
		EffectiveRate: effectiveRate,
		TaxName:       taxName,
		TaxCountry:    taxCountry,
		ReverseCharge: reverseCharge,
		Lines:         lines,
		Breakdowns:    breakdowns,
	}, nil
}

// parseStripeRate parses a Stripe Tax `percentage_decimal` string (e.g.
// "8.875") into the precise percent rate as float64. Stripe documents
// this field as a string precisely to avoid lossy float round-trip;
// Velox stores it verbatim into the tax_rate NUMERIC(7,4) column per
// ADR-042/043. Returns 0 on parse failure (caller logs at the call
// site).
func parseStripeRate(pct string) float64 {
	if pct == "" {
		return 0
	}
	v, err := strconv.ParseFloat(pct, 64)
	if err != nil {
		return 0
	}
	return v
}

// stripeTaxTypeNames maps Stripe Tax's tax_type enum to a clean, customer-
// facing label. Stripe returns the raw snake_case enum (e.g. "sales_tax") in
// tax_rate_details.tax_type; rendered verbatim the invoice tax line reads
// "sales_tax (8.875%)". The document-level tax_breakdown carries NO
// display_name (only the per-line and shipping breakdowns do — stripe-go v82),
// so for the common case (per-line breakdown null → name taken from the
// document level) this map is the only source of a readable label. Covers all
// 15 TaxCalculationTaxBreakdownTaxRateDetailsTaxType values; acronyms
// uppercased, words title-cased.
var stripeTaxTypeNames = map[string]string{
	"amusement_tax":       "Amusement Tax",
	"communications_tax":  "Communications Tax",
	"gst":                 "GST",
	"hst":                 "HST",
	"igst":                "IGST",
	"jct":                 "JCT",
	"lease_tax":           "Lease Tax",
	"pst":                 "PST",
	"qst":                 "QST",
	"retail_delivery_fee": "Retail Delivery Fee",
	"rst":                 "RST",
	"sales_tax":           "Sales Tax",
	"service_tax":         "Service Tax",
	"vat":                 "VAT",
}

// taxTypeDisplayName resolves the customer-facing tax label. It prefers
// Stripe's own display_name when present (per-line / shipping breakdowns carry
// a localized one, e.g. "Value-added tax (VAT)"), then the verified enum map,
// then a graceful title-case of any future enum Stripe adds before we map it —
// so a tax line never leaks a raw snake_case token. Returns "" only when both
// inputs are empty (caller leaves the field unset).
func taxTypeDisplayName(displayName, taxType string) string {
	if displayName != "" {
		return displayName
	}
	if n, ok := stripeTaxTypeNames[taxType]; ok {
		return n
	}
	if taxType == "" {
		return ""
	}
	parts := strings.Split(taxType, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// parseLineRef extracts the index from a reference like "line_2". Used when
// a Stripe response's line reference doesn't match any request Ref (defensive
// fallback; Stripe echoes references verbatim in practice).
func parseLineRef(ref string) int {
	if !strings.HasPrefix(ref, "line_") {
		return -1
	}
	n, err := strconv.Atoi(ref[5:])
	if err != nil {
		return -1
	}
	return n
}

func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}
