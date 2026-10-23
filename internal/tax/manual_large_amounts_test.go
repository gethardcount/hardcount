package tax

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The manual provider works in int64 cents. Its products (amount × rate in
// parts per million, amount × discount) can be larger than any int64, so
// they are worked out in 128 bits: the tax of any line amount is exact, and
// a result that does not fit int64 is an error, never a wrapped number.
// The rate is 0 to 100%, so a tax never exceeds its base and only the sum of
// the lines can be too large.

func manualLines(amounts ...int64) []RequestLine {
	lines := make([]RequestLine, len(amounts))
	for i, a := range amounts {
		lines[i] = RequestLine{Ref: string(rune('a' + i)), AmountCents: a}
	}
	return lines
}

func TestManual_LargeAmountsAreExact(t *testing.T) {
	const maxI = math.MaxInt64
	cases := []struct {
		name      string
		rate      float64
		inclusive bool
		discount  int64
		amounts   []int64
		wantTax   int64
		wantLines []int64 // per-line tax
		wantNet   []int64 // per-line net
	}{
		{
			// 1e14 × 20% = 2e13.
			name: "exclusive, 1e14 at 20%", rate: 20,
			amounts: []int64{100_000_000_000_000},
			wantTax: 20_000_000_000_000, wantLines: []int64{20_000_000_000_000},
			wantNet: []int64{100_000_000_000_000},
		},
		{
			// 4999999999999999995 × 20% = 999999999999999999.
			name: "exclusive, near 5e18 at 20%", rate: 20,
			amounts: []int64{4_999_999_999_999_999_995},
			wantTax: 999_999_999_999_999_999, wantLines: []int64{999_999_999_999_999_999},
			wantNet: []int64{4_999_999_999_999_999_995},
		},
		{
			name: "exclusive, MaxInt64 at 100%", rate: 100,
			amounts: []int64{maxI},
			wantTax: maxI, wantLines: []int64{maxI}, wantNet: []int64{maxI},
		},
		{
			// 8.875% of 9e18 = 798750000000000000 exactly; split 2:1.
			name: "exclusive, two lines summing to 9e18 at 8.875%", rate: 8.875,
			amounts:   []int64{6_000_000_000_000_000_000, 3_000_000_000_000_000_000},
			wantTax:   798_750_000_000_000_000,
			wantLines: []int64{532_500_000_000_000_000, 266_250_000_000_000_000},
			wantNet:   []int64{6_000_000_000_000_000_000, 3_000_000_000_000_000_000},
		},
		{
			// Discount 1e18 off 9e18, spread 2:1: bases 5.333…e18 and
			// 2.666…e18; tax 20% of 8e18 = 1.6e18.
			name: "exclusive, discount on large lines", rate: 20, discount: 1_000_000_000_000_000_000,
			amounts:   []int64{6_000_000_000_000_000_000, 3_000_000_000_000_000_000},
			wantTax:   1_600_000_000_000_000_000,
			wantLines: []int64{1_066_666_666_666_666_667, 533_333_333_333_333_333},
			wantNet:   []int64{6_000_000_000_000_000_000, 3_000_000_000_000_000_000},
		},
		{
			// Gross 1e14 at 20%: net = 1e14 / 1.2 = 83333333333333.33 → 83333333333333;
			// tax = 1e14 − 83333333333333 = 16666666666667.
			name: "inclusive, 1e14 at 20%", rate: 20, inclusive: true,
			amounts: []int64{100_000_000_000_000},
			wantTax: 16_666_666_666_667, wantLines: []int64{16_666_666_666_667},
			wantNet: []int64{83_333_333_333_333},
		},
		{
			// Gross MaxInt64 at 100%: net = 4611686018427387903.5 → 4611686018427387904
			// (half to even); tax = MaxInt64 − net = 4611686018427387903.
			name: "inclusive, MaxInt64 at 100%", rate: 100, inclusive: true,
			amounts: []int64{maxI},
			wantTax: 4_611_686_018_427_387_903, wantLines: []int64{4_611_686_018_427_387_903},
			wantNet: []int64{4_611_686_018_427_387_904},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewManualProvider(tc.rate, "VAT")
			res, err := p.Calculate(context.Background(), Request{
				LineItems: manualLines(tc.amounts...), TaxInclusive: tc.inclusive, DiscountCents: tc.discount,
			})
			if err != nil {
				t.Fatalf("Calculate: %v", err)
			}
			if res.TotalTaxCents != tc.wantTax {
				t.Errorf("total tax = %d, want %d", res.TotalTaxCents, tc.wantTax)
			}
			var sum int64
			for i, l := range res.Lines {
				sum += l.TaxAmountCents
				if l.TaxAmountCents != tc.wantLines[i] {
					t.Errorf("line %d tax = %d, want %d", i, l.TaxAmountCents, tc.wantLines[i])
				}
				if l.NetAmountCents != tc.wantNet[i] {
					t.Errorf("line %d net = %d, want %d", i, l.NetAmountCents, tc.wantNet[i])
				}
			}
			if sum != res.TotalTaxCents {
				t.Errorf("line taxes sum to %d, total is %d", sum, res.TotalTaxCents)
			}
		})
	}
}

// Lines whose sum does not fit int64 are an error naming the overflow.
func TestManual_LinesTooLargeToSumAreAnError(t *testing.T) {
	for _, inclusive := range []bool{false, true} {
		p := NewManualProvider(20, "VAT")
		_, err := p.Calculate(context.Background(), Request{
			LineItems:    manualLines(5_000_000_000_000_000_000, 5_000_000_000_000_000_000),
			TaxInclusive: inclusive,
		})
		if !errors.Is(err, domain.ErrAmountOverflow) {
			t.Errorf("inclusive=%v: err = %v, want ErrAmountOverflow", inclusive, err)
		}
	}
}
