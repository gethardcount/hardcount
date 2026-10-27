package recipe

import (
	"strings"
	"testing"
)

// withPrice returns catalogRecipe with its first price's shape replaced by
// shape (YAML lines indented for a price).
func withPrice(shape string) string {
	return strings.Replace(catalogRecipe, "        mode: per_unit\n        unit_amount_cents: '0.0003'\n", shape, 1)
}

// TestParseRecipe_RefusesRemovedMeterFields: a meter is named by event_name,
// not key, and a price has no aggregation of its own (ADR-130 §2). A recipe
// that still uses either fails the load instead of installing a meter
// without its event name or a price whose aggregation is ignored.
func TestParseRecipe_RefusesRemovedMeterFields(t *testing.T) {
	for name, yaml := range map[string]string{
		"meter key":                 strings.Replace(catalogRecipe, "  - event_name: tokens", "  - key: tokens", 1),
		"price aggregation":         withPrice("        mode: per_unit\n        unit_amount_cents: '0.0003'\n        aggregation_mode: sum\n"),
		"meter aggregation unknown": strings.Replace(catalogRecipe, "    aggregation: sum", "    aggregation: last_ever", 1),
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
	if _, err := parseRecipe([]byte(catalogRecipe)); err != nil {
		t.Fatalf("control: catalog recipe should parse, got %v", err)
	}

	cases := []struct {
		name, from, to string
	}{
		{"top level", "name: Catalog check", "nmae: Catalog check"},
		{"plan", "base_bill_timing: in_advance", "base_bill_timming: in_advance"},
		{"price", "unit_amount_cents: '0.0003'", "unit_amount_cent: '0.0003'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typo := strings.Replace(catalogRecipe, tc.from, tc.to, 1)
			if typo == catalogRecipe {
				t.Fatalf("fixture: %q not found in catalogRecipe", tc.from)
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

// TestParseRecipe_PriceShapes: a recipe's prices take ADR-130 §3's shapes
// and words. A graduated price's last tier is open (`up_to: null`, or no
// up_to) and only the last; a package has a size above 0 and a decimal
// amount. The old words, `flat`, flat_amount_cents, graduated_tiers and
// overage_unit_amount_cents, fail the load, and so does a tier table that
// could not rate, before any install writes it.
func TestParseRecipe_PriceShapes(t *testing.T) {
	for name, shape := range map[string]string{
		"per_unit":                 "        mode: per_unit\n        unit_amount_cents: '0.0035'\n",
		"graduated, up_to null":    "        mode: graduated\n        tiers:\n          - {up_to: 100, unit_amount_cents: '5'}\n          - {up_to: null, unit_amount_cents: '1'}\n",
		"graduated, no last up_to": "        mode: graduated\n        tiers:\n          - {up_to: 100, unit_amount_cents: '5'}\n          - {unit_amount_cents: '1'}\n",
		"package at a decimal":     "        mode: package\n        package_size: 1000\n        package_amount_cents: '0.2'\n",
	} {
		if _, err := parseRecipe([]byte(withPrice(shape))); err != nil {
			t.Errorf("%s: %v, want it to parse", name, err)
		}
	}
	for name, shape := range map[string]string{
		"mode flat":                  "        mode: flat\n        unit_amount_cents: '1'\n",
		"no mode":                    "        unit_amount_cents: '1'\n",
		"flat_amount_cents":          "        mode: per_unit\n        flat_amount_cents: '1'\n",
		"graduated_tiers":            "        mode: graduated\n        graduated_tiers:\n          - {up_to: 0, unit_amount_cents: '1'}\n",
		"overage_unit_amount_cents":  "        mode: package\n        package_size: 10\n        package_amount_cents: '1'\n        overage_unit_amount_cents: '1'\n",
		"a bounded last tier":        "        mode: graduated\n        tiers:\n          - {up_to: 100, unit_amount_cents: '5'}\n          - {up_to: 1000, unit_amount_cents: '1'}\n",
		"the last tier at up_to 0":   "        mode: graduated\n        tiers:\n          - {up_to: 100, unit_amount_cents: '5'}\n          - {up_to: 0, unit_amount_cents: '1'}\n",
		"an open tier before last":   "        mode: graduated\n        tiers:\n          - {up_to: null, unit_amount_cents: '5'}\n          - {up_to: 100, unit_amount_cents: '1'}\n",
		"no tiers":                   "        mode: graduated\n",
		"a package of size 0":        "        mode: package\n        package_size: 0\n        package_amount_cents: '1'\n",
		"a package amount 13 places": "        mode: package\n        package_size: 10\n        package_amount_cents: '0.0000000000001'\n",
		// A price carries only its mode's fields.
		"per_unit with tiers":          "        mode: per_unit\n        unit_amount_cents: '1'\n        tiers:\n          - {up_to: 100, unit_amount_cents: '5'}\n          - {up_to: null, unit_amount_cents: '1'}\n",
		"per_unit with a package size": "        mode: per_unit\n        unit_amount_cents: '1'\n        package_size: 100\n",
		"graduated with a unit amount": "        mode: graduated\n        unit_amount_cents: '1'\n        tiers:\n          - {up_to: null, unit_amount_cents: '1'}\n",
		"package with tiers":           "        mode: package\n        package_size: 10\n        package_amount_cents: '1'\n        tiers:\n          - {up_to: null, unit_amount_cents: '1'}\n",
	} {
		if _, err := parseRecipe([]byte(withPrice(shape))); err == nil {
			t.Errorf("%s: parsed, want the load to fail", name)
		}
	}
}
