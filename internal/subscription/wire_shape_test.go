package subscription

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestWireShape_SnakeCase pins the JSON contract for billing-thresholds
// PATCH/GET surfaces. The cost dashboard's "Billing Thresholds" panel
// reads these keys directly; drift here (drop a json:"…" tag, or an
// empty slice marshalling as null) breaks the frontend at runtime, so
// we marshal the input + domain shapes and assert:
//
//   - Every documented snake_case key is present at the right level.
//   - No PascalCase keys leak through (json tag dropped off a field).
//   - usage_gte round-trips as a JSON string per ADR-005 — meter
//     quantities can be fractional NUMERIC(38,12) and JSON numbers
//     would lose precision on large values.
//   - item_thresholds[] marshals as a JSON array even when empty,
//     so the UI can iterate without a null guard.
//
// This is the merge gate for /v1/subscriptions/{id}/billing-thresholds.
// See docs/design/billing-thresholds.md.
func TestWireShape_SnakeCase(t *testing.T) {
	t.Run("BillingThresholdsInputFullyPopulated", func(t *testing.T) {
		reset := true
		body := BillingThresholdsInput{
			AmountGTE:         500000,
			ResetBillingCycle: &reset,
			ItemThresholds: []ItemThresholdInput{
				{
					SubscriptionItemID: "vlx_subitem_abc",
					UsageGTE:           "1000000.123456",
				},
			},
		}

		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		topLevel := []string{"amount_gte", "reset_billing_cycle", "item_thresholds"}
		for _, k := range topLevel {
			if _, ok := raw[k]; !ok {
				t.Errorf("input missing %q (raw keys=%v)", k, mapKeys(raw))
			}
		}
		// PascalCase leak guard
		for _, k := range []string{"AmountGTE", "ResetBillingCycle", "ItemThresholds"} {
			if _, ok := raw[k]; ok {
				t.Errorf("input leaked PascalCase key %q", k)
			}
		}

		items, ok := raw["item_thresholds"].([]any)
		if !ok {
			t.Fatalf("item_thresholds must marshal as array, got %T", raw["item_thresholds"])
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item threshold, got %d", len(items))
		}
		it0, ok := items[0].(map[string]any)
		if !ok {
			t.Fatalf("item_thresholds[0] not an object: %T", items[0])
		}
		for _, k := range []string{"subscription_item_id", "usage_gte"} {
			if _, ok := it0[k]; !ok {
				t.Errorf("item_thresholds[0] missing %q (keys=%v)", k, mapKeys(it0))
			}
		}
		// usage_gte on input is the JSON string form (decimal NUMERIC).
		if uStr, isString := it0["usage_gte"].(string); !isString {
			t.Errorf("usage_gte must marshal as JSON string on input, got %T", it0["usage_gte"])
		} else if uStr != "1000000.123456" {
			t.Errorf("usage_gte precision lost: got %q want %q", uStr, "1000000.123456")
		}
	})

	t.Run("DomainBillingThresholdsRoundTrip", func(t *testing.T) {
		// What gets returned from GET /subscriptions/{id} after a PATCH
		// — domain.BillingThresholds is what hydrates onto the row and
		// is rendered to the dashboard. Same snake_case, same always-array
		// idiom for item_thresholds, same string-precision usage_gte.
		bt := domain.BillingThresholds{
			AmountGTE:         500000,
			ResetBillingCycle: true,
			ItemThresholds: []domain.SubscriptionItemThreshold{
				{
					SubscriptionItemID: "vlx_subitem_abc",
					UsageGTE:           decimal.RequireFromString("1000000.123456"),
				},
			},
		}

		blob, err := json.Marshal(bt)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		for _, k := range []string{"amount_gte", "reset_billing_cycle", "item_thresholds"} {
			if _, ok := raw[k]; !ok {
				t.Errorf("billing_thresholds missing %q (keys=%v)", k, mapKeys(raw))
			}
		}
		for _, k := range []string{"AmountGTE", "ResetBillingCycle", "ItemThresholds"} {
			if _, ok := raw[k]; ok {
				t.Errorf("billing_thresholds leaked PascalCase key %q", k)
			}
		}

		items, ok := raw["item_thresholds"].([]any)
		if !ok {
			t.Fatalf("item_thresholds must marshal as JSON array, got %T", raw["item_thresholds"])
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item threshold, got %d", len(items))
		}
		it0 := items[0].(map[string]any)
		for _, k := range []string{"subscription_item_id", "usage_gte"} {
			if _, ok := it0[k]; !ok {
				t.Errorf("item_thresholds[0] missing %q (keys=%v)", k, mapKeys(it0))
			}
		}
		// usage_gte on the domain type is decimal.Decimal which marshals
		// as a string per shopspring/decimal MarshalJSON — same precision
		// guarantee as the input shape.
		uStr, isString := it0["usage_gte"].(string)
		if !isString {
			t.Errorf("domain usage_gte must marshal as JSON string, got %T", it0["usage_gte"])
		} else if uStr != "1000000.123456" {
			t.Errorf("usage_gte precision lost: got %q want %q", uStr, "1000000.123456")
		}
	})

	t.Run("EmptyItemThresholdsArrayNotNull", func(t *testing.T) {
		// When a tenant configures only an amount cap (no per-item caps),
		// item_thresholds must still serialize as []. Empty-state UI
		// iterates over the slice; null would crash.
		bt := domain.BillingThresholds{
			AmountGTE:         500000,
			ResetBillingCycle: true,
			ItemThresholds:    []domain.SubscriptionItemThreshold{},
		}
		blob, err := json.Marshal(bt)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var raw map[string]any
		if err := json.Unmarshal(blob, &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		arr, ok := raw["item_thresholds"].([]any)
		if !ok {
			t.Fatalf("item_thresholds must marshal as JSON array even when empty, got %T", raw["item_thresholds"])
		}
		if len(arr) != 0 {
			t.Errorf("item_thresholds should be empty in this fixture, got %d", len(arr))
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
