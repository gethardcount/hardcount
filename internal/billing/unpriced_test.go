package billing

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
)

func unpricedSeries(t *testing.T, mode string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "hardcount_usage_unpriced_quantity_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "mode" && lp.GetValue() == mode {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// The counter tells live from test mode, and leaves test-clock simulations
// out (one clock advance can close many periods), like the other billing
// metrics. Without that, the runbook's alert fires on test experiments.
func TestUnpricedRecord_ModeLabelAndTestClockSkip(t *testing.T) {
	live, test := unpricedSeries(t, "live"), unpricedSeries(t, "test")

	u := unpricedUsage{}
	u.add("mtr_unpriced_rec_live", decimal.NewFromInt(7))
	u.record(true, domain.Subscription{ID: "sub_live"})
	if got := unpricedSeries(t, "live") - live; got != 7 {
		t.Fatalf("live: counter grew by %v, want 7", got)
	}
	if got := unpricedSeries(t, "test") - test; got != 0 {
		t.Fatalf("live usage counted under mode=test: %v", got)
	}

	u = unpricedUsage{}
	u.add("mtr_unpriced_rec_test", decimal.NewFromInt(3))
	u.record(false, domain.Subscription{ID: "sub_test"})
	if got := unpricedSeries(t, "test") - test; got != 3 {
		t.Fatalf("test mode: counter grew by %v, want 3", got)
	}

	u = unpricedUsage{}
	u.add("mtr_unpriced_rec_clock", decimal.NewFromInt(5))
	u.record(false, domain.Subscription{ID: "sub_clock", TestClockID: "clk_1"})
	if got := unpricedSeries(t, "test") - test; got != 3 {
		t.Fatalf("test-clock subscription counted: grew by %v, want 3", got)
	}
}

// The counter has one series per mode, whatever the meters: a label per meter
// would add a series for every meter of every account, and none is ever
// dropped.
func TestUnpricedRecord_OneSeriesPerMode(t *testing.T) {
	u := unpricedUsage{}
	u.add("mtr_unpriced_one_a", decimal.NewFromInt(1))
	u.add("mtr_unpriced_one_b", decimal.NewFromInt(2))
	u.record(true, domain.Subscription{ID: "sub_one"})

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "hardcount_usage_unpriced_quantity_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if ls := m.GetLabel(); len(ls) != 1 || ls[0].GetName() != "mode" {
				t.Errorf("series labels = %v, want mode alone", ls)
			}
		}
	}
}
