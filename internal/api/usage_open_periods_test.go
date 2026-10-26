package api

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// openPeriodsSubs answers one active subscription with two items, one of
// them with a plan change pending at the period end, which ingest does not
// read: an event after the period end is not checked.
type openPeriodsSubs struct{ start, end time.Time }

func (s openPeriodsSubs) ListLiveByCustomer(context.Context, string, string) ([]domain.Subscription, error) {
	started := s.start.Add(15 * time.Hour)
	return []domain.Subscription{{
		ID: "sub_open", Status: domain.SubscriptionActive,
		CurrentBillingPeriodStart: &s.start, CurrentBillingPeriodEnd: &s.end, StartedAt: &started,
		Items: []domain.SubscriptionItem{
			{ID: "it_a", PlanID: "pln_a", PendingPlanID: "pln_b", PendingPlanEffectiveAt: &s.end},
			{ID: "it_c", PlanID: "pln_c"},
		},
	}}, nil
}

// ListItemIntervals answers both items on their plans since the period start.
func (s openPeriodsSubs) ListItemIntervals(context.Context, string, string) ([]domain.ItemInterval, error) {
	return []domain.ItemInterval{
		{SubscriptionItemID: "it_a", PlanID: "pln_a", Quantity: 1, StartsAt: s.start},
		{SubscriptionItemID: "it_c", PlanID: "pln_c", Quantity: 1, StartsAt: s.start},
	}, nil
}

// ListCustomerMeterCoverageEnds answers no other subscription.
func (s openPeriodsSubs) ListCustomerMeterCoverageEnds(context.Context, string, string, string, time.Time, time.Time) ([]domain.MeterCoverageEnd, error) {
	return nil, nil
}

type noWatermarks struct{}

func (noWatermarks) ThresholdWatermarks(context.Context, string, string, time.Time, time.Time) (*time.Time, *time.Time, error) {
	return nil, nil, nil
}

// countingPricing counts the reads of plan prices. pln_a prices mtr_a from
// the period start on and mtr_a2 from its end; pln_b prices mtr_b for model
// x only; pln_c prices mtr_c.
type countingPricing struct {
	end   time.Time
	reads *int
	pins  *[]string
}

func (p countingPricing) GetMeterTx(context.Context, *sql.Tx, string) (domain.Meter, error) {
	panic("not read by OpenPeriods")
}

func (p countingPricing) priceAt(planID string, at time.Time) pricing.PriceMatch {
	switch {
	case planID == "pln_a" && at.Before(p.end):
		return pricing.PriceMatch{MeterID: "mtr_a", Match: map[string]string{}}
	case planID == "pln_a":
		return pricing.PriceMatch{MeterID: "mtr_a2", Match: map[string]string{}}
	case planID == "pln_b":
		return pricing.PriceMatch{MeterID: "mtr_b", Match: map[string]string{"model": "x"}}
	default:
		return pricing.PriceMatch{MeterID: "mtr_c", Match: map[string]string{}}
	}
}

func (p countingPricing) PriceMatchesAt(_ context.Context, _ string, pins []pricing.PlanPin) ([]pricing.PinPrices, error) {
	*p.reads++
	out := make([]pricing.PinPrices, len(pins))
	for i, pin := range pins {
		*p.pins = append(*p.pins, pin.PlanID+"@"+pin.At.Format("02 15:04"))
		out[i] = pricing.PinPrices{Passed: true, Matches: []pricing.PriceMatch{p.priceAt(pin.PlanID, pin.At)}}
	}
	return out, nil
}

// Ingest reads a customer's open periods on every event. The prices each
// plan has at each pin are one read, however many items and pins the
// subscriptions have. Only the period's plans are read, at their pins: not
// the plans from the period end on, or a pending change's plan. And the
// floor is the first billing interval's start.
//
// Mutation: read each (plan, pin) on its own, and the subscription below
// costs two reads.
func TestUsageOpenPeriods_ReadsPlanPricesOnce(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	reads := 0
	var pins []string
	a := usageOpenPeriods{
		subs:       openPeriodsSubs{start: start, end: end},
		pricing:    countingPricing{end: end, reads: &reads, pins: &pins},
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
	atPin := map[string]string{}
	for planID, ps := range p.Plans {
		for m := range ps.ByMeter {
			atPin[planID] += m
		}
	}
	if want := map[string]string{"pln_a": "mtr_a", "pln_c": "mtr_c"}; fmt.Sprint(atPin) != fmt.Sprint(want) {
		t.Errorf("prices at the period start = %v, want %v", atPin, want)
	}
	slices.Sort(pins)
	if want := []string{"pln_a@01 15:00", "pln_c@01 15:00"}; fmt.Sprint(pins) != fmt.Sprint(want) {
		t.Errorf("pins read = %v, want %v", pins, want)
	}
	if p.Floor == nil || !p.Floor.Equal(start) {
		t.Errorf("floor = %v, want the first billing interval's start %v, not the start instant", p.Floor, start)
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
	coverage   []domain.MeterCoverageEnd
}

// ListCustomerMeterCoverageEnds answers s.coverage for sub_new.
func (s joinedSubs) ListCustomerMeterCoverageEnds(_ context.Context, _, _, excludeSubID string, _, _ time.Time) ([]domain.MeterCoverageEnd, error) {
	if excludeSubID != "sub_new" {
		return nil, nil
	}
	return s.coverage, nil
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
		return []domain.ItemInterval{{SubscriptionItemID: "it_old", PlanID: "pln_old", Quantity: 1, StartsAt: s.start.AddDate(0, -2, 0)}}, nil
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

func (p pinRecorder) PriceMatchesAt(_ context.Context, _ string, pins []pricing.PlanPin) ([]pricing.PinPrices, error) {
	out := make([]pricing.PinPrices, len(pins))
	for i, pin := range pins {
		if pin.At.Before(p.end) {
			p.pins[pin.PlanID] = pin.At
		}
		out[i] = pricing.PinPrices{Passed: true, Matches: []pricing.PriceMatch{{MeterID: "mtr_" + pin.PlanID, Match: map[string]string{}}}}
	}
	return out, nil
}

// Ingest reads the prices of each plan of the period at its pin of the
// period, the one the close prices it at (ADR-130 §4 step 3, amended
// 2026-10-26): the first instant in the period the subscription had an item
// on the plan, a subscription's start, an add, a swap. A plan left and
// joined again in the period keeps the first join, and a plan swapped away
// from is read too. A plan joined before the period is read at the period
// start.
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
		"pln_prev":    started,
		"pln_back":    started,
		"pln_old":     start,
	}
	for plan, at := range want {
		if got, ok := rec.pins[plan]; !ok || !got.Equal(at) {
			t.Errorf("plan %s read at %v, want %v", plan, got, at)
		}
	}
	if len(rec.pins) != len(want) {
		t.Errorf("plans read = %v, want %v", rec.pins, want)
	}
}

// Ingest classifies an event by the plan stretches the close bills the
// period by, read from the billing intervals of every active subscription:
// a plan swapped away from and an item removed in the period keep their
// stretches, and an item added on the period's first day bills from the
// period start (ADR-012).
//
// Mutation: read billing intervals only when an item joined its plan in the
// period → sub_old's stretch is missing; build spans from the current items
// → pln_prev and it_gone's pln_back stretch are missing.
func TestUsageOpenPeriods_StretchesAreTheClosesStretches(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	subs := joinedSubs{start: start, end: end, reads: map[string]int{}}
	a := usageOpenPeriods{subs: subs, pricing: pinRecorder{end: end, pins: map[string]time.Time{}}, thresholds: noWatermarks{}}
	got, err := a.OpenPeriods(context.Background(), "ten_open", "cus_open")
	if err != nil {
		t.Fatalf("OpenPeriods: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("open periods = %d, want 2", len(got))
	}
	_, gone, _, _, swapped := subs.times()
	spans := func(p usage.OpenPeriod) []string {
		var out []string
		for _, sp := range p.Stretches {
			var meters []string
			for m := range p.Plans[sp.PlanID].ByMeter {
				meters = append(meters, m)
			}
			slices.Sort(meters)
			out = append(out, fmt.Sprintf("%s [%s, %s) %v", sp.PlanID, sp.From.Format("02 15:04"), sp.To.Format("02 15:04"), meters))
		}
		slices.Sort(out)
		return out
	}
	f := func(t time.Time) string { return t.Format("02 15:04") }
	wantNew := []string{
		fmt.Sprintf("pln_added [%s, %s) [mtr_pln_added]", f(start), f(end)),
		fmt.Sprintf("pln_back [%s, %s) [mtr_pln_back]", f(start), f(end)),
		fmt.Sprintf("pln_back [%s, %s) [mtr_pln_back]", f(start), f(gone)),
		fmt.Sprintf("pln_prev [%s, %s) [mtr_pln_prev]", f(start), f(swapped)),
		fmt.Sprintf("pln_start [%s, %s) [mtr_pln_start]", f(start), f(end)),
		fmt.Sprintf("pln_swapped [%s, %s) [mtr_pln_swapped]", f(swapped), f(end)),
	}
	slices.Sort(wantNew)
	if g := spans(got[0]); fmt.Sprint(g) != fmt.Sprint(wantNew) {
		t.Errorf("sub_new stretches =\n%v\nwant\n%v", g, wantNew)
	}
	wantOld := []string{fmt.Sprintf("pln_old [%s, %s) [mtr_pln_old]", f(start), f(end))}
	if g := spans(got[1]); fmt.Sprint(g) != fmt.Sprint(wantOld) {
		t.Errorf("sub_old stretches = %v, want %v", g, wantOld)
	}
	if subs.reads["sub_new"] != 1 || subs.reads["sub_old"] != 1 {
		t.Errorf("billing-interval reads = %v, want each subscription once", subs.reads)
	}
}

// Ingest classifies an event of a meter by the windows the close bills the
// meter's usage over (billing.PeriodPlan.UsageWindows): it_back, added on
// the period's first day on pln_back, reaches back to the period start, but
// the time before 16:00 stays with it_gone, which was on the meter then. And
// another subscription that stopped covering mtr_pln_start at 17:00 moves
// that meter's window start there.
//
// Mutation: build the windows without the coverage ends → pln_start's window
// starts at the period start.
func TestUsageOpenPeriods_WindowsAreTheClosesWindows(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	covered := start.Add(17 * time.Hour)
	subs := joinedSubs{start: start, end: end, reads: map[string]int{}, coverage: []domain.MeterCoverageEnd{{MeterID: "mtr_pln_start", EndsAt: covered}}}
	a := usageOpenPeriods{subs: subs, pricing: pinRecorder{end: end, pins: map[string]time.Time{}}, thresholds: noWatermarks{}}
	got, err := a.OpenPeriods(context.Background(), "ten_open", "cus_open")
	if err != nil {
		t.Fatalf("OpenPeriods: %v", err)
	}
	_, gone, _, _, _ := subs.times()
	f := func(t time.Time) string { return t.Format("02 15:04") }
	windows := func(meterID string) string {
		var out []string
		for _, w := range got[0].Windows[meterID] {
			out = append(out, fmt.Sprintf("[%s, %s)", f(w.From), f(w.To)))
		}
		return fmt.Sprint(out)
	}
	if g, w := windows("mtr_pln_back"), fmt.Sprint([]string{fmt.Sprintf("[%s, %s)", f(start), f(gone)), fmt.Sprintf("[%s, %s)", f(gone), f(end))}); g != w {
		t.Errorf("mtr_pln_back windows = %s, want %s: one window per instant", g, w)
	}
	if g, w := windows("mtr_pln_start"), fmt.Sprint([]string{fmt.Sprintf("[%s, %s)", f(covered), f(end))}); g != w {
		t.Errorf("mtr_pln_start windows = %s, want %s: from the other subscription's coverage end", g, w)
	}
}
