package recipe

import (
	"strings"
	"testing"
)

// A recipe's rates take the same limits as a rate sent to the prices API
// (domain.CheckRate), checked when the recipe loads, so a recipe never
// fails at install on a rate.
//
// Mutation-verify: drop the domain.CheckRate call in parseRate → the
// refused cases go red.
func TestParseRecipe_RatesTakeTheRequestLimits(t *testing.T) {
	cases := []struct {
		field string // the name a refusal carries
		shape func(rate string) string
	}{
		{"unit_amount_cents", func(rate string) string {
			return "        mode: per_unit\n        unit_amount_cents: '" + rate + "'\n"
		}},
		{"package_amount_cents", func(rate string) string {
			return "        mode: package\n        package_size: 100\n        package_amount_cents: '" + rate + "'\n"
		}},
		{"tiers.unit_amount_cents", func(rate string) string {
			return "        mode: graduated\n        tiers:\n          - up_to: null\n            unit_amount_cents: '" + rate + "'\n"
		}},
	}
	for _, tc := range cases {
		for _, rate := range []string{"0.000000000001", "999999999999999999.999999999999", "0"} {
			if _, err := parseRecipe([]byte(withPrice(tc.shape(rate)))); err != nil {
				t.Errorf("%s %s refused: %v", tc.field, rate, err)
			}
		}
		for _, rate := range []string{"0.0000000000001", "1000000000000000000", "-1", "1e9999999999"} {
			_, err := parseRecipe([]byte(withPrice(tc.shape(rate))))
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("%s %s: err = %v, want a refusal naming the field", tc.field, rate, err)
			}
		}
	}
}
