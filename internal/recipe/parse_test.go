package recipe

import (
	"strings"
	"testing"
)

// minimalRecipe is the smallest YAML parseRecipe accepts, with one key at
// each nesting level the typo cases below replace.
const minimalRecipe = `key: strict_check
version: 1.0.0
name: Strict check
meters:
  - key: calls
    name: Calls
    unit: call
    aggregation: sum
plans:
  - code: basic
    name: Basic
    currency: USD
    billing_interval: monthly
    base_amount_cents: 1000
    base_bill_timing: in_advance
    meters: [calls]
dunning:
  policy:
    name: Retry
    max_retries: 1
    intervals_hours: [24]
    final_subscription_action: pause
    final_invoice_action: none
`

// TestParseRecipe_RejectsUnknownKey: a misspelled key must fail the load,
// not decode to the field's zero value. Before strict decoding,
// `base_bill_timming` parsed cleanly and the plan billed in arrears.
func TestParseRecipe_RejectsUnknownKey(t *testing.T) {
	if _, err := parseRecipe([]byte(minimalRecipe)); err != nil {
		t.Fatalf("control: minimal recipe should parse, got %v", err)
	}

	cases := []struct {
		name, from, to string
	}{
		{"top level", "name: Strict check", "nmae: Strict check"},
		{"plan", "base_bill_timing: in_advance", "base_bill_timming: in_advance"},
		{"dunning policy", "max_retries: 1", "max_retry: 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typo := strings.Replace(minimalRecipe, tc.from, tc.to, 1)
			if typo == minimalRecipe {
				t.Fatalf("fixture: %q not found in minimalRecipe", tc.from)
			}
			badKey := strings.SplitN(tc.to, ":", 2)[0]
			_, err := parseRecipe([]byte(typo))
			if err == nil {
				t.Fatalf("parseRecipe accepted unknown key %q", badKey)
			}
			if !strings.Contains(err.Error(), badKey) {
				t.Errorf("error should name %q, got %v", badKey, err)
			}
		})
	}
}

// TestParseRecipe_LegacyFinalActionNamesReplacement: the pre-ADR-112 key
// keeps its specific error, which tells the author which keys replace it,
// instead of the generic unknown-field error.
func TestParseRecipe_LegacyFinalActionNamesReplacement(t *testing.T) {
	legacy := strings.Replace(minimalRecipe, "    final_invoice_action: none\n",
		"    final_invoice_action: none\n    final_action: cancel_subscription\n", 1)
	_, err := parseRecipe([]byte(legacy))
	if err == nil {
		t.Fatal("parseRecipe accepted dunning.policy.final_action")
	}
	if !strings.Contains(err.Error(), "final_subscription_action") {
		t.Errorf("error should name the replacement keys, got %v", err)
	}
}
