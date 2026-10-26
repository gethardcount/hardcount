package api

import (
	"context"
	"database/sql"
	"errors"
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

// ListItemIntervals is not read: every item joined its plan before the
// period.
func (s openPeriodsSubs) ListItemIntervals(context.Context, string, string) ([]domain.ItemInterval, error) {
	return nil, errors.New("billing intervals read for a subscription whose items joined before the period")
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

// joinedSubs answers two active subscriptions. sub_new is in its first
// period, which started at midnight; the subscription started at 15:00. Its
// item it_start came with it, it_added was added at 18:00 and it_swapped was
// swapped onto its plan on the 10th. Its item it_gone, on pln_back since the
// start, was removed at 16:00, and it_back was added on pln_back at 20:00.
// sub_old is in a later period, and every item of it joined its plan before
// that period started.
type joinedSubs struct {
	start, end time.Time
	reads      map[string]int // billing-interval reads, by subscription
}

func (s joinedSubs) times() (started, gone, added, back, swapped time.Time) {
	return s.start.Add(15 * time.Hour), s.start.Add(16 * time.Hour), s.start.Add(18 * time.Hour), s.start.Add(20 * time.Hour), s.start.AddDate(0, 0, 9).Add(12 * time.Hour)
}

func (s joinedSubs) ListLiveByCustomer(context.Context, string, string) ([]domain.Subscription, error) {
	started, _, added, back, swapped := s.times()
	earlier := s.start.AddDate(0, -2, 0)
	return []domain.Subscription{{
		ID: "sub_new", Status: domain.SubscriptionActive, CreatedAt: started,
		CurrentBillingPeriodStart: &s.start, CurrentBillingPeriodEnd: &s.end, StartedAt: &started,
		Items: []domain.SubscriptionItem{
			{ID: "it_start", PlanID: "pln_start", CreatedAt: started},
			{ID: "it_added", PlanID: "pln_added", CreatedAt: added},
			{ID: "it_swapped", PlanID: "pln_swapped", CreatedAt: started, PlanChangedAt: &swapped},
			{ID: "it_back", PlanID: "pln_back", CreatedAt: back},
		},
	}, {
		ID: "sub_old", Status: domain.SubscriptionActive, CreatedAt: earlier,
		CurrentBillingPeriodStart: &s.start, CurrentBillingPeriodEnd: &s.end, StartedAt: &earlier,
		Items: []domain.SubscriptionItem{
			{ID: "it_old", PlanID: "pln_old", CreatedAt: earlier.Add(time.Hour), PlanChangedAt: &earlier},
		},
	}}, nil
}

// ListItemIntervals answers sub_new's billing intervals. A first-day add
// opens its interval at the period start (ADR-012).
func (s joinedSubs) ListItemIntervals(_ context.Context, _, subID string) ([]domain.ItemInterval, error) {
	s.reads[subID]++
	if subID != "sub_new" {
		return nil, errors.New("billing intervals read for a subscription whose items joined before the period")
	}
	started, gone, added, back, swapped := s.times()
	return []domain.ItemInterval{
		{SubscriptionItemID: "it_start", PlanID: "pln_start", Quantity: 1, StartsAt: s.start, ItemCreatedAt: started},
		{SubscriptionItemID: "it_added", PlanID: "pln_added", Quantity: 1, StartsAt: s.start, ItemCreatedAt: added},
		{SubscriptionItemID: "it_swapped", PlanID: "pln_prev", Quantity: 1, StartsAt: s.start, EndsAt: &swapped, ItemCreatedAt: started},
		{SubscriptionItemID: "it_swapped", PlanID: "pln_swapped", Quantity: 1, StartsAt: swapped, ItemCreatedAt: started},
		{SubscriptionItemID: "it_gone", PlanID: "pln_back", Quantity: 1, StartsAt: s.start, EndsAt: &gone, ItemCreatedAt: started},
		{SubscriptionItemID: "it_back", PlanID: "pln_back", Quantity: 1, StartsAt: s.start, ItemCreatedAt: back},
	}, nil
}

// pinRecorder records each pin read before the period end, by plan.
type pinRecorder struct {
	end  time.Time
	pins map[string]time.Time
}

func (p pinRecorder) GetMeterTx(context.Context, *sql.Tx, string) (domain.Meter, error) {
	panic("not read by OpenPeriods")
}

func (p pinRecorder) MetersPricedAt(_ context.Context, _ string, pins []pricing.PlanPin) ([][]string, error) {
	out := make([][]string, len(pins))
	for i, pin := range pins {
		if pin.At.Before(p.end) {
			p.pins[pin.PlanID] = pin.At
		}
		out[i] = []string{"mtr_" + pin.PlanID}
	}
	return out, nil
}

// Ingest reads the meters of each current plan at its pin of the period,
// the one the close prices it at (ADR-130 §4 step 3, amended 2026-10-26): the
// first instant in the period the subscription had an item on the plan, a
// subscription's start, an add, a swap. A plan left and joined again in the
// period keeps the first join. A plan joined before the period is read at
// the period start, and then the billing intervals are not read.
//
// Mutation: read every plan at the period start, and the plans of sub_new
// are read at midnight. Pin a plan at its current item's join, and pln_back
// is read at the re-add.
func TestUsageOpenPeriods_PinsEachPlanWhereItWasFirstJoined(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	rec := pinRecorder{end: end, pins: map[string]time.Time{}}
	subs := joinedSubs{start: start, end: end, reads: map[string]int{}}
	a := usageOpenPeriods{subs: subs, pricing: rec, thresholds: noWatermarks{}}
	if _, err := a.OpenPeriods(context.Background(), "ten_open", "cus_open"); err != nil {
		t.Fatalf("OpenPeriods: %v", err)
	}
	started, _, added, _, swapped := subs.times()
	want := map[string]time.Time{
		"pln_start":   started,
		"pln_added":   added,
		"pln_swapped": swapped,
		"pln_back":    started,
		"pln_old":     start,
	}
	for plan, at := range want {
		if got, ok := rec.pins[plan]; !ok || !got.Equal(at) {
			t.Errorf("plan %s read at %v, want %v", plan, got, at)
		}
	}
	if subs.reads["sub_new"] != 1 || subs.reads["sub_old"] != 0 {
		t.Errorf("billing-interval reads = %v, want sub_new once and sub_old never", subs.reads)
	}
}
