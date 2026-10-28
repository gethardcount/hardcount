package tax

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestNoneProvider verifies the zero-tax backend: names itself, returns one
// zero-tax line per input, Commit is a silent no-op. Covers the contract the
// billing engine relies on so tenants on tax_provider='none' never hit an
// unexpected error path.
func TestNoneProvider(t *testing.T) {
	p := NewNoneProvider()
	if p.Name() != "none" {
		t.Fatalf("Name() = %q, want none", p.Name())
	}

	req := Request{LineItems: []RequestLine{
		{Ref: "line_0", AmountCents: 1000},
		{Ref: "line_1", AmountCents: 2500},
	}}
	res, err := p.Calculate(context.Background(), req)
	if err != nil {
		t.Fatalf("Calculate: unexpected error: %v", err)
	}
	if res.Provider != "none" {
		t.Errorf("Provider = %q, want none", res.Provider)
	}
	if res.TotalTaxCents != 0 {
		t.Errorf("TotalTaxCents = %d, want 0", res.TotalTaxCents)
	}
	if len(res.Lines) != 2 {
		t.Fatalf("Lines len = %d, want 2", len(res.Lines))
	}
	if res.Lines[0].NetAmountCents != 1000 || res.Lines[1].NetAmountCents != 2500 {
		t.Errorf("Lines net mismatch: %+v", res.Lines)
	}
	for i, l := range res.Lines {
		if l.TaxAmountCents != 0 {
			t.Errorf("line %d tax = %d, want 0", i, l.TaxAmountCents)
		}
	}
	txID, err := p.Commit(context.Background(), "calc_1", "inv_1")
	if err != nil {
		t.Errorf("Commit: unexpected error: %v", err)
	}
	if txID != "" {
		t.Errorf("NoneProvider.Commit should return empty transaction id, got %q", txID)
	}

	// Reverse must also be a no-op that returns an empty ReversalResult so
	// the credit note flow can call it without provider-type branching.
	rev, err := p.Reverse(context.Background(), ReversalRequest{
		OriginalTransactionID: "tx_xxx", CreditNoteID: "cn_1",
		Mode: ReversalModeFull,
	})
	if err != nil {
		t.Errorf("Reverse: unexpected error: %v", err)
	}
	if rev == nil || rev.TransactionID != "" {
		t.Errorf("NoneProvider.Reverse should return empty result, got %+v", rev)
	}
}

// TestManualProvider_Exclusive covers the default flat-rate exclusive path:
// tax added on top of the net subtotal, per-line rounding reconciled to the
// jurisdiction-level total. Also checks the Breakdown row the PDF relies on
// for the aggregate tax label.
func TestManualProvider_Exclusive(t *testing.T) {
	p := NewManualProvider(18.00, "GST") // 18%

	req := Request{
		Currency: "INR",
		LineItems: []RequestLine{
			{Ref: "line_0", AmountCents: 10000},
			{Ref: "line_1", AmountCents: 5000},
		},
	}
	res, err := p.Calculate(context.Background(), req)
	if err != nil {
		t.Fatalf("Calculate: unexpected error: %v", err)
	}
	if res.Provider != "manual" {
		t.Errorf("Provider = %q, want manual", res.Provider)
	}
	wantTotal := int64(2700) // 18% of 15000
	if res.TotalTaxCents != wantTotal {
		t.Errorf("TotalTaxCents = %d, want %d", res.TotalTaxCents, wantTotal)
	}
	if res.EffectiveRate != 18.00 {
		t.Errorf("EffectiveRate = %g, want 1800", res.EffectiveRate)
	}
	if res.TaxName != "GST" {
		t.Errorf("TaxName = %q, want GST", res.TaxName)
	}
	if len(res.Breakdowns) != 1 || res.Breakdowns[0].AmountCents != wantTotal {
		t.Errorf("Breakdowns = %+v", res.Breakdowns)
	}

	sumTax := int64(0)
	for _, l := range res.Lines {
		sumTax += l.TaxAmountCents
	}
	if sumTax != wantTotal {
		t.Errorf("sum(line tax) = %d, want %d (rounding reconciliation failed)", sumTax, wantTotal)
	}
}

// TestManualProvider_LargestRemainderNoInversion is the regression for the
// "biggest line, least tax" artifact. Three near-equal lines
// ($33.33/$33.33/$33.34) at 7.25% produce a −1¢ residual: naive per-line
// rounding sums to 726¢ but the exact document tax on $100.00 is 725¢. The old
// code dumped the −1¢ on the positionally-last line (the $33.34 one), leaving
// it taxed *less* than its smaller peers. Largest-remainder apportionment must
// instead dock a line whose remainder is smallest, never inverting base order.
func TestManualProvider_LargestRemainderNoInversion(t *testing.T) {
	p := NewManualProvider(7.25, "Sales Tax")

	req := Request{
		Currency: "USD",
		LineItems: []RequestLine{
			{Ref: "line_0", AmountCents: 3333},
			{Ref: "line_1", AmountCents: 3333},
			{Ref: "line_2", AmountCents: 3334},
		},
	}
	res, err := p.Calculate(context.Background(), req)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}

	const wantTotal = int64(725) // 10000¢ × 7.25% = 725.00¢ exactly
	if res.TotalTaxCents != wantTotal {
		t.Errorf("TotalTaxCents = %d, want %d", res.TotalTaxCents, wantTotal)
	}

	var sumTax int64
	for _, l := range res.Lines {
		sumTax += l.TaxAmountCents
	}
	if sumTax != wantTotal {
		t.Errorf("sum(line tax) = %d, want %d", sumTax, wantTotal)
	}

	// No inversion: a line with a >= base must not carry < tax.
	for i := range res.Lines {
		for j := range res.Lines {
			if req.LineItems[i].AmountCents > req.LineItems[j].AmountCents &&
				res.Lines[i].TaxAmountCents < res.Lines[j].TaxAmountCents {
				t.Errorf("inversion: line %d (base %d) taxed %d < line %d (base %d) taxed %d",
					i, req.LineItems[i].AmountCents, res.Lines[i].TaxAmountCents,
					j, req.LineItems[j].AmountCents, res.Lines[j].TaxAmountCents)
			}
		}
	}

	// Largest base ($33.34, largest fractional remainder) must round up to 242.
	if got := res.Lines[2].TaxAmountCents; got != 242 {
		t.Errorf("line_2 ($33.34) tax = %d, want 242 (it should not absorb the −1¢)", got)
	}
}

// TestDistributeLargestRemainder_Property fuzzes the apportionment invariants
// across line counts, rates, bases, AND both exclusive and inclusive modes: the
// per-line taxes always sum to the provider's reported total, and base order is
// never inverted (a larger line never carries less tax than a smaller one). The
// inclusive arm is the regression guard for the inclusive path, which otherwise
// only has single-line coverage.
func TestDistributeLargestRemainder_Property(t *testing.T) {
	rates := []float64{7.25, 18.0, 8.875, 5.0, 9.999}
	bases := [][]int64{
		{3333, 3333, 3334},
		{1, 1, 1, 1, 1},
		{999999, 1, 50000, 7},
		{100, 200, 300, 400},
		{1234, 5678, 999, 4321, 17},
	}
	for _, inclusive := range []bool{false, true} {
		for _, rate := range rates {
			p := NewManualProvider(rate, "T")
			for _, bs := range bases {
				lines := make([]RequestLine, len(bs))
				for i, b := range bs {
					lines[i] = RequestLine{Ref: "l", AmountCents: b}
				}
				res, err := p.Calculate(context.Background(),
					Request{Currency: "USD", TaxInclusive: inclusive, LineItems: lines})
				if err != nil {
					t.Fatalf("Calculate: %v", err)
				}
				var sum int64
				for _, l := range res.Lines {
					sum += l.TaxAmountCents
				}
				if sum != res.TotalTaxCents {
					t.Errorf("incl=%v rate=%g bases=%v: sum(line)=%d != total=%d",
						inclusive, rate, bs, sum, res.TotalTaxCents)
				}
				for i := range res.Lines {
					for j := range res.Lines {
						if bs[i] > bs[j] && res.Lines[i].TaxAmountCents < res.Lines[j].TaxAmountCents {
							t.Errorf("incl=%v rate=%g bases=%v: inversion line %d<%d",
								inclusive, rate, bs, i, j)
						}
					}
				}
			}
		}
	}
}

// TestManualProvider_Inclusive verifies gross → net carve-out: the engine's
// subtotal invariant depends on sum(Net) + tax == sum(original gross).
func TestManualProvider_Inclusive(t *testing.T) {
	p := NewManualProvider(20.00, "VAT") // 20%

	req := Request{
		TaxInclusive: true,
		LineItems:    []RequestLine{{Ref: "line_0", AmountCents: 12000}},
	}
	res, err := p.Calculate(context.Background(), req)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// 12000 gross at 20% inclusive → 10000 net + 2000 tax
	if res.TotalTaxCents != 2000 {
		t.Errorf("TotalTaxCents = %d, want 2000", res.TotalTaxCents)
	}
	if res.Lines[0].NetAmountCents != 10000 {
		t.Errorf("NetAmountCents = %d, want 10000", res.Lines[0].NetAmountCents)
	}
	if res.Lines[0].TaxAmountCents+res.Lines[0].NetAmountCents != 12000 {
		t.Errorf("net+tax != gross: %+v", res.Lines[0])
	}
}

// TestManualProvider_ZeroRate: when the tenant has tax_rate_bp=0 (e.g. no
// tax collected in their jurisdiction) every line gets zero tax and the
// provider returns empty breakdowns — the PDF then skips the tax row.
func TestManualProvider_ZeroRate(t *testing.T) {
	p := NewManualProvider(0, "")

	req := Request{LineItems: []RequestLine{{Ref: "line_0", AmountCents: 5000}}}
	res, err := p.Calculate(context.Background(), req)
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.TotalTaxCents != 0 {
		t.Errorf("TotalTaxCents = %d, want 0", res.TotalTaxCents)
	}
	if len(res.Breakdowns) != 0 {
		t.Errorf("Breakdowns = %+v, want empty", res.Breakdowns)
	}
}

// TestManualProvider_ReverseIsNoOp: manual providers have no upstream
// tax_transaction to reverse, so Reverse must return an empty result
// with no error — the credit note flow should treat that as a no-op
// without special-casing provider names.
func TestManualProvider_ReverseIsNoOp(t *testing.T) {
	p := NewManualProvider(18.00, "GST")

	rev, err := p.Reverse(context.Background(), ReversalRequest{
		OriginalTransactionID: "tx_xxx", CreditNoteID: "cn_1",
		Mode: ReversalModeFull,
	})
	if err != nil {
		t.Fatalf("Reverse: unexpected error: %v", err)
	}
	if rev == nil || rev.TransactionID != "" {
		t.Errorf("ManualProvider.Reverse should return empty result, got %+v", rev)
	}
}

// TestManualProvider_Exempt: when the customer is exempt, Calculate returns
// a zeroed Result with Exempt=true and the reason propagated to the PDF
// audit snapshot. Reverse-charge takes the same path but sets ReverseCharge
// instead of Exempt so the PDF can render the legally required legend.
func TestManualProvider_ExemptStatuses(t *testing.T) {
	p := NewManualProvider(18.00, "GST")

	t.Run("exempt", func(t *testing.T) {
		req := Request{
			CustomerStatus:       StatusExempt,
			CustomerExemptReason: "501(c)(3)",
			LineItems:            []RequestLine{{Ref: "line_0", AmountCents: 10000}},
		}
		res, err := p.Calculate(context.Background(), req)
		if err != nil {
			t.Fatalf("Calculate: %v", err)
		}
		if !res.Exempt {
			t.Error("Exempt flag not set")
		}
		if res.ReverseCharge {
			t.Error("ReverseCharge should be false for exempt status")
		}
		if res.TotalTaxCents != 0 {
			t.Errorf("TotalTaxCents = %d, want 0", res.TotalTaxCents)
		}
		if res.ExemptReason != "501(c)(3)" {
			t.Errorf("ExemptReason = %q, want 501(c)(3)", res.ExemptReason)
		}
	})

	t.Run("reverse_charge", func(t *testing.T) {
		req := Request{
			CustomerStatus: StatusReverseCharge,
			LineItems:      []RequestLine{{Ref: "line_0", AmountCents: 10000}},
		}
		res, err := p.Calculate(context.Background(), req)
		if err != nil {
			t.Fatalf("Calculate: %v", err)
		}
		if !res.ReverseCharge {
			t.Error("ReverseCharge flag not set")
		}
		if res.Exempt {
			t.Error("Exempt should be false for reverse_charge")
		}
		if res.TotalTaxCents != 0 {
			t.Errorf("TotalTaxCents = %d, want 0", res.TotalTaxCents)
		}
	})
}

// TestResolver_Routing verifies tenant settings → provider mapping. Stripe
// Tax falls back to manual when no Stripe clients are configured so a
// mis-seeded tenant setting doesn't take billing offline.
func TestResolver_Routing(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		r := NewResolver(nil)
		p, err := r.Resolve(context.Background(), domain.TenantSettings{TaxProvider: "none"})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if p.Name() != "none" {
			t.Errorf("Name = %q, want none", p.Name())
		}
	})

	t.Run("manual", func(t *testing.T) {
		r := NewResolver(nil)
		p, err := r.Resolve(context.Background(), domain.TenantSettings{
			TaxProvider: "manual", TaxRate: 18.00, TaxName: "GST",
		})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if p.Name() != "manual" {
			t.Errorf("Name = %q, want manual", p.Name())
		}
	})

	t.Run("empty_provider_defaults_to_none", func(t *testing.T) {
		r := NewResolver(nil)
		p, err := r.Resolve(context.Background(), domain.TenantSettings{})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if p.Name() != "none" {
			t.Errorf("Name = %q, want none (safe default)", p.Name())
		}
	})

	t.Run("unknown_provider_errors", func(t *testing.T) {
		// tax_provider is constrained to {none,manual,stripe_tax} by
		// validateSettings, so an unrecognized value means a corrupted
		// settings row. Fail loud (no silent $0-tax fallback) so the
		// misconfiguration surfaces as a stuck billing cycle rather than
		// silent under-collection.
		r := NewResolver(nil)
		p, err := r.Resolve(context.Background(), domain.TenantSettings{TaxProvider: "avalara"})
		if err == nil {
			t.Fatalf("Resolve(avalara): want error, got provider %v", p)
		}
	})

	t.Run("stripe_tax_without_clients_errors_post_ADR_041", func(t *testing.T) {
		// Pre-ADR-041 behavior: silently fall back to manual provider.
		// Post-ADR-041 behavior: fail loud so the operator sees that
		// tax_provider=stripe_tax was set without wiring Stripe — they
		// should explicitly choose tax_provider=manual if that's what
		// they want, instead of getting it by accident through a fallback.
		r := NewResolver(nil)
		_, err := r.Resolve(context.Background(), domain.TenantSettings{
			TaxProvider: "stripe_tax", TaxRate: 5.00, TaxName: "Sales Tax",
		})
		if err == nil {
			t.Fatal("Resolve: expected error when stripe_tax selected without wired client")
		}
	})
}

// TestNoneProvider_HonorsExemptionStatus locks the consistency fix: a
// none-provider tenant's exempt / reverse_charge customer still gets the
// exemption flags on the result so the invoice renders the legally-required
// legend — same as the manual and stripe providers.
func TestNoneProvider_HonorsExemptionStatus(t *testing.T) {
	p := NewNoneProvider()
	line := []RequestLine{{Ref: "l1", AmountCents: 1000}}

	t.Run("exempt sets Exempt + reason, zero tax", func(t *testing.T) {
		res, err := p.Calculate(context.Background(), Request{
			CustomerStatus: StatusExempt, CustomerExemptReason: "Non-profit", LineItems: line,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Exempt || res.ExemptReason != "Non-profit" || res.TotalTaxCents != 0 {
			t.Errorf("got Exempt=%v reason=%q tax=%d, want true/Non-profit/0", res.Exempt, res.ExemptReason, res.TotalTaxCents)
		}
	})

	t.Run("reverse_charge sets ReverseCharge, zero tax", func(t *testing.T) {
		res, err := p.Calculate(context.Background(), Request{CustomerStatus: StatusReverseCharge, LineItems: line})
		if err != nil {
			t.Fatal(err)
		}
		if !res.ReverseCharge || res.Exempt || res.TotalTaxCents != 0 {
			t.Errorf("got ReverseCharge=%v Exempt=%v tax=%d, want true/false/0", res.ReverseCharge, res.Exempt, res.TotalTaxCents)
		}
	})

	t.Run("standard stays plain zero-tax (no flags)", func(t *testing.T) {
		res, err := p.Calculate(context.Background(), Request{CustomerStatus: StatusStandard, LineItems: line})
		if err != nil {
			t.Fatal(err)
		}
		if res.Exempt || res.ReverseCharge {
			t.Errorf("standard should not set exempt/reverse flags: Exempt=%v ReverseCharge=%v", res.Exempt, res.ReverseCharge)
		}
	})
}
