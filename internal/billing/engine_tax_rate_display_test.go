package billing

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/tax"
)

// TestDisplayTaxRate pins the rule that the invoice-level invoices.tax_rate is
// the STATUTORY rate when every taxed line shares one rate (the common
// single-jurisdiction case), and only falls back to the blended effective rate
// for genuinely multi-rate invoices. This is what fixes the NYC case: Stripe's
// effective rate is 8.88 (888÷10000) but the line items carry the true 8.8750,
// which is the rate the customer/auditor should see. See ADR-047.
func TestDisplayTaxRate(t *testing.T) {
	cases := []struct {
		name      string
		lines     []tax.ResultLine
		effective float64
		want      float64
	}{
		{
			name: "single rate across all taxed lines → statutory (not effective)",
			lines: []tax.ResultLine{
				{TaxAmountCents: 355, TaxRate: 8.875},
				{TaxAmountCents: 311, TaxRate: 8.875},
				{TaxAmountCents: 222, TaxRate: 8.875},
			},
			effective: 8.88, // Stripe's tax÷subtotal
			want:      8.875,
		},
		{
			name: "mixed rates → blended effective",
			lines: []tax.ResultLine{
				{TaxAmountCents: 888, TaxRate: 8.875}, // US-NY
				{TaxAmountCents: 725, TaxRate: 7.25},  // US-CA
			},
			effective: 8.065,
			want:      8.065,
		},
		{
			name: "taxed + exempt mix → statutory (exempt line ignored)",
			lines: []tax.ResultLine{
				{TaxAmountCents: 888, TaxRate: 8.875},
				{TaxAmountCents: 0, TaxRate: 0}, // exempt / not-collecting line
			},
			effective: 4.44,
			want:      8.875,
		},
		{
			name:      "no taxed lines → effective fallback",
			lines:     []tax.ResultLine{{TaxAmountCents: 0, TaxRate: 0}},
			effective: 0,
			want:      0,
		},
		{
			name:      "empty lines → effective fallback",
			lines:     nil,
			effective: 7.25,
			want:      7.25,
		},
		{
			name: "manual provider parity: line rate == effective → unchanged",
			lines: []tax.ResultLine{
				{TaxAmountCents: 1800, TaxRate: 18.0},
			},
			effective: 18.0,
			want:      18.0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := displayTaxRate(c.lines, c.effective); got != c.want {
				t.Errorf("displayTaxRate = %v, want %v", got, c.want)
			}
		})
	}
}
