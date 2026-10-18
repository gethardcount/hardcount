package billing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// TestWireShape_SnakeCase pins the JSON contract Track B's cost dashboard
// + projected-bill panel depend on. Drift here (e.g. dropping a json:"…"
// tag and falling back to PascalCase, or an empty slice marshalling as
// null) breaks the frontend at runtime, so we marshal a fully-populated
// PreviewResult and assert:
//
//   - Every documented snake_case key is present at the right level.
//   - No PascalCase keys leak through (caught by the json:"…" tags
//     dropping off a field, which has happened before).
//   - Slice fields marshal as JSON arrays even when empty — clients can
//     iterate without null guards.
//   - Decimal quantity round-trips as a string (per ADR-005), not a
//     JSON number that would lose precision on large fractional values.
//
// This is the merge gate for /v1/invoices/create_preview. See
// docs/design/create-preview.md.
func TestWireShape_SnakeCase(t *testing.T) {
	t.Run("FullyPopulated", func(t *testing.T) {
		result := PreviewResult{
			CustomerID:         "vlx_cus_abc",
			SubscriptionID:     "vlx_sub_xyz",
			PlanName:           "AI API Pro",
			BillingPeriodStart: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			BillingPeriodEnd:   time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			Lines: []PreviewLine{
				{
					LineType:        "base_fee",
					Description:     "AI API Pro - base fee (qty 1)",
					Currency:        "USD",
					Quantity:        decimal.NewFromInt(1),
					UnitAmountCents: 0,
					AmountCents:     0,
				},
				{
					LineType:            "usage",
					Description:         "Tokens - input (cached)",
					MeterID:             "vlx_mtr_tokens",
					RatingRuleVersionID: "vlx_rrv_input",
					RuleKey:             "gpt4_input_uncached",
					DimensionMatch:      map[string]any{"model": "gpt-4", "operation": "input", "cached": false},
					Currency:            "USD",
					Quantity:            decimal.RequireFromString("1234567.891234"),
					UnitAmountCents:     0,
					AmountCents:         3000,
					PricingMode:         "flat",
				},
			},
			Totals: []PreviewTotal{
				{Currency: "USD", AmountCents: 3000},
			},
			Warnings:    []string{},
			GeneratedAt: time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC),
		}

		blob, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		// Top-level snake_case keys — the cost dashboard reads every one.
		topLevel := []string{
			"customer_id",
			"subscription_id",
			"plan_name",
			"billing_period_start",
			"billing_period_end",
			"lines",
			"totals",
			"warnings",
			"generated_at",
		}
		for _, k := range topLevel {
			if _, ok := raw[k]; !ok {
				t.Errorf("response missing %q (raw keys=%v)", k, mapKeys(raw))
			}
		}

		// Forbidden PascalCase keys at top level. Drop a json tag and
		// these would slip through.
		for _, k := range []string{
			"CustomerID", "SubscriptionID", "PlanName",
			"BillingPeriodStart", "BillingPeriodEnd",
			"Lines", "Totals", "Warnings", "GeneratedAt",
		} {
			if _, ok := raw[k]; ok {
				t.Errorf("response leaked PascalCase key %q", k)
			}
		}

		// lines[] must be a JSON array, never null.
		linesAny, ok := raw["lines"].([]any)
		if !ok {
			t.Fatalf("lines must marshal as JSON array, got %T", raw["lines"])
		}
		if len(linesAny) != 2 {
			t.Fatalf("expected 2 lines, got %d", len(linesAny))
		}

		// Per-line snake_case checks. The cost dashboard's per-rule
		// breakdown reads every key on the usage line.
		baseLine, ok := linesAny[0].(map[string]any)
		if !ok {
			t.Fatalf("base line not an object: %T", linesAny[0])
		}
		for _, k := range []string{"line_type", "description", "currency", "quantity", "unit_amount_cents", "amount_cents"} {
			if _, ok := baseLine[k]; !ok {
				t.Errorf("base line missing %q (keys=%v)", k, mapKeys(baseLine))
			}
		}
		// Optional fields on a base_fee line: meter_id, rating_rule_version_id,
		// rule_key, dimension_match, pricing_mode. They should be omitted
		// (omitempty) when empty rather than marshaled as zero values.
		for _, k := range []string{"meter_id", "rating_rule_version_id", "rule_key", "dimension_match", "pricing_mode"} {
			if _, ok := baseLine[k]; ok {
				t.Errorf("base_fee line should omit empty %q (got %v)", k, baseLine[k])
			}
		}

		usageLine, ok := linesAny[1].(map[string]any)
		if !ok {
			t.Fatalf("usage line not an object: %T", linesAny[1])
		}
		for _, k := range []string{
			"line_type", "description", "meter_id", "rating_rule_version_id",
			"rule_key", "dimension_match", "currency", "quantity",
			"unit_amount_cents", "amount_cents", "pricing_mode",
		} {
			if _, ok := usageLine[k]; !ok {
				t.Errorf("usage line missing %q (keys=%v)", k, mapKeys(usageLine))
			}
		}
		// quantity must be a JSON string per ADR-005 — large fractional
		// AI-usage primitives (GPU-hours, cached-token ratios) lose
		// precision if marshaled as JSON number. shopspring/decimal
		// normalizes trailing zeros in MarshalJSON, so we round-trip
		// the meaningful digits and assert exact equality.
		qtyStr, isString := usageLine["quantity"].(string)
		if !isString {
			t.Fatalf("quantity must marshal as JSON string (decimal precision), got %T", usageLine["quantity"])
		}
		if qtyStr != "1234567.891234" {
			t.Errorf("quantity precision lost: got %q want %q", qtyStr, "1234567.891234")
		}
		// dimension_match must be a JSON object — preserves the rule's
		// match expression for multi-dim drill-down.
		if _, isMap := usageLine["dimension_match"].(map[string]any); !isMap {
			t.Errorf("dimension_match must marshal as JSON object, got %T", usageLine["dimension_match"])
		}

		// totals[] is always-array; one entry per currency.
		totalsAny, ok := raw["totals"].([]any)
		if !ok {
			t.Fatalf("totals must marshal as JSON array, got %T", raw["totals"])
		}
		if len(totalsAny) != 1 {
			t.Fatalf("expected 1 total entry, got %d", len(totalsAny))
		}
		total0, ok := totalsAny[0].(map[string]any)
		if !ok {
			t.Fatalf("totals[0] not an object: %T", totalsAny[0])
		}
		for _, k := range []string{"currency", "amount_cents"} {
			if _, ok := total0[k]; !ok {
				t.Errorf("totals[0] missing %q (keys=%v)", k, mapKeys(total0))
			}
		}

		// warnings[] empty must be [] not null. Same idiom as
		// customer-usage so the dashboard's per-meter warning rendering
		// can iterate without a null guard.
		warnings, ok := raw["warnings"].([]any)
		if !ok {
			t.Fatalf("warnings must marshal as JSON array even when empty, got %T", raw["warnings"])
		}
		if len(warnings) != 0 {
			t.Errorf("warnings should be empty in this fixture, got %d", len(warnings))
		}
	})

	t.Run("EmptyResultSlicesAreArrays", func(t *testing.T) {
		// A result with zero lines / zero totals / zero warnings still
		// must marshal to []s, not nulls — empty-state UI iterates over
		// the slices directly and would crash on a null.
		result := PreviewResult{
			CustomerID:     "vlx_cus_abc",
			SubscriptionID: "vlx_sub_xyz",
			Lines:          []PreviewLine{},
			Totals:         []PreviewTotal{},
			Warnings:       []string{},
		}

		blob, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		for _, k := range []string{"lines", "totals", "warnings"} {
			arr, ok := raw[k].([]any)
			if !ok {
				t.Errorf("%q must marshal as JSON array even when empty, got %T", k, raw[k])
				continue
			}
			if len(arr) != 0 {
				t.Errorf("%q should be empty in this fixture, got %d entries", k, len(arr))
			}
		}
	})
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
