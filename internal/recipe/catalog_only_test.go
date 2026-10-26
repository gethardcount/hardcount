package recipe

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// catalogRecipe is the smallest recipe parseRecipe accepts: a meter and a
// plan with its prices (ADR-130 §15). The cases below change one line each.
const catalogRecipe = `key: catalog_check
version: 1.0.0
name: Catalog check
meters:
  - event_name: tokens
    name: Tokens
    unit: tokens
    aggregation: sum
plans:
  - code: basic
    name: Basic
    currency: USD
    billing_interval: monthly
    base_amount_cents: 1000
    base_bill_timing: in_advance
    prices:
      - meter: tokens
        dimension_match: { model: m1, token_type: input }
        mode: per_unit
        unit_amount_cents: '0.0003'
      - meter: tokens
        dimension_match: { model: m1, token_type: output }
        mode: per_unit
        unit_amount_cents: '0.0015'
`

// parsedJSON parses y and returns the recipe as its API JSON.
func parsedJSON(t *testing.T, y string) map[string]any {
	t.Helper()
	r, err := parseRecipe([]byte(y))
	if err != nil {
		t.Fatalf("parseRecipe: %v\n%s", err, y)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestParseRecipe_CatalogOnly: a recipe holds meters and plans with their
// prices, and nothing else (ADR-130 §15). Each price names its meter, its
// labels and its rate, and takes its plan's currency.
func TestParseRecipe_CatalogOnly(t *testing.T) {
	got := parsedJSON(t, catalogRecipe)
	plans, _ := got["plans"].([]any)
	if len(plans) != 1 {
		t.Fatalf("plans = %v, want one", got["plans"])
	}
	prices, _ := plans[0].(map[string]any)["prices"].([]any)
	if len(prices) != 2 {
		t.Fatalf("plan prices = %v, want two", plans[0])
	}
	first, _ := prices[0].(map[string]any)
	match, _ := first["dimension_match"].(map[string]any)
	if first["meter"] != "tokens" || first["mode"] != "per_unit" || first["unit_amount_cents"] != "0.0003" ||
		match["model"] != "m1" || match["token_type"] != "input" {
		t.Errorf("first price = %v", first)
	}
	for _, gone := range []string{"rating_rules", "pricing_rules", "dunning_policy", "webhook"} {
		if _, ok := got[gone]; ok {
			t.Errorf("a parsed recipe still has %q", gone)
		}
	}
}

// TestParseRecipe_RefusesRemovedSections: the sections a recipe no longer
// has fail the load with an error that names them, instead of installing
// part of what the author wrote. Recipes no longer create a dunning policy
// or a webhook endpoint (ADR-130 §15, approved 2026-10-23).
func TestParseRecipe_RefusesRemovedSections(t *testing.T) {
	if _, err := parseRecipe([]byte(catalogRecipe)); err != nil {
		t.Fatalf("control: the catalog recipe should parse, got %v", err)
	}
	cases := []struct{ name, yaml, want string }{
		{"dunning", catalogRecipe + "dunning:\n  policy:\n    name: Retry\n    retry_days: [1]\n    final_subscription_action: pause\n    final_invoice_action: none\n", "dunning"},
		{"webhook", catalogRecipe + "webhook:\n  events: [invoice.paid]\n  url_placeholder: 'https://example.com/hook'\n", "webhook"},
		{"rating_rules", catalogRecipe + "rating_rules:\n  - key: r\n    mode: per_unit\n    currency: USD\n    unit_amount_cents: '1'\n", "rating_rules"},
		{"pricing_rules", catalogRecipe + "pricing_rules:\n  - meter: tokens\n    rating_rule: r\n    dimension_match: {}\n    priority: 100\n", "pricing_rules"},
		{"plan meters", strings.Replace(catalogRecipe, "    prices:\n", "    meters: [tokens]\n    prices:\n", 1), "meters"},
		{"price priority", strings.Replace(catalogRecipe, "        unit_amount_cents: '0.0003'\n", "        unit_amount_cents: '0.0003'\n        priority: 100\n", 1), "priority"},
		{"price currency", strings.Replace(catalogRecipe, "        unit_amount_cents: '0.0003'\n", "        unit_amount_cents: '0.0003'\n        currency: USD\n", 1), "currency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRecipe([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("parseRecipe accepted it:\n%s", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %q, got %v", tc.want, err)
			}
		})
	}
}

// TestParseRecipe_PricesHaveAWinner: the parser runs the tie check the
// price writer runs (domain.PricesTie), so a recipe whose prices could both
// take one event with the same number of labels fails the load, naming both,
// instead of failing every install with 409 price_conflict. A price on a
// meter the recipe does not declare, an empty label and a label that is not
// a string fail too.
func TestParseRecipe_PricesHaveAWinner(t *testing.T) {
	if _, err := parseRecipe([]byte(catalogRecipe)); err != nil {
		t.Fatalf("control: the catalog recipe should parse, got %v", err)
	}
	moreLabels := strings.Replace(catalogRecipe, "{ model: m1, token_type: output }", "{ model: m1, token_type: input, context_tier: long }", 1)
	if _, err := parseRecipe([]byte(moreLabels)); err != nil {
		t.Fatalf("a price with one more label than another has a winner, got %v", err)
	}
	cases := []struct{ name, from, to, want string }{
		{"two labels each, could both match", "{ model: m1, token_type: output }", "{ model_raw: x, token_type: input }", "model_raw"},
		{"the same labels", "{ model: m1, token_type: output }", "{ token_type: input, model: m1 }", "same labels"},
		{"unknown meter", "      - meter: tokens\n        dimension_match: { model: m1, token_type: output }", "      - meter: calls\n        dimension_match: { model: m1, token_type: output }", "calls"},
		{"empty label value", "{ model: m1, token_type: output }", "{ model: m1, token_type: '' }", "token_type"},
		{"number label value", "{ model: m1, token_type: output }", "{ model: m1, token_type: 5 }", "token_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := strings.Replace(catalogRecipe, tc.from, tc.to, 1)
			if y == catalogRecipe {
				t.Fatalf("fixture: %q not found", tc.from)
			}
			_, err := parseRecipe([]byte(y))
			if err == nil {
				t.Fatalf("parseRecipe accepted it:\n%s", y)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %q, got %v", tc.want, err)
			}
		})
	}
}

// TestEmbeddedRecipes_CatalogOnly: every built-in recipe holds only the
// sections a recipe has, so none of them creates a dunning policy or a
// webhook endpoint (ADR-130 §15).
func TestEmbeddedRecipes_CatalogOnly(t *testing.T) {
	allowed := map[string]bool{
		"key": true, "version": true, "name": true, "summary": true, "description": true,
		"overridable": true, "meters": true, "plans": true,
	}
	entries, err := recipeFS.ReadDir("recipes")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := recipeFS.ReadFile("recipes/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var top map[string]any
		if err := yaml.Unmarshal(raw, &top); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		for k := range top {
			if !allowed[k] {
				t.Errorf("%s has a %q section", e.Name(), k)
			}
		}
	}
}

// TestPreview_ListsTheCatalog: the preview lists what an install creates,
// the meters and each plan with its prices, and nothing else.
func TestPreview_ListsTheCatalog(t *testing.T) {
	svc := NewService(nil, newMemStore(), loadRegistry(t), nil, nil, nil)
	out, err := svc.Preview(t.Context(), "replicate_style", nil)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Objects map[string]json.RawMessage `json:"objects"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(raw.Objects))
	for k := range raw.Objects {
		keys = append(keys, k)
	}
	if len(raw.Objects) != 2 || raw.Objects["meters"] == nil || raw.Objects["plans"] == nil {
		t.Fatalf("preview objects = %v, want meters and plans only", keys)
	}
	var plans []struct {
		Prices []struct {
			Meter string `json:"meter"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(raw.Objects["plans"], &plans); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Prices) != 5 || plans[0].Prices[0].Meter != "gpu_seconds" {
		t.Errorf("preview plans = %s, want one plan with the 5 GPU prices", raw.Objects["plans"])
	}
}
