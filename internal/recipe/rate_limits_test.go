package recipe

import (
	"strings"
	"testing"
)

// A recipe's rates take the same limits as a rate sent to POST
// /v1/rating-rules: a recipe installs its rules through the store, not the
// pricing service, so the limits are checked when the recipe loads.
//
// Mutation-verify: drop the domain.CheckDecimal call in parseRate → the
// refused cases go red.
func TestParseRecipe_RatesTakeTheRequestLimits(t *testing.T) {
	head := "rating_rules:\n  - key: calls_rate\n    name: Calls\n    currency: USD\n"
	cases := []struct {
		field string // the name a refusal carries
		rule  func(rate string) string
	}{
		{"unit_amount_cents", func(rate string) string {
			return head + "    mode: per_unit\n    unit_amount_cents: '" + rate + "'\n"
		}},
		{"package_amount_cents", func(rate string) string {
			return head + "    mode: package\n    package_size: 100\n    package_amount_cents: '" + rate + "'\n"
		}},
		{"tiers.unit_amount_cents", func(rate string) string {
			return head + "    mode: graduated\n    tiers:\n      - up_to: null\n        unit_amount_cents: '" + rate + "'\n"
		}},
	}
	for _, tc := range cases {
		withRate := func(rate string) string {
			return strings.Replace(minimalRecipe, "plans:\n", tc.rule(rate)+"plans:\n", 1)
		}
		for _, rate := range []string{"0.000000000001", "999999999999999999.999999999999", "0"} {
			if _, err := parseRecipe([]byte(withRate(rate))); err != nil {
				t.Errorf("%s %s refused: %v", tc.field, rate, err)
			}
		}
		for _, rate := range []string{"0.0000000000001", "1000000000000000000", "-1", "1e9999999999"} {
			_, err := parseRecipe([]byte(withRate(rate)))
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("%s %s: err = %v, want a refusal naming the field", tc.field, rate, err)
			}
		}
	}
}
