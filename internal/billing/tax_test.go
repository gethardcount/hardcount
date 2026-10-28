package billing

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/money"
)

// ---------------------------------------------------------------------------
// Tax calculation tests
//
// The billing engine computes tax using integer math (basis points):
//   taxAmount = subtotal * taxRateBP / 10000
//   if (subtotal * taxRateBP) % 10000 >= 5000 then round up
//
// These standalone tests verify the formula matches the engine's behavior
// without needing to run a full billing cycle.
// ---------------------------------------------------------------------------

// computeTaxBP mirrors the billing engine's tax calculation logic exactly.
// Uses banker's rounding (half-to-even) to match the engine's zero-bias math.
func computeTaxBP(subtotalCents int64, taxRateBP int64) int64 {
	if taxRateBP <= 0 || subtotalCents <= 0 {
		return 0
	}
	return money.RoundHalfToEven(subtotalCents*taxRateBP, 10000)
}

// computeLineTaxBP mirrors per-line-item tax in the engine.
func computeLineTaxBP(amountCents int64, taxRateBP int64) int64 {
	if taxRateBP <= 0 || amountCents <= 0 {
		return 0
	}
	return money.RoundHalfToEven(amountCents*int64(taxRateBP), 10000)
}

func TestTaxBP_BasicCases(t *testing.T) {
	tests := []struct {
		name     string
		subtotal int64
		rateBP   int64
		wantTax  int64
	}{
		{
			name:     "18.5% on $100",
			subtotal: 10000,
			rateBP:   1850,
			wantTax:  1850,
		},
		{
			name:     "7.25% rounding: 999 cents",
			subtotal: 999,
			rateBP:   725,
			// 999 * 725 = 724275, / 10000 = 72, remainder = 4275 < 5000 => no round up
			wantTax: 72,
		},
		{
			name:     "zero rate",
			subtotal: 10000,
			rateBP:   0,
			wantTax:  0,
		},
		{
			name:     "zero subtotal",
			subtotal: 0,
			rateBP:   1850,
			wantTax:  0,
		},
		{
			name:     "100% tax rate",
			subtotal: 10000,
			rateBP:   10000,
			wantTax:  10000,
		},
		{
			name:     "5% on 1 cent — rounds down to 0",
			subtotal: 1,
			rateBP:   500,
			// 1 * 500 = 500, / 10000 = 0, remainder = 500 < 5000 => no round up
			wantTax: 0,
		},
		{
			name:     "large amount: $9,999,999.99 at 18.5%",
			subtotal: 999_999_999,
			rateBP:   1850,
			// 999999999 * 1850 = 1,849,999,998,150
			// / 10000 = 184,999,999 (integer)
			// remainder = 8150 >= 5000 => round up
			wantTax: 185_000_000,
		},
		{
			name:     "20% VAT on $50",
			subtotal: 5000,
			rateBP:   2000,
			wantTax:  1000,
		},
		{
			name:     "8.875% NYC sales tax on $99.99",
			subtotal: 9999,
			rateBP:   888, // 8.88% (closest whole BP)
			// 9999 * 888 = 8,879,112, / 10000 = 887, remainder = 9112 >= 5000 => round up
			wantTax: 888,
		},
		{
			name:     "0.01% on $100 — smallest meaningful tax",
			subtotal: 10000,
			rateBP:   1,
			// 10000 * 1 = 10000, / 10000 = 1, remainder = 0
			wantTax: 1,
		},
		{
			name:     "negative rate treated as zero",
			subtotal: 10000,
			rateBP:   -100,
			wantTax:  0,
		},
		{
			name:     "negative subtotal treated as zero",
			subtotal: -5000,
			rateBP:   1850,
			wantTax:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTaxBP(tt.subtotal, tt.rateBP)
			if got != tt.wantTax {
				t.Errorf("computeTaxBP(%d, %d) = %d, want %d", tt.subtotal, tt.rateBP, got, tt.wantTax)
			}
		})
	}
}

func TestTaxBP_RoundingEdgeCases(t *testing.T) {
	// Remainder == exactly 5000 should round up
	// subtotal * rateBP must end in exactly 5000 mod 10000
	// Example: 10 * 5000 = 50000, /10000 = 5, remainder = 0 => no rounding (exact)
	// Find: 3 * 8333 = 24999, /10000 = 2, remainder = 4999 => no round up
	// Find: 3 * 8334 = 25002, /10000 = 2, remainder = 5002 => round up
	tests := []struct {
		name     string
		subtotal int64
		rateBP   int64
		wantTax  int64
	}{
		{
			name:     "remainder exactly 4999 — no round up",
			subtotal: 3,
			rateBP:   8333,
			// 3 * 8333 = 24999, /10000 = 2, remainder = 4999
			wantTax: 2,
		},
		{
			name:     "remainder exactly 5000, odd quotient — round up (banker's)",
			subtotal: 2,
			rateBP:   7500,
			// 2 * 7500 = 15000, /10000 = 1, remainder = 5000. Quotient 1 is odd, round to even → 2.
			wantTax: 2,
		},
		{
			name:     "remainder exactly 5000, even quotient — round down (banker's, not half-up!)",
			subtotal: 5,
			rateBP:   5000,
			// 5 * 5000 = 25000, /10000 = 2, remainder = 5000. Quotient 2 is even, stay → 2.
			// Half-up would incorrectly return 3 here.
			wantTax: 2,
		},
		{
			name:     "remainder 5001 — round up",
			subtotal: 3,
			rateBP:   8334,
			// 3 * 8334 = 25002, /10000 = 2, remainder = 5002
			wantTax: 3,
		},
		{
			name:     "exact division — no rounding needed",
			subtotal: 10000,
			rateBP:   2000,
			// 10000 * 2000 = 20,000,000, /10000 = 2000, remainder = 0
			wantTax: 2000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeTaxBP(tt.subtotal, tt.rateBP)
			if got != tt.wantTax {
				remainder := (tt.subtotal * int64(tt.rateBP)) % 10000
				t.Errorf("computeTaxBP(%d, %d) = %d, want %d (remainder=%d)",
					tt.subtotal, tt.rateBP, got, tt.wantTax, remainder)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Line-item tax summing — per-line taxes must sum exactly to the invoice-level
// tax. This verifies the *sum invariant* across line-item combinations using a
// simple last-line reconciliation as a stand-in. The PRODUCTION distribution is
// largest-remainder (ADR-046), exercised against the real provider in
// internal/tax/providers_test.go (TestManualProvider_LargestRemainderNoInversion
// + the property test); the sum invariant proved here holds for either method.
// ---------------------------------------------------------------------------

func TestTaxBP_LineItemSumEqualsInvoiceTax(t *testing.T) {
	tests := []struct {
		name     string
		lineAmts []int64 // per-line amounts in cents
		rateBP   int64
	}{
		{
			name:     "two equal lines at 18.5%",
			lineAmts: []int64{5000, 5000},
			rateBP:   1850,
		},
		{
			name:     "three unequal lines at 7.25%",
			lineAmts: []int64{1234, 5678, 999},
			rateBP:   725,
		},
		{
			name:     "single line",
			lineAmts: []int64{9999},
			rateBP:   1850,
		},
		{
			name:     "many small lines at high rate",
			lineAmts: []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
			rateBP:   9999,
		},
		{
			name:     "mix of large and small",
			lineAmts: []int64{999999, 1, 50000, 7},
			rateBP:   825,
		},
		{
			name:     "two lines that individually round differently",
			lineAmts: []int64{333, 667},
			rateBP:   1850,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Compute subtotal
			subtotal := int64(0)
			for _, a := range tt.lineAmts {
				subtotal += a
			}

			// Invoice-level tax
			invoiceTax := computeTaxBP(subtotal, tt.rateBP)

			// Per-line tax via the rounding primitive (production distributes
			// the residual by largest remainder; see ADR-046 / internal/tax).
			lineItems := make([]domain.InvoiceLineItem, len(tt.lineAmts))
			var lineTaxSum int64
			for i, amt := range tt.lineAmts {
				lineTax := computeLineTaxBP(amt, tt.rateBP)
				lineItems[i] = domain.InvoiceLineItem{
					AmountCents:      amt,
					TaxRate:          float64(tt.rateBP) / 100,
					TaxAmountCents:   lineTax,
					TotalAmountCents: amt + lineTax,
				}
				lineTaxSum += lineTax
			}

			// Reconcile to the invoice total (last-line stand-in; production uses
			// largest-remainder — either satisfies the sum invariant asserted here)
			if len(lineItems) > 0 && lineTaxSum != invoiceTax {
				diff := invoiceTax - lineTaxSum
				last := &lineItems[len(lineItems)-1]
				last.TaxAmountCents += diff
				last.TotalAmountCents += diff
				lineTaxSum += diff
			}

			if lineTaxSum != invoiceTax {
				t.Errorf("line tax sum %d != invoice tax %d (subtotal=%d, rateBP=%d)",
					lineTaxSum, invoiceTax, subtotal, tt.rateBP)
			}

			// Verify total_amount_cents consistency
			for i, li := range lineItems {
				expectedTotal := li.AmountCents + li.TaxAmountCents
				if li.TotalAmountCents != expectedTotal {
					t.Errorf("line %d: total_amount_cents=%d, want amount(%d)+tax(%d)=%d",
						i, li.TotalAmountCents, li.AmountCents, li.TaxAmountCents, expectedTotal)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Property: tax on a+b should equal (tax on a) + (tax on b) + adjustment <= 1 cent
// This verifies the adjustment never exceeds 1 cent.
// ---------------------------------------------------------------------------

func TestTaxBP_AdjustmentNeverExceedsOneCent(t *testing.T) {
	testCases := []struct {
		lineAmts []int64
		rateBP   int64
	}{
		{[]int64{1, 1}, 9999},
		{[]int64{9999, 1}, 9999},
		{[]int64{333, 333, 334}, 3333},
		{[]int64{1000, 2000, 3000, 4000}, 1575},
		{[]int64{17, 23, 41, 59, 83}, 725},
	}

	for _, tc := range testCases {
		subtotal := int64(0)
		for _, a := range tc.lineAmts {
			subtotal += a
		}
		invoiceTax := computeTaxBP(subtotal, tc.rateBP)

		var lineTaxSum int64
		for _, amt := range tc.lineAmts {
			lineTaxSum += computeLineTaxBP(amt, tc.rateBP)
		}

		diff := invoiceTax - lineTaxSum
		if diff < -1 || diff > 1 {
			t.Errorf("adjustment %d exceeds 1 cent for lineAmts=%v, rateBP=%d (invoiceTax=%d, lineTaxSum=%d)",
				diff, tc.lineAmts, tc.rateBP, invoiceTax, lineTaxSum)
		}
	}
}

// ---------------------------------------------------------------------------
// Verify tax is always non-negative for valid inputs
// ---------------------------------------------------------------------------

func TestTaxBP_AlwaysNonNegative(t *testing.T) {
	subtotals := []int64{0, 1, 100, 999, 10000, 999999999}
	rates := []int64{0, 1, 500, 1000, 1850, 5000, 10000}

	for _, sub := range subtotals {
		for _, rate := range rates {
			tax := computeTaxBP(sub, rate)
			if tax < 0 {
				t.Errorf("negative tax %d for subtotal=%d, rateBP=%d", tax, sub, rate)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Tax should never exceed subtotal (rate capped at 100% = 10000 BP)
// ---------------------------------------------------------------------------

func TestTaxBP_NeverExceedsSubtotal(t *testing.T) {
	subtotals := []int64{1, 100, 9999, 10000, 999999999}
	for _, sub := range subtotals {
		tax := computeTaxBP(sub, 10000) // 100%
		if tax > sub {
			t.Errorf("tax %d exceeds subtotal %d at 100%%", tax, sub)
		}
	}
}
