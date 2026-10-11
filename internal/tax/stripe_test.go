package tax

import (
	"testing"

	"github.com/stripe/stripe-go/v82"
)

// TestMapResult_TaxabilityReason_Persisted is the regression test for issue #4.
// Stripe Tax returns a structured `taxability_reason` on every per-line
// `tax_breakdown` entry; the engine needs that string preserved so the PDF
// and dashboard can distinguish two zero-tax outcomes (reverse_charge vs
// not_collecting vs customer_exempt) that read identically on the totals row
// but require different legends. mapResult was previously dropping the
// per-line value entirely; this test pins the round-trip.
func TestMapResult_TaxabilityReason_Persisted(t *testing.T) {
	cases := []struct {
		name string
		// reason is the Stripe-canonical taxability_reason string applied to
		// the single line in the mocked calculation.
		reason stripe.TaxCalculationLineItemTaxBreakdownTaxabilityReason
		// taxAmount is the Stripe-side per-line tax (zero for the reverse
		// charge / customer-exempt cases — matches the issue's motivating
		// scenario where tax_amount=0 lines need different legends).
		taxAmount int64
	}{
		{"not_collecting (no merchant registration)", stripe.TaxCalculationLineItemTaxBreakdownTaxabilityReasonNotCollecting, 0},
		{"reverse_charge (EU B2B)", stripe.TaxCalculationLineItemTaxBreakdownTaxabilityReasonReverseCharge, 0},
		{"customer_exempt (exemption certificate)", stripe.TaxCalculationLineItemTaxBreakdownTaxabilityReasonCustomerExempt, 0},
		{"product_exempt (jurisdiction excludes category)", stripe.TaxCalculationLineItemTaxBreakdownTaxabilityReasonProductExempt, 0},
		{"standard_rated (default sales tax path)", stripe.TaxCalculationLineItemTaxBreakdownTaxabilityReasonStandardRated, 825},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := Request{
				LineItems: []RequestLine{{Ref: "line_0", AmountCents: 10000, Quantity: 1}},
			}
			calc := &stripe.TaxCalculation{
				ID:                 "taxcalc_test",
				TaxAmountExclusive: c.taxAmount,
				LineItems: &stripe.TaxCalculationLineItemList{
					Data: []*stripe.TaxCalculationLineItem{{
						Reference: "line_0",
						Amount:    10000,
						AmountTax: c.taxAmount,
						TaxBreakdown: []*stripe.TaxCalculationLineItemTaxBreakdown{{
							Amount:           c.taxAmount,
							TaxabilityReason: c.reason,
						}},
					}},
				},
			}

			p := &StripeTaxProvider{}
			res, err := p.mapResult(calc, req)
			if err != nil {
				t.Fatalf("mapResult: unexpected error: %v", err)
			}
			if len(res.Lines) != 1 {
				t.Fatalf("Lines len = %d, want 1", len(res.Lines))
			}
			if got, want := res.Lines[0].TaxabilityReason, string(c.reason); got != want {
				t.Errorf("TaxabilityReason = %q, want %q (Stripe-canonical reason must round-trip onto the line)", got, want)
			}
		})
	}
}

// TestMapResult_TaxabilityReason_EmptyWhenNoBreakdown verifies the per-line
// reason stays "" when Stripe doesn't supply a TaxBreakdown[0] — defensive
// guard so a sparse Stripe response doesn't write a zero-value enum onto the
// invoice line. Empty is the correct sentinel for "no signal" downstream.
func TestMapResult_TaxabilityReason_EmptyWhenNoBreakdown(t *testing.T) {
	req := Request{LineItems: []RequestLine{{Ref: "line_0", AmountCents: 5000, Quantity: 1}}}
	calc := &stripe.TaxCalculation{
		ID: "taxcalc_no_breakdown",
		LineItems: &stripe.TaxCalculationLineItemList{
			Data: []*stripe.TaxCalculationLineItem{{
				Reference: "line_0",
				Amount:    5000,
				AmountTax: 0,
				// TaxBreakdown intentionally empty.
			}},
		},
	}

	p := &StripeTaxProvider{}
	res, err := p.mapResult(calc, req)
	if err != nil {
		t.Fatalf("mapResult: unexpected error: %v", err)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("Lines len = %d, want 1", len(res.Lines))
	}
	if got := res.Lines[0].TaxabilityReason; got != "" {
		t.Errorf("TaxabilityReason = %q, want empty (no TaxBreakdown[0] should leave the field unset)", got)
	}
}

// TestTaxTypeDisplayName pins the Stripe tax_type → customer-facing label map.
// Stripe returns raw snake_case enums ("sales_tax"); rendered verbatim the
// invoice reads "sales_tax (8.875%)". The map (all 15 stripe-go v82 tax_type
// values) yields "Sales Tax", preferring Stripe's display_name when present and
// title-casing any unmapped future enum so a raw token never reaches the PDF.
func TestTaxTypeDisplayName(t *testing.T) {
	cases := []struct {
		displayName string
		taxType     string
		want        string
	}{
		{"", "sales_tax", "Sales Tax"},
		{"", "vat", "VAT"},
		{"", "gst", "GST"},
		{"", "qst", "QST"},
		{"", "igst", "IGST"},
		{"", "retail_delivery_fee", "Retail Delivery Fee"},
		// Stripe's own localized display_name wins when supplied (per-line path).
		{"Value-added tax (VAT)", "vat", "Value-added tax (VAT)"},
		// Unmapped future enum → graceful title-case, never raw snake_case.
		{"", "some_future_tax", "Some Future Tax"},
		// Both empty → unset.
		{"", "", ""},
	}
	for _, c := range cases {
		if got := taxTypeDisplayName(c.displayName, c.taxType); got != c.want {
			t.Errorf("taxTypeDisplayName(%q, %q) = %q, want %q", c.displayName, c.taxType, got, c.want)
		}
	}
}

// TestMapResult_DocLevelRateFallback is the regression for the NYC precision
// loss found in invoice vlx_inv_d8gomormajdhcl08grvg. Stripe returns the
// verbatim percentage_decimal ("8.875") + jurisdiction in the DOCUMENT-level
// tax_breakdown, but leaves the PER-LINE tax_breakdown null (the request only
// expands "line_items", not "line_items.data.tax_breakdown"). Pre-fix, the line
// fell back to the rounded effective rate (888/10000 = 8.88) with an empty
// jurisdiction — silently losing Stripe's true 8.8750. The fix seeds the line
// from the single document-level rate.
func TestMapResult_DocLevelRateFallback(t *testing.T) {
	req := Request{LineItems: []RequestLine{{Ref: "line_0", AmountCents: 10000, Quantity: 1}}}
	calc := &stripe.TaxCalculation{
		ID:                 "taxcalc_nyc",
		TaxAmountExclusive: 888,
		// Verbatim rate + jurisdiction live in the document-level breakdown.
		TaxBreakdown: []*stripe.TaxCalculationTaxBreakdown{{
			Amount:           888,
			TaxableAmount:    10000,
			TaxabilityReason: stripe.TaxCalculationTaxBreakdownTaxabilityReason("standard_rated"),
			TaxRateDetails: &stripe.TaxCalculationTaxBreakdownTaxRateDetails{
				Country:           "US",
				State:             "NY",
				TaxType:           stripe.TaxCalculationTaxBreakdownTaxRateDetailsTaxType("sales_tax"),
				PercentageDecimal: "8.875",
			},
		}},
		LineItems: &stripe.TaxCalculationLineItemList{
			Data: []*stripe.TaxCalculationLineItem{{
				Reference:    "line_0",
				Amount:       10000,
				AmountTax:    888,
				TaxBreakdown: nil, // Stripe left this null (only line_items expanded)
			}},
		},
	}

	p := &StripeTaxProvider{}
	res, err := p.mapResult(calc, req)
	if err != nil {
		t.Fatalf("mapResult: %v", err)
	}
	if len(res.Lines) != 1 {
		t.Fatalf("Lines len = %d, want 1", len(res.Lines))
	}
	if res.TaxName != "Sales Tax" {
		t.Errorf("TaxName = %q, want \"Sales Tax\" (mapped from tax_type=sales_tax, not the raw enum)", res.TaxName)
	}
	l := res.Lines[0]
	if l.TaxRate != 8.875 {
		t.Errorf("line TaxRate = %v, want 8.875 (verbatim from document-level percentage_decimal, NOT the rounded 8.88 effective rate)", l.TaxRate)
	}
	if l.Jurisdiction != "US-NY" {
		t.Errorf("line Jurisdiction = %q, want US-NY (must not be dropped)", l.Jurisdiction)
	}
	if l.TaxabilityReason != "standard_rated" {
		t.Errorf("line TaxabilityReason = %q, want standard_rated", l.TaxabilityReason)
	}
	if l.TaxAmountCents != 888 {
		t.Errorf("line TaxAmountCents = %d, want 888 (Stripe's amount, unchanged)", l.TaxAmountCents)
	}
}

// TestMapResult_PerLineBreakdownWins verifies the document-level fallback does
// NOT override a genuine per-line breakdown when Stripe does supply one
// (multi-jurisdiction invoices). The per-line rate must take precedence.
func TestMapResult_PerLineBreakdownWins(t *testing.T) {
	req := Request{LineItems: []RequestLine{{Ref: "line_0", AmountCents: 10000, Quantity: 1}}}
	calc := &stripe.TaxCalculation{
		ID:                 "taxcalc_multi",
		TaxAmountExclusive: 600,
		TaxBreakdown: []*stripe.TaxCalculationTaxBreakdown{{
			Amount: 600,
			TaxRateDetails: &stripe.TaxCalculationTaxBreakdownTaxRateDetails{
				Country: "US", State: "CA", PercentageDecimal: "6.0",
			},
		}},
		LineItems: &stripe.TaxCalculationLineItemList{
			Data: []*stripe.TaxCalculationLineItem{{
				Reference: "line_0", Amount: 10000, AmountTax: 600,
				TaxBreakdown: []*stripe.TaxCalculationLineItemTaxBreakdown{{
					TaxRateDetails: &stripe.TaxCalculationLineItemTaxBreakdownTaxRateDetails{
						PercentageDecimal: "7.25",
					},
				}},
			}},
		},
	}
	p := &StripeTaxProvider{}
	res, err := p.mapResult(calc, req)
	if err != nil {
		t.Fatalf("mapResult: %v", err)
	}
	if l := res.Lines[0]; l.TaxRate != 7.25 {
		t.Errorf("line TaxRate = %v, want 7.25 (per-line breakdown must win over document-level 6.0)", l.TaxRate)
	}
}

// TestMapResult_NotCollecting_DocLevelReasonReachesZeroTaxLine is the FLOW
// B10 leg-10 regression, found against LIVE Stripe: an account with no
// registration in the customer's jurisdiction returns taxability_reason
// "not_collecting" ONLY in the document-level tax_breakdown (per-line
// breakdown null, amount_tax 0, rate 0). The old fallback gated the reason
// copy on docRate>0 && AmountTax!=0 — the one class the field exists to
// disambiguate (zero-tax outcomes) never round-tripped onto the line.
func TestMapResult_NotCollecting_DocLevelReasonReachesZeroTaxLine(t *testing.T) {
	req := Request{
		LineItems: []RequestLine{{Ref: "line_0", AmountCents: 10000, Quantity: 1}},
	}
	calc := &stripe.TaxCalculation{
		ID:                 "taxcalc_gb_nc",
		TaxAmountExclusive: 0,
		TaxBreakdown: []*stripe.TaxCalculationTaxBreakdown{{
			Amount:           0,
			TaxableAmount:    10000,
			TaxabilityReason: stripe.TaxCalculationTaxBreakdownTaxabilityReason("not_collecting"),
			TaxRateDetails: &stripe.TaxCalculationTaxBreakdownTaxRateDetails{
				Country:           "GB",
				TaxType:           stripe.TaxCalculationTaxBreakdownTaxRateDetailsTaxType("vat"),
				PercentageDecimal: "0.0",
			},
		}},
		LineItems: &stripe.TaxCalculationLineItemList{
			Data: []*stripe.TaxCalculationLineItem{{
				Reference:    "line_0",
				Amount:       10000,
				AmountTax:    0,
				TaxBreakdown: nil, // live Stripe shape: per-line breakdown absent
			}},
		},
	}

	p := &StripeTaxProvider{}
	res, err := p.mapResult(calc, req)
	if err != nil {
		t.Fatalf("mapResult: unexpected error: %v", err)
	}
	if got := res.Lines[0].TaxabilityReason; got != "not_collecting" {
		t.Errorf("line TaxabilityReason = %q, want %q (doc-level zero-tax reason must reach the line)", got, "not_collecting")
	}
	if res.Lines[0].TaxRate != 0 || res.Lines[0].TaxAmountCents != 0 {
		t.Errorf("zero-tax line must stay rate 0 / tax 0, got rate=%v tax=%d",
			res.Lines[0].TaxRate, res.Lines[0].TaxAmountCents)
	}
}

// Every requested line must come back mapped: a line Stripe returned under an
// unknown reference, or one missing from the list, used to keep zero tax while
// the invoice total included it.
func TestMapResult_UnmappedLineFails(t *testing.T) {
	req := Request{LineItems: []RequestLine{
		{Ref: "line_0", AmountCents: 10000, Quantity: 1},
		{Ref: "line_1", AmountCents: 5000, Quantity: 1},
	}}
	line := func(ref string, tax int64) *stripe.TaxCalculationLineItem {
		return &stripe.TaxCalculationLineItem{Reference: ref, Amount: 10000, AmountTax: tax}
	}
	p := &StripeTaxProvider{}

	unknownRef := &stripe.TaxCalculation{ID: "taxcalc_x", TaxAmountExclusive: 1500,
		LineItems: &stripe.TaxCalculationLineItemList{Data: []*stripe.TaxCalculationLineItem{line("line_0", 1000), line("mystery", 500)}}}
	if _, err := p.mapResult(unknownRef, req); err == nil {
		t.Error("unknown line reference: want an error, got a result")
	}

	missing := &stripe.TaxCalculation{ID: "taxcalc_y", TaxAmountExclusive: 1500,
		LineItems: &stripe.TaxCalculationLineItemList{Data: []*stripe.TaxCalculationLineItem{line("line_0", 1000)}}}
	if _, err := p.mapResult(missing, req); err == nil {
		t.Error("missing line: want an error, got a result")
	}

	full := &stripe.TaxCalculation{ID: "taxcalc_z", TaxAmountExclusive: 1500,
		LineItems: &stripe.TaxCalculationLineItemList{Data: []*stripe.TaxCalculationLineItem{line("line_1", 500), line("line_0", 1000)}}}
	res, err := p.mapResult(full, req)
	if err != nil {
		t.Fatalf("every line mapped (any order): %v", err)
	}
	if res.Lines[0].TaxAmountCents != 1000 || res.Lines[1].TaxAmountCents != 500 {
		t.Errorf("lines = %d/%d, want 1000/500", res.Lines[0].TaxAmountCents, res.Lines[1].TaxAmountCents)
	}
}
