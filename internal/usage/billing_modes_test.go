package usage

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The Usage page's per-meter number follows the aggregation billing uses,
// the meter's own (ADR-130 §2). A max or last meter billed in several
// buckets reads "mixed" and shows the raw sum: billing adds each bucket's
// peak or latest value, which is not the meter's peak or latest value, so no
// number claims a mode billing doesn't use.
func TestBillingModes(t *testing.T) {
	meters := []domain.Meter{
		{ID: "no_rules", Aggregation: "max"},
		{ID: "sum_rules", Aggregation: "sum"},
		{ID: "latest", Aggregation: "last"},
		{ID: "max_two_rules", Aggregation: "max"},
		{ID: "max_rule_plus_unclaimed", Aggregation: "max"},
		{ID: "last_rule_plus_unclaimed", Aggregation: "last"},
		{ID: "max_catch_all_only", Aggregation: "max"},
		{ID: "count_two_rules", Aggregation: "count"},
		{ID: "unknown", Aggregation: ""},
	}
	rules := []domain.MeterPricingRule{
		{MeterID: "sum_rules", DimensionMatch: map[string]any{"model": "a"}},
		{MeterID: "sum_rules", DimensionMatch: map[string]any{"model": "b"}},
		{MeterID: "latest"},
		{MeterID: "max_two_rules", DimensionMatch: map[string]any{"region": "eu"}},
		{MeterID: "max_two_rules"},
		{MeterID: "max_rule_plus_unclaimed", DimensionMatch: map[string]any{"region": "eu"}},
		{MeterID: "last_rule_plus_unclaimed", DimensionMatch: map[string]any{"region": "eu"}},
		{MeterID: "max_catch_all_only"},
		// Counts add up across buckets, so several count rules stay "count".
		{MeterID: "count_two_rules", DimensionMatch: map[string]any{"model": "a"}},
		{MeterID: "count_two_rules"},
	}
	want := map[string]string{
		"no_rules":                 "max",
		"sum_rules":                "sum",
		"latest":                   "last",
		"max_two_rules":            AggregationMixed,
		"max_rule_plus_unclaimed":  AggregationMixed,
		"last_rule_plus_unclaimed": AggregationMixed,
		"max_catch_all_only":       "max",
		"count_two_rules":          "count",
		"unknown":                  "sum",
	}
	got := BillingModes(meters, rules)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: mode = %q, want %q", id, got[id], w)
		}
	}
}

// A mixed meter shows the plain sum, day by day too.
func TestApplyMeterModes_MixedIsTheRawSum(t *testing.T) {
	agg := Aggregate{ByMeter: []MeterTotal{{
		MeterID: "m", Total: decimal.NewFromInt(470), Events: 3,
		sumOfCustomerMax: decimal.NewFromInt(210), sumOfCustomerLast: decimal.NewFromInt(120),
		Daily: []DailyValue{{Date: "2026-10-01", sum: decimal.NewFromInt(470), count: decimal.NewFromInt(3), sumOfCustomerMax: decimal.NewFromInt(210)}},
	}}}
	agg.ApplyMeterModes(map[string]string{"m": AggregationMixed})
	m := agg.ByMeter[0]
	if m.Aggregation != AggregationMixed || !m.Value.Equal(decimal.NewFromInt(470)) || !m.Daily[0].Value.Equal(decimal.NewFromInt(470)) {
		t.Fatalf("mixed row = %q %s / day %s, want mixed 470 / 470", m.Aggregation, m.Value, m.Daily[0].Value)
	}
}
