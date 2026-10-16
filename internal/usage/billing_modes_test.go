package usage

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The Usage page's per-meter number follows the mode billing uses, which
// lives on each pricing rule (meters.aggregation is advisory since 0054).
// A meter whose events bill under more than one mode reads "mixed" and
// shows the raw sum, so no number claims a mode billing doesn't use.
func TestBillingModes(t *testing.T) {
	meters := []domain.Meter{
		{ID: "no_rules", Aggregation: "max"},
		{ID: "rule_overrides", Aggregation: "sum"},
		{ID: "two_modes", Aggregation: "sum"},
		{ID: "same_mode", Aggregation: "sum"},
		{ID: "unclaimed_differs", Aggregation: "sum"},
		{ID: "latest", Aggregation: "sum"},
		{ID: "max_two_rules", Aggregation: "sum"},
		{ID: "max_rule_plus_unclaimed", Aggregation: "max"},
		{ID: "last_rule_plus_unclaimed", Aggregation: "last"},
		{ID: "max_catch_all_only", Aggregation: "sum"},
		{ID: "count_two_rules", Aggregation: "sum"},
	}
	rules := []domain.MeterPricingRule{
		{MeterID: "rule_overrides", AggregationMode: domain.AggMax},
		{MeterID: "two_modes", AggregationMode: domain.AggMax, DimensionMatch: map[string]any{"tier": "gpu"}},
		{MeterID: "two_modes", AggregationMode: domain.AggSum},
		{MeterID: "same_mode", AggregationMode: domain.AggSum, DimensionMatch: map[string]any{"model": "a"}},
		{MeterID: "same_mode", AggregationMode: domain.AggSum, DimensionMatch: map[string]any{"model": "b"}},
		// No catch-all rule: unclaimed events bill under the meter's sum.
		{MeterID: "unclaimed_differs", AggregationMode: domain.AggCount, DimensionMatch: map[string]any{"model": "a"}},
		{MeterID: "latest", AggregationMode: domain.AggLastDuringPeriod},
		// Peaks per bucket, added up, are not the peak: several buckets of a
		// max or last meter are mixed even when every bucket uses that mode.
		{MeterID: "max_two_rules", AggregationMode: domain.AggMax, DimensionMatch: map[string]any{"region": "eu"}},
		{MeterID: "max_two_rules", AggregationMode: domain.AggMax},
		{MeterID: "max_rule_plus_unclaimed", AggregationMode: domain.AggMax, DimensionMatch: map[string]any{"region": "eu"}},
		{MeterID: "last_rule_plus_unclaimed", AggregationMode: domain.AggLastEver, DimensionMatch: map[string]any{"region": "eu"}},
		{MeterID: "max_catch_all_only", AggregationMode: domain.AggMax},
		// Counts add up across buckets, so several count rules stay "count".
		{MeterID: "count_two_rules", AggregationMode: domain.AggCount, DimensionMatch: map[string]any{"model": "a"}},
		{MeterID: "count_two_rules", AggregationMode: domain.AggCount},
	}
	want := map[string]string{
		"no_rules":          "max",
		"rule_overrides":    "max",
		"two_modes":         AggregationMixed,
		"same_mode":         "sum",
		"unclaimed_differs": AggregationMixed,
		"latest":            "last",

		"max_two_rules":            AggregationMixed,
		"max_rule_plus_unclaimed":  AggregationMixed,
		"last_rule_plus_unclaimed": AggregationMixed,
		"max_catch_all_only":       "max",
		"count_two_rules":          "count",
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
