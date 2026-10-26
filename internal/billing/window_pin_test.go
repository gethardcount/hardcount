package billing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Each stretch of a period is pinned where its item joined the stretch's
// plan, or at the period start when it joined before (ADR-130 §4 step 3,
// amended 2026-10-26). A quantity change does not move the pin; a swap does.
// The item threshold reads an item's current plan at its latest stretch's
// pin, and from the item's own fields when that stretch is not in the read.
//
// Mutation: pin every stretch at the period start, or restart the run on a
// quantity change, and the pins below move.
func TestPlanStretches_PinEachStretchWhereItsItemJoined(t *testing.T) {
	ps := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pe := ps.AddDate(0, 1, 0)
	started := ps.Add(15 * time.Hour)
	qty := ps.AddDate(0, 0, 3)
	swap := ps.AddDate(0, 0, 9)
	sub := domain.Subscription{
		CreatedAt: started, StartedAt: &started,
		Items: []domain.SubscriptionItem{{ID: "it", PlanID: "pln_b", CreatedAt: started, PlanChangedAt: &swap}},
	}
	rows := map[string][]domain.ItemInterval{"it": {
		{SubscriptionItemID: "it", PlanID: "pln_a", Quantity: 1, StartsAt: ps, EndsAt: &qty, ItemCreatedAt: started},
		{SubscriptionItemID: "it", PlanID: "pln_a", Quantity: 2, StartsAt: qty, EndsAt: &swap, ItemCreatedAt: started},
		{SubscriptionItemID: "it", PlanID: "pln_b", Quantity: 2, StartsAt: swap, ItemCreatedAt: started},
	}}
	stretches := planStretches(sub, intervalSegmentsByItem(rows, ps, pe), ps, pe, ps)
	want := []struct {
		plan  string
		start time.Time
		pin   time.Time
	}{{"pln_a", ps, started}, {"pln_a", qty, started}, {"pln_b", swap, swap}}
	if len(stretches) != len(want) {
		t.Fatalf("stretches = %+v, want %d", stretches, len(want))
	}
	for i, w := range want {
		s := stretches[i]
		if s.planID != w.plan || !s.start.Equal(w.start) || !s.pin.at.Equal(w.pin) {
			t.Errorf("stretch %d = %s from %v pinned %v, want %s from %v pinned %v", i, s.planID, s.start, s.pin.at, w.plan, w.start, w.pin)
		}
	}
	if got := itemPin(sub, stretches, sub.Items[0], ps); !got.at.Equal(swap) {
		t.Errorf("item pin = %v, want the swap %v", got.at, swap)
	}
	// A read that ends before the swap has no stretch on plan B: the pin
	// comes from the item's last swap.
	early := planStretches(sub, intervalSegmentsByItem(rows, ps, qty), ps, qty, ps)
	if got := itemPin(sub, early, sub.Items[0], ps); !got.at.Equal(swap) {
		t.Errorf("item pin without a plan B stretch = %v, want the swap %v", got.at, swap)
	}

	// The next period: every stretch joined before it, so all are pinned at
	// its start.
	next := pe.AddDate(0, 1, 0)
	for _, s := range planStretches(sub, intervalSegmentsByItem(rows, pe, next), pe, next, pe) {
		if !s.pin.at.Equal(pe) {
			t.Errorf("next period: %s pinned %v, want the period start %v", s.planID, s.pin.at, pe)
		}
	}
}

// pinPrices is a PriceSource whose prices and overrides depend on the pin:
// prices[at] is the plan's prices at a pin at or after at (the latest such),
// overrides[priceID] an override shape from its instant on.
type pinPrices struct {
	prices    map[time.Time][]domain.PlanPrice
	overrides map[string]struct {
		from  time.Time
		shape domain.PriceShape
	}
}

func (p pinPrices) Meter(context.Context, string, string) (domain.Meter, error) {
	return domain.Meter{}, nil
}

func (p pinPrices) PlanPrices(_ context.Context, _, _ string, pin ratePin) ([]domain.PlanPrice, error) {
	var best time.Time
	var out []domain.PlanPrice
	for at, prices := range p.prices {
		if !at.After(pin.at) && !at.Before(best) {
			best, out = at, prices
		}
	}
	return out, nil
}

func (p pinPrices) Price(_ context.Context, _, _ string, price domain.PlanPrice, pin ratePin) (domain.PlanPrice, error) {
	if o, ok := p.overrides[price.ID]; ok && !pin.at.Before(o.from) {
		price.PriceShape = o.shape
	}
	return price, nil
}

// Windows of one plan and meter priced alike are billed under one pin: a
// later pin at which the meter's prices, overrides applied, are the same as
// at an earlier one maps to the earlier one. Each meter is compared on its own, a price
// with an equal shape written as other digits is alike, and a changed rate,
// an override or a new price on the meter is not.
//
// Mutation: compare the plan's prices as a whole, and meter "a" is split by
// meter "b"'s edit; leave the override out, and the override pin maps back.
func TestPricedAlike_EarliestPinPricingTheMeterTheSame(t *testing.T) {
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	t1, t2, t3, t4 := d.Add(time.Hour), d.Add(2*time.Hour), d.Add(3*time.Hour), d.Add(4*time.Hour)
	unit := func(id, meter, cents string) domain.PlanPrice {
		return domain.PlanPrice{ID: id, MeterID: meter, DimensionMatch: map[string]string{}, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString(cents)}}
	}
	src := pinPrices{prices: map[time.Time][]domain.PlanPrice{
		d:  {unit("pa", "a", "1"), unit("pb", "b", "1")},
		t1: {unit("pa", "a", "1.0"), unit("pb", "b", "5")},                      // b edited; a rewritten with equal digits
		t3: {unit("pa", "a", "1"), unit("pb", "b", "5"), unit("pa2", "a", "9")}, // a gains a price
	}, overrides: map[string]struct {
		from  time.Time
		shape domain.PriceShape
	}{"pb": {from: t4, shape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}}}}
	pin := func(at time.Time) ratePin { return ratePin{at: at} }
	planMeters := map[planPin][]string{}
	for _, at := range []time.Time{d, t1, t2, t3, t4} {
		planMeters[planPin{planID: "p", pin: pin(at)}] = []string{"a", "b"}
	}
	got, err := pricedAlike(context.Background(), src, "t", "c", planMeters)
	if err != nil {
		t.Fatal(err)
	}
	want := alikePins{
		{planID: "p", meterID: "a", pin: pin(t1)}: pin(d),  // b's edit does not split a
		{planID: "p", meterID: "a", pin: pin(t2)}: pin(d),  // no change
		{planID: "p", meterID: "b", pin: pin(t2)}: pin(t1), // the edit's own pin
		{planID: "p", meterID: "a", pin: pin(t4)}: pin(t3), // after a's new price
		{planID: "p", meterID: "b", pin: pin(t3)}: pin(t1),
		{planID: "p", meterID: "b", pin: pin(t4)}: pin(d), // the override is b's old rate
	}
	if len(got) != len(want) {
		t.Errorf("pricedAlike = %v, want %v", got, want)
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || !g.at.Equal(w.at) {
			t.Errorf("%s/%s at %s: billed under %v (mapped %v), want %s", k.planID, k.meterID, k.pin.at.Format(time.Kitchen), g.at, ok, w.at.Format(time.Kitchen))
		}
	}
}

// Windows mapped alike are one window in both maps: the item that left and
// the item added on the same plan later that day bill one window from the
// period start, so a max meter's peak is read once.
//
// Mutation: ignore alike, and the added item's window is its own key.
func TestMeterUsageIntervals_AlikePinsAreOneWindow(t *testing.T) {
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	pe := d.AddDate(0, 1, 0)
	created := d.AddDate(0, 0, -20)
	left, added, wm := d.Add(2*time.Hour), d.Add(4*time.Hour), d.Add(time.Hour)
	sub := domain.Subscription{CreatedAt: created, StartedAt: &created, Items: []domain.SubscriptionItem{{ID: "y", PlanID: "p", CreatedAt: added}}}
	segs := map[string][]baseSegment{
		"x": {{start: d, end: left, planID: "p", runStart: created, itemCreatedAt: created}},
		"y": {{start: d, end: pe, planID: "p", runStart: d, itemCreatedAt: added}},
	}
	pinX, pinY := windowPin(sub, d, created), windowPin(sub, d, added)
	plans := map[planPin][]string{{planID: "p", pin: pinX}: {"m"}, {planID: "p", pin: pinY}: {"m"}}
	alike := alikePins{{planID: "p", meterID: "m", pin: pinY}: pinX}
	clamped, full := meterUsageIntervals(planStretches(sub, segs, d, pe, d), plans, alike, &wm, usageFloors{})
	for name, m := range map[string]map[planMeter][]usageInterval{"clamped": clamped, "full": full} {
		if len(m) != 1 {
			t.Errorf("%s: %d keys, want 1: %v", name, len(m), m)
		}
		ivs := mergeUsageIntervals(m[planMeter{planID: "p", meterID: "m", pin: pinX}])
		start := d
		if name == "clamped" {
			start = wm
		}
		if len(ivs) != 1 || !ivs[0].start.Equal(start) || !ivs[0].end.Equal(pe) {
			t.Errorf("%s = %v, want one window [%v, %v)", name, ivs, start, pe)
		}
	}
}

// A window priced alike to an earlier pin does not take that pin's place in
// the one-window-per-instant order. On a period's first day an item x on plan
// p leaves the meter, an item y priced differently joins it (on plan q, or on
// p after an edit), y leaves, and an item z on p priced as x was joins. Each
// added item's window reaches back to the period start (ADR-012). y keeps the
// time between x leaving and z joining; z's time is billed under x's pin.
//
// Mutation: re-key z to x's pin before the overlap step, and x's pin takes
// the whole period: y's window is gone.
func TestMeterUsageIntervals_AlikeKeepsItsOwnPlaceInTime(t *testing.T) {
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	pe := d.AddDate(0, 1, 0)
	created := d.AddDate(0, 0, -20)
	xLeft, yAdded, yLeft, zAdded, wm := d.Add(2*time.Hour), d.Add(3*time.Hour), d.Add(5*time.Hour), d.Add(6*time.Hour), d.Add(time.Hour)
	for _, yPlan := range []string{"p", "q"} {
		t.Run("y on "+yPlan, func(t *testing.T) {
			sub := domain.Subscription{CreatedAt: created, StartedAt: &created, Items: []domain.SubscriptionItem{{ID: "z", PlanID: "p", CreatedAt: zAdded}}}
			segs := map[string][]baseSegment{
				"x": {{start: d, end: xLeft, planID: "p", runStart: created, itemCreatedAt: created}},
				"y": {{start: d, end: yLeft, planID: yPlan, runStart: d, itemCreatedAt: yAdded}},
				"z": {{start: d, end: pe, planID: "p", runStart: d, itemCreatedAt: zAdded}},
			}
			pinX, pinY, pinZ := windowPin(sub, d, created), windowPin(sub, d, yAdded), windowPin(sub, d, zAdded)
			plans := map[planPin][]string{{planID: "p", pin: pinX}: {"m"}, {planID: yPlan, pin: pinY}: {"m"}, {planID: "p", pin: pinZ}: {"m"}}
			alike := alikePins{{planID: "p", meterID: "m", pin: pinZ}: pinX}
			x := planMeter{planID: "p", meterID: "m", pin: pinX}
			y := planMeter{planID: yPlan, meterID: "m", pin: pinY}
			clamped, full := meterUsageIntervals(planStretches(sub, segs, d, pe, d), plans, alike, &wm, usageFloors{})
			for name, m := range map[string]map[planMeter][]usageInterval{"clamped": clamped, "full": full} {
				start := d
				if name == "clamped" {
					start = wm
				}
				want := windowsText(map[planMeter][]usageInterval{x: {{start, xLeft}, {yLeft, pe}}, y: {{xLeft, yLeft}}})
				if got := windowsText(m); got != want {
					t.Errorf("%s = %s, want %s", name, got, want)
				}
			}
		})
	}
}

// windowsText is windows merged, one key per line in key order, each instant
// as an RFC 3339 time.
func windowsText(windows map[planMeter][]usageInterval) string {
	var lines []string
	for k, ivs := range windows {
		line := fmt.Sprintf("%s/%s@%s:", k.planID, k.meterID, k.pin.at.Format(time.RFC3339))
		for _, iv := range mergeUsageIntervals(ivs) {
			line += fmt.Sprintf(" [%s, %s)", iv.start.Format(time.RFC3339), iv.end.Format(time.RFC3339))
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return strings.Join(lines, "; ")
}
