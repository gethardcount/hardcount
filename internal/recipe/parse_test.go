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
  - event_name: calls
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
    retry_days: [1]
    final_subscription_action: pause
    final_invoice_action: none
`

// TestParseRecipe_RefusesRemovedMeterFields: a meter is named by event_name,
// not key, and a pricing rule has no aggregation of its own (ADR-130 §2).
// A recipe that still uses either fails the load instead of installing a
// meter without its event name or a rule whose aggregation is ignored.
func TestParseRecipe_RefusesRemovedMeterFields(t *testing.T) {
	withRule := strings.Replace(minimalRecipe, "plans:\n", `rating_rules:
  - key: calls_rate
    name: Calls
    mode: per_unit
    currency: USD
    unit_amount_cents: '1'
pricing_rules:
  - meter: calls
    rating_rule: calls_rate
    dimension_match: {}
    priority: 100
plans:
`, 1)
	if _, err := parseRecipe([]byte(withRule)); err != nil {
		t.Fatalf("control: a recipe with a pricing rule should parse, got %v", err)
	}
	for name, yaml := range map[string]string{
		"meter key":                 strings.Replace(minimalRecipe, "  - event_name: calls", "  - key: calls", 1),
		"pricing rule aggregation":  strings.Replace(withRule, "    priority: 100", "    priority: 100\n    aggregation_mode: sum", 1),
		"meter aggregation unknown": strings.Replace(minimalRecipe, "    aggregation: sum", "    aggregation: last_ever", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRecipe([]byte(yaml)); err == nil {
				t.Fatalf("parseRecipe accepted it:\n%s", yaml)
			}
		})
	}
}

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
		{"dunning policy", "retry_days: [1]", "retry_day: [1]"},
		// The schedule fields retry_days replaced are refused too, as
		// unknown keys, so a recipe cannot carry a count or hours that
		// would be dropped.
		{"old retry count", "retry_days: [1]", "max_retries: 1"},
		{"old hour waits", "retry_days: [1]", "intervals_hours: [24]"},
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

// TestParseRecipe_PriceShapes: a recipe's rating rules take ADR-130 §3's
// shapes and words. A graduated rule's last tier is open (`up_to: null`, or
// no up_to) and only the last; a package has a size above 0 and a decimal
// amount. The old words, `flat`, flat_amount_cents, graduated_tiers and
// overage_unit_amount_cents, fail the load, and so does a tier table that
// could not rate, before any install writes it.
func TestParseRecipe_PriceShapes(t *testing.T) {
	withRule := func(rule string) string {
		return strings.Replace(minimalRecipe, "plans:\n", "rating_rules:\n  - key: calls_rate\n    name: Calls\n    currency: USD\n"+rule+"plans:\n", 1)
	}
	for name, rule := range map[string]string{
		"per_unit":                 "    mode: per_unit\n    unit_amount_cents: '0.0035'\n",
		"graduated, up_to null":    "    mode: graduated\n    tiers:\n      - {up_to: 100, unit_amount_cents: '5'}\n      - {up_to: null, unit_amount_cents: '1'}\n",
		"graduated, no last up_to": "    mode: graduated\n    tiers:\n      - {up_to: 100, unit_amount_cents: '5'}\n      - {unit_amount_cents: '1'}\n",
		"package at a decimal":     "    mode: package\n    package_size: 1000\n    package_amount_cents: '0.2'\n",
	} {
		if _, err := parseRecipe([]byte(withRule(rule))); err != nil {
			t.Errorf("%s: %v, want it to parse", name, err)
		}
	}
	for name, rule := range map[string]string{
		"mode flat":                  "    mode: flat\n    unit_amount_cents: '1'\n",
		"flat_amount_cents":          "    mode: per_unit\n    flat_amount_cents: '1'\n",
		"graduated_tiers":            "    mode: graduated\n    graduated_tiers:\n      - {up_to: 0, unit_amount_cents: '1'}\n",
		"overage_unit_amount_cents":  "    mode: package\n    package_size: 10\n    package_amount_cents: '1'\n    overage_unit_amount_cents: '1'\n",
		"a bounded last tier":        "    mode: graduated\n    tiers:\n      - {up_to: 100, unit_amount_cents: '5'}\n      - {up_to: 1000, unit_amount_cents: '1'}\n",
		"the last tier at up_to 0":   "    mode: graduated\n    tiers:\n      - {up_to: 100, unit_amount_cents: '5'}\n      - {up_to: 0, unit_amount_cents: '1'}\n",
		"an open tier before last":   "    mode: graduated\n    tiers:\n      - {up_to: null, unit_amount_cents: '5'}\n      - {up_to: 100, unit_amount_cents: '1'}\n",
		"no tiers":                   "    mode: graduated\n",
		"a package of size 0":        "    mode: package\n    package_size: 0\n    package_amount_cents: '1'\n",
		"a package amount 13 places": "    mode: package\n    package_size: 10\n    package_amount_cents: '0.0000000000001'\n",
	} {
		if _, err := parseRecipe([]byte(withRule(rule))); err == nil {
			t.Errorf("%s: parsed, want the load to fail", name)
		}
	}
}
