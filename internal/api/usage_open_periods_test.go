package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
)

// openPeriodsSubs answers one active subscription with two items, one of
// them with a plan change pending at the period end.
type openPeriodsSubs struct{ start, end time.Time }

func (s openPeriodsSubs) ListLiveByCustomer(context.Context, string, string) ([]domain.Subscription, error) {
	return []domain.Subscription{{
		ID: "sub_open", Status: domain.SubscriptionActive,
		CurrentBillingPeriodStart: &s.start, CurrentBillingPeriodEnd: &s.end, StartedAt: &s.start,
		Items: []domain.SubscriptionItem{
			{PlanID: "pln_a", PendingPlanID: "pln_b", PendingPlanEffectiveAt: &s.end},
			{PlanID: "pln_c"},
		},
	}}, nil
}

type noWatermarks struct{}

func (noWatermarks) ThresholdWatermarks(context.Context, string, string, time.Time, time.Time) (*time.Time, *time.Time, error) {
	return nil, nil, nil
}

// countingPricing counts the reads of plan prices. pln_a prices mtr_a from
// the period start on and mtr_a2 from its end; pln_b prices mtr_b; pln_c
// prices mtr_c.
type countingPricing struct {
	end   time.Time
	reads *int
}

func (p countingPricing) GetMeterTx(context.Context, *sql.Tx, string) (domain.Meter, error) {
	panic("not read by OpenPeriods")
}

func (p countingPricing) meterAt(planID string, at time.Time) string {
	switch {
	case planID == "pln_a" && at.Before(p.end):
		return "mtr_a"
	case planID == "pln_a":
		return "mtr_a2"
	case planID == "pln_b":
		return "mtr_b"
	default:
		return "mtr_c"
	}
}

func (p countingPricing) MetersPricedAt(_ context.Context, _ string, pins []pricing.PlanPin) ([][]string, error) {
	*p.reads++
	out := make([][]string, len(pins))
	for i, pin := range pins {
		out[i] = []string{p.meterAt(pin.PlanID, pin.At)}
	}
	return out, nil
}

// Ingest reads a customer's open periods on every event. The meters each
// plan prices at each pin are one read, however many items, pins and pending
// changes the subscriptions have.
//
// Mutation: read each (plan, pin) on its own, and the subscription below
// costs five reads.
func TestUsageOpenPeriods_ReadsPlanMetersOnce(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	reads := 0
	a := usageOpenPeriods{
		subs:       openPeriodsSubs{start: start, end: end},
		pricing:    countingPricing{end: end, reads: &reads},
		thresholds: noWatermarks{},
	}
	got, err := a.OpenPeriods(context.Background(), "ten_open", "cus_open")
	if err != nil {
		t.Fatalf("OpenPeriods: %v", err)
	}
	if reads != 1 {
		t.Errorf("plan price reads = %d, want 1", reads)
	}
	if len(got) != 1 {
		t.Fatalf("open periods = %d, want 1", len(got))
	}
	p := got[0]
	for _, m := range []string{"mtr_a", "mtr_c"} {
		if !p.Meters[m] {
			t.Errorf("Meters = %v, want %s (priced at the period start)", p.Meters, m)
		}
	}
	if len(p.Meters) != 2 {
		t.Errorf("Meters = %v, want mtr_a and mtr_c", p.Meters)
	}
	for _, m := range []string{"mtr_b", "mtr_c"} {
		if !p.NextMeters[m] {
			t.Errorf("NextMeters = %v, want %s (priced from the period end)", p.NextMeters, m)
		}
	}
	if len(p.NextMeters) != 2 {
		t.Errorf("NextMeters = %v, want mtr_b and mtr_c", p.NextMeters)
	}
}
