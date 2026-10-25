package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The price shapes of ADR-130 §3, each written in the API's words and priced
// by the one function every rating path calls (domain.ComputeAmountCents):
//
//   - per_unit: quantity × unit_amount_cents;
//   - graduated: each tier prices the units above the tier before it, up to
//     its up_to; the last tier is open, "up_to": null;
//   - package: ceil(quantity / package_size) × package_amount_cents, so a
//     part package bills as a whole one, and the package amount is a
//     decimal.
//
// A line rounds once, half to even, to whole cents. Each expected amount is
// worked out by hand in its comment.
//
// Mutation-verify: price packages with floor instead of ceil → the package
// cases go red; let the last tier carry a bound → TestPriceShapes_LastTierIsOpen
// goes red.

// priceShape decodes a price from the API's JSON words.
func priceShape(t *testing.T, raw string) domain.RatingRuleVersion {
	t.Helper()
	var r domain.RatingRuleVersion
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return r
}

func TestPriceShapes_EachShapeBillsItsGolden(t *testing.T) {
	type bill struct {
		qty  string
		want int64
	}
	cases := []struct {
		name  string
		price string
		bills []bill
	}{
		{
			name:  "per_unit at a decimal rate",
			price: `{"mode":"per_unit","unit_amount_cents":"0.0035"}`,
			bills: []bill{
				{"0", 0},
				{"1", 0},          // 0.0035
				{"143", 1},        // 0.5005
				{"1000", 4},       // 3.5, half to even
				{"3000", 10},      // 10.5, half to even
				{"1234.5", 4},     // 4.32075
				{"1000000", 3500}, // $35.00 a million
			},
		},
		{
			name: "graduated, the last tier open",
			price: `{"mode":"graduated","tiers":[
				{"up_to":100,"unit_amount_cents":"5"},
				{"up_to":1000,"unit_amount_cents":"2.5"},
				{"up_to":null,"unit_amount_cents":"1.25"}]}`,
			bills: []bill{
				{"0", 0},
				{"50", 250},
				{"100", 500},
				{"101", 502},           // 500 + 2.5, half to even
				{"1000", 2750},         // 500 + 900 × 2.5
				{"1500.5", 3376},       // 2750 + 500.5 × 1.25 = 3375.625
				{"1000000", 1_251_500}, // 2750 + 999000 × 1.25
			},
		},
		{
			name: "graduated, a free first tier",
			price: `{"mode":"graduated","tiers":[
				{"up_to":1000,"unit_amount_cents":"0"},
				{"up_to":null,"unit_amount_cents":"0.2"}]}`,
			bills: []bill{
				{"999", 0},
				{"1000", 0},
				{"1500", 100}, // 500 × 0.2
			},
		},
		{
			name:  "package, a part package bills as a whole one",
			price: `{"mode":"package","package_size":100,"package_amount_cents":"120.5"}`,
			bills: []bill{
				{"0", 0},
				{"1", 120},     // one package: 120.5, half to even
				{"100", 120},   // one package
				{"101", 241},   // two packages
				{"150", 241},   // two packages
				{"250.5", 362}, // three packages: 361.5, half to even
			},
		},
		{
			name:  "package below a cent: $0.002 per 1,000 tokens",
			price: `{"mode":"package","package_size":1000,"package_amount_cents":"0.2"}`,
			bills: []bill{
				{"1", 0},         // one package: 0.2
				{"1000", 0},      // one package: 0.2
				{"2500", 1},      // three packages: 0.6
				{"1000000", 200}, // 1,000 packages
				{"1000001", 200}, // 1,001 packages: 200.2
			},
		},
		{
			// The smallest part a quantity can hold (12 decimal places)
			// still starts a package, however large the package.
			name:  "package, a part package of 10^-12 at a large size",
			price: `{"mode":"package","package_size":100000,"package_amount_cents":"100"}`,
			bills: []bill{
				{"99999.999999999999", 100},  // one package
				{"100000", 100},              // one package
				{"100000.000000000001", 200}, // two packages
				{"200000.000000000002", 300}, // three packages
				{"30000000.000000000001", 30100},
			},
		},
		{
			name:  "package of 10^18 units",
			price: `{"mode":"package","package_size":1000000000000000000,"package_amount_cents":"1"}`,
			bills: []bill{
				{"1000000000000000000", 1}, // one package
				{"1000000000000000049", 2}, // two packages
				{"1000000000000000000.000000000001", 2},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := priceShape(t, tc.price)
			for _, b := range tc.bills {
				got, err := domain.ComputeAmountCents(rule, decimal.RequireFromString(b.qty))
				if err != nil {
					t.Errorf("%s units: %v", b.qty, err)
					continue
				}
				if got != b.want {
					t.Errorf("%s units: %d cents, want %d", b.qty, got, b.want)
				}
			}
		})
	}
}

// TestPriceShapes_LastTierIsOpen: only the last tier is open, and it must be.
// A bounded last tier would leave the usage past it with no price, so it
// does not rate, even for a quantity under its bound; an open tier before the
// last, and the old spelling of an open last tier (`"up_to": 0`), do not
// either.
func TestPriceShapes_LastTierIsOpen(t *testing.T) {
	for _, tc := range []struct{ name, tiers string }{
		{"a bounded last tier", `[{"up_to":100,"unit_amount_cents":"5"},{"up_to":1000,"unit_amount_cents":"1"}]`},
		{"an open tier before the last", `[{"up_to":null,"unit_amount_cents":"5"},{"up_to":100,"unit_amount_cents":"1"}]`},
		{"the last tier at up_to 0", `[{"up_to":100,"unit_amount_cents":"5"},{"up_to":0,"unit_amount_cents":"1"}]`},
		{"two open tiers", `[{"up_to":null,"unit_amount_cents":"5"},{"up_to":null,"unit_amount_cents":"1"}]`},
		{"bounds that do not increase", `[{"up_to":100,"unit_amount_cents":"5"},{"up_to":100,"unit_amount_cents":"2"},{"up_to":null,"unit_amount_cents":"1"}]`},
		{"no tiers", `[]`},
	} {
		rule := priceShape(t, `{"mode":"graduated","tiers":`+tc.tiers+`}`)
		if got, err := domain.ComputeAmountCents(rule, decimal.NewFromInt(50)); err == nil {
			t.Errorf("%s: priced 50 units at %d cents, want an error", tc.name, got)
		}
	}
}

// TestPriceShapes_WireWords: a price goes back out in the words it came in:
// mode per_unit, unit_amount_cents, tiers whose last up_to is null, and a
// decimal package amount. No old word is sent.
func TestPriceShapes_WireWords(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"mode":"per_unit","unit_amount_cents":"0.0035"}`, `"mode":"per_unit"`},
		{`{"mode":"per_unit","unit_amount_cents":"0.0035"}`, `"unit_amount_cents":"0.0035"`},
		{`{"mode":"graduated","tiers":[{"up_to":100,"unit_amount_cents":"5"},{"up_to":null,"unit_amount_cents":"1"}]}`,
			`"tiers":[{"up_to":100,"unit_amount_cents":"5"},{"up_to":null,"unit_amount_cents":"1"}]`},
		{`{"mode":"package","package_size":1000,"package_amount_cents":"0.2"}`, `"package_amount_cents":"0.2"`},
	} {
		out, err := json.Marshal(priceShape(t, tc.in))
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if !strings.Contains(string(out), tc.want) {
			t.Errorf("%s encodes as %s, want it to hold %s", tc.in, out, tc.want)
		}
		for _, old := range []string{"flat_amount_cents", "graduated_tiers", "overage_unit_amount_cents"} {
			if strings.Contains(string(out), old) {
				t.Errorf("%s encodes as %s, which holds the old word %s", tc.in, out, old)
			}
		}
	}
}
