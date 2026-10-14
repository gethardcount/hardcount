package billing

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

func unpricedSeries(t *testing.T, meterID, mode string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "velox_usage_unpriced_quantity_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["meter_id"] == meterID && labels["mode"] == mode {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The counter tells live from test mode, and leaves test-clock simulations
// out (one clock advance can close many periods), like the other billing
// metrics. Without that, the runbook's alert fires on test experiments.
func TestUnpricedRecord_ModeLabelAndTestClockSkip(t *testing.T) {
	u := unpricedUsage{}
	u.add("mtr_unpriced_rec_live", decimal.NewFromInt(7))
	u.record(postgres.WithLivemode(context.Background(), true), domain.Subscription{ID: "sub_live"})
	if got := unpricedSeries(t, "mtr_unpriced_rec_live", "live"); got != 7 {
		t.Fatalf("live: counter = %v, want 7", got)
	}
	if got := unpricedSeries(t, "mtr_unpriced_rec_live", "test"); got != 0 {
		t.Fatalf("live usage counted under mode=test: %v", got)
	}

	u = unpricedUsage{}
	u.add("mtr_unpriced_rec_test", decimal.NewFromInt(3))
	ctxTest := postgres.WithLivemode(context.Background(), false)
	u.record(ctxTest, domain.Subscription{ID: "sub_test"})
	if got := unpricedSeries(t, "mtr_unpriced_rec_test", "test"); got != 3 {
		t.Fatalf("test mode: counter = %v, want 3", got)
	}

	u = unpricedUsage{}
	u.add("mtr_unpriced_rec_clock", decimal.NewFromInt(5))
	u.record(ctxTest, domain.Subscription{ID: "sub_clock", TestClockID: "clk_1"})
	if got := unpricedSeries(t, "mtr_unpriced_rec_clock", "test"); got != 0 {
		t.Fatalf("test-clock subscription counted: %v, want 0", got)
	}
}
