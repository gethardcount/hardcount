package tax

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// ManualProvider applies a single tenant-configured tax rate (basis points)
// uniformly across every line item. Deliberately simple: no jurisdiction
// lookup, no per-product tax codes, no cross-border auto zero-rating.
// Tenants who need jurisdictional accuracy pick stripe_tax.
//
// Inclusive vs exclusive: honoured via Request.TaxInclusive. In exclusive
// mode the line amount is net and tax is added on top; in inclusive mode
// the line amount is gross and tax is carved out so subtotal + tax = gross.
//
// Exempt / reverse-charge routing lives one layer up: Calculate honours
// req.CustomerStatus and returns a zeroed Result with the appropriate flag
// set, so the invoice PDF renders the right legend regardless of provider.
//
// References for the flat-rate shape: Stripe's legacy tax_rates object,
// Lago's integration-agnostic tax model, Chargebee's manual tax rules. All
// three concluded that the only safe manual behaviour is "apply what the
// tenant configured" — inferring cross-border zero-rating silently
// produces wrong invoices under OSS/OIDAR and in most US sales-tax regimes.
type ManualProvider struct {
	rate    float64 // Percent rate (4-decimal precision). ADR-042/043.
	ratePPM int64   // Same rate scaled to parts-per-million (1 ppm = 0.0001%) for integer math without precision loss.
	taxName string
}

// NewManualProvider returns a provider that applies rate uniformly.
// rate is in percent (4-decimal precision; e.g. 8.875 for NYC).
// taxName is the label shown on the invoice tax row ("VAT", "Sales Tax", …).
// ADR-042/043: switched from integer basis-points to NUMERIC(7,4) percent.
// The provider stores BOTH the float (for ResultLine.TaxRate display) and
// the integer ppm scaling (for line-tax math without float drift).
func NewManualProvider(rate float64, taxName string) *ManualProvider {
	if rate < 0 {
		rate = 0
	}
	return &ManualProvider{
		rate:    rate,
		ratePPM: int64(math.Round(rate * 10000)), // 8.875% → 88750 ppm
		taxName: taxName,
	}
}

func (*ManualProvider) Name() string { return "manual" }

func (m *ManualProvider) Calculate(_ context.Context, req Request) (*Result, error) {
	switch req.CustomerStatus {
	case StatusExempt:
		return exemptResult("manual", req, false, req.CustomerExemptReason), nil
	case StatusReverseCharge:
		return exemptResult("manual", req, true, ""), nil
	}

	// A negative line amount is out of contract, and the failure it used to
	// produce was silent: the per-line base is clamped at zero, so the clamped
	// line's share drops out of the apportionment while the invoice-level total
	// still counts it. The residual then exceeds the one-cent-per-line the
	// largest-remainder loop can move, and the result is line taxes that miss
	// the total by an arbitrary amount AND a negative tax line — e.g. lines of
	// +100.00 and −30.00 at 20% return a 14.00 total against line taxes of
	// [19.99, −0.01].
	//
	// billing.collapseTaxRequestLines is the chokepoint that upholds the
	// invariant (issue #556). This is not a second guard for the same failure:
	// it converts the remaining silent-wrong-invoice mode into a loud one for
	// any caller that reaches a provider directly.
	for i, li := range req.LineItems {
		if li.AmountCents < 0 {
			return nil, fmt.Errorf("tax: line %d (%q) has a negative amount (%d) — tax providers must be given net non-negative lines", i, li.Ref, li.AmountCents)
		}
	}

	lines := make([]ResultLine, len(req.LineItems))
	if m.ratePPM <= 0 || len(req.LineItems) == 0 {
		for i, li := range req.LineItems {
			lines[i] = ResultLine{Ref: li.Ref, NetAmountCents: li.AmountCents}
		}
		return &Result{Provider: "manual", Lines: lines}, nil
	}

	// Amounts are int64 cents. Every product below (amount × rate in ppm,
	// discount × amount) is worked out in 128 bits by money.MulDiv*, so a
	// tax is exact for any line amount. The rate is 0 to 100% (tenant
	// settings), so a tax or net never exceeds its base; only the lines' sum
	// can be too large, and that is an error, never a wrapped number.
	if m.ratePPM > 1_000_000 {
		return nil, fmt.Errorf("tax: manual rate %g%% is over 100%%", m.rate)
	}
	subtotal := int64(0)
	for _, li := range req.LineItems {
		var err error
		if subtotal, err = domain.AddCents(subtotal, li.AmountCents); err != nil {
			return nil, fmt.Errorf("tax: the line amounts sum past int64 cents: %w", err)
		}
	}

	if req.TaxInclusive {
		return m.calculateInclusive(req, lines, subtotal)
	}
	return m.calculateExclusive(req, lines, subtotal)
}

// errTaxOverflow is a product the 128-bit math cannot fit back into int64.
// The checks above make it unreachable; it is returned, not ignored, so a
// change that breaks them fails loudly.
var errTaxOverflow = fmt.Errorf("tax: an amount does not fit int64 cents: %w", domain.ErrAmountOverflow)

// discountShares spreads discount over the lines in proportion to their
// amounts (the last line takes the remainder) and returns each line's base
// after its share.
func discountShares(lineItems []RequestLine, discount, subtotal int64) ([]int64, error) {
	bases := make([]int64, len(lineItems))
	var discountApplied int64
	for i, li := range lineItems {
		var linePortion int64
		if i == len(lineItems)-1 {
			linePortion = discount - discountApplied
		} else {
			// discount <= subtotal, so the share is at most the line amount.
			p, ok := money.MulDivRoundHalfToEven(discount, li.AmountCents, subtotal)
			if !ok {
				return nil, errTaxOverflow
			}
			linePortion = p
			discountApplied += linePortion
		}
		bases[i] = max(li.AmountCents-linePortion, 0)
	}
	return bases, nil
}

// calculateExclusive: line amounts are net; tax is added on top.
// Discount is distributed proportionally across lines before rate is applied.
func (m *ManualProvider) calculateExclusive(req Request, lines []ResultLine, subtotal int64) (*Result, error) {
	discount := min(max(req.DiscountCents, 0), subtotal)
	taxableBase := subtotal - discount
	if taxableBase <= 0 {
		for i, li := range req.LineItems {
			lines[i] = ResultLine{Ref: li.Ref, NetAmountCents: li.AmountCents}
		}
		return &Result{Provider: "manual", Lines: lines}, nil
	}

	// Integer math via parts-per-million (1 ppm = 0.0001%). 8.875%
	// = 88750 ppm; tax = base × ppm / 1_000_000. Preserves 4-decimal
	// precision without float drift.
	totalTax, ok := money.MulDivRoundHalfToEven(taxableBase, m.ratePPM, 1_000_000)
	if !ok {
		return nil, errTaxOverflow
	}

	// Per-line taxable base after proportional discount. The exact (unrounded)
	// per-line tax is base × ppm / 1_000_000; we hand those exact shares to
	// largest-remainder apportionment so the rounded line taxes sum to totalTax.
	bases, err := discountShares(req.LineItems, discount, subtotal)
	if err != nil {
		return nil, err
	}
	lineTaxes, ok := distributeLargestRemainder(totalTax, bases, m.ratePPM, 1_000_000)
	if !ok {
		return nil, errTaxOverflow
	}

	for i, li := range req.LineItems {
		lines[i] = ResultLine{
			Ref:            li.Ref,
			NetAmountCents: li.AmountCents,
			TaxAmountCents: lineTaxes[i],
			TaxRate:        m.rate, // ADR-042/043
			TaxName:        m.taxName,
		}
	}

	return m.wrap(totalTax, lines), nil
}

// calculateInclusive: line amounts are gross (tax-inclusive). Net and tax
// are carved out per line so engine-side subtotal == sum(Net) and
// subtotal + tax == sum(original gross).
//
// Discount is applied in GROSS terms (matches the semantics tenants expect
// from inclusive pricing: "10% off sticker"). Net and tax are derived from
// the post-discount gross base.
func (m *ManualProvider) calculateInclusive(req Request, lines []ResultLine, subtotal int64) (*Result, error) {
	discount := min(max(req.DiscountCents, 0), subtotal)
	taxableGross := subtotal - discount
	if taxableGross <= 0 {
		for i, li := range req.LineItems {
			lines[i] = ResultLine{Ref: li.Ref, NetAmountCents: li.AmountCents}
		}
		return &Result{Provider: "manual", Lines: lines}, nil
	}

	// Inclusive math: gross = net × (1 + rate). With rate in ppm
	// scaled to 1_000_000-base, denom = 1_000_000 + ratePPM.
	denom := int64(1_000_000 + m.ratePPM)
	taxableNet, ok := money.MulDivRoundHalfToEven(taxableGross, 1_000_000, denom)
	if !ok {
		return nil, errTaxOverflow
	}
	totalTax := taxableGross - taxableNet

	// Exact per-line inclusive tax is grossBase × ratePPM / denom; feed those
	// exact shares to largest-remainder apportionment so rounded line taxes sum
	// to totalTax.
	bases, err := discountShares(req.LineItems, discount, subtotal)
	if err != nil {
		return nil, err
	}
	lineTaxes, ok := distributeLargestRemainder(totalTax, bases, m.ratePPM, denom)
	if !ok {
		return nil, errTaxOverflow
	}

	for i, li := range req.LineItems {
		// Full-line (undiscounted) net for NetAmountCents so the engine's
		// stored subtotal is undiscounted-net and DiscountCents is net-units,
		// matching what the invoice shows.
		lineNetUndisc, ok := money.MulDivRoundHalfToEven(li.AmountCents, 1_000_000, denom)
		if !ok {
			return nil, errTaxOverflow
		}

		lines[i] = ResultLine{
			Ref:            li.Ref,
			NetAmountCents: lineNetUndisc,
			TaxAmountCents: lineTaxes[i],
			TaxRate:        m.rate, // ADR-042/043
			TaxName:        m.taxName,
		}
	}

	return m.wrap(totalTax, lines), nil
}

func (m *ManualProvider) wrap(totalTax int64, lines []ResultLine) *Result {
	return &Result{
		Provider:      "manual",
		TotalTaxCents: totalTax,
		EffectiveRate: m.rate, // ADR-042/043
		TaxName:       m.taxName,
		Lines:         lines,
		Breakdowns: []Breakdown{{
			Name:        m.taxName,
			Rate:        m.rate,
			AmountCents: totalTax,
		}},
	}
}

// distributeLargestRemainder allocates total across len(bases) lines using the
// largest-remainder (a.k.a. minimum-distortion) method. Each line's exact share
// is bases[i]×mul/den (worked out in 128 bits); it receives the share's floor,
// then the leftover cents (total − Σfloor) are handed out one at a time to the
// lines with the largest fractional remainders, ties broken by lowest index.
// ok is false when a share does not fit int64.
//
// This is the residual-distribution rule every reference indirect-tax engine
// converges on (Sovos: "split among the lines that have the highest remainder…
// added to the first line" on ties; Avalara / Dynamics 365: "the line that
// results in the minimum percentage change"). It guarantees Σ(line tax) == total
// while never docking a larger-base line below a smaller-base one — the
// inversion the previous "dump the residual on the positionally-last line"
// shortcut produced when the residual was negative.
func distributeLargestRemainder(total int64, bases []int64, mul, den int64) ([]int64, bool) {
	out := make([]int64, len(bases))
	if len(bases) == 0 || den <= 0 {
		return out, true
	}

	type rem struct {
		idx       int
		remainder int64
	}
	rems := make([]rem, len(bases))
	var allocated int64
	for i, b := range bases {
		q, r, ok := money.MulDivRem(max(b, 0), max(mul, 0), den)
		if !ok {
			return nil, false
		}
		out[i] = q
		rems[i] = rem{idx: i, remainder: r}
		allocated += out[i]
	}

	// leftover is mathematically in [0, len(bases)] (Σfloor ≤ floor(Σ) ≤ total);
	// the clawback branch is defensive against an upstream total that doesn't
	// match the supplied shares.
	leftover := total - allocated
	if leftover > 0 {
		sort.SliceStable(rems, func(a, b int) bool {
			if rems[a].remainder != rems[b].remainder {
				return rems[a].remainder > rems[b].remainder // largest remainder first
			}
			return rems[a].idx < rems[b].idx // tie → lowest index
		})
		for k := int64(0); k < leftover && int(k) < len(rems); k++ {
			out[rems[k].idx]++
		}
	} else if leftover < 0 {
		sort.SliceStable(rems, func(a, b int) bool {
			if rems[a].remainder != rems[b].remainder {
				return rems[a].remainder < rems[b].remainder // smallest remainder first
			}
			return rems[a].idx < rems[b].idx
		})
		for k := int64(0); k < -leftover && int(k) < len(rems); k++ {
			out[rems[k].idx]--
		}
	}

	return out, true
}

func (*ManualProvider) Commit(_ context.Context, _, _ string) (string, error) { return "", nil }

// Reverse is a no-op — manual providers have no upstream tax_transaction
// to reverse. The credit note flow treats an empty ReversalResult.
// TransactionID as "nothing to record" and proceeds.
func (*ManualProvider) Reverse(_ context.Context, _ ReversalRequest) (*ReversalResult, error) {
	return &ReversalResult{}, nil
}
