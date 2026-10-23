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
	withRate := func(field, rate string) string {
		rule := "rating_rules:\n  - key: calls_rate\n    name: Calls\n    mode: flat\n    currency: USD\n    " + field + ": '" + rate + "'\n"
		if field == "unit_amount_cents" {
			rule = "rating_rules:\n  - key: calls_rate\n    name: Calls\n    mode: graduated\n    currency: USD\n    graduated_tiers:\n      - up_to: 0\n        unit_amount_cents: '" + rate + "'\n"
		}
		return strings.Replace(minimalRecipe, "plans:\n", rule+"plans:\n", 1)
	}
	for _, field := range []string{"flat_amount_cents", "overage_unit_amount_cents", "unit_amount_cents"} {
		for _, rate := range []string{"0.000000000001", "999999999999999999.999999999999", "0"} {
			if _, err := parseRecipe([]byte(withRate(field, rate))); err != nil {
				t.Errorf("%s %s refused: %v", field, rate, err)
			}
		}
		for _, rate := range []string{"0.0000000000001", "1000000000000000000", "-1", "1e9999999999"} {
			_, err := parseRecipe([]byte(withRate(field, rate)))
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s %s: err = %v, want a refusal naming the field", field, rate, err)
			}
		}
	}
}
