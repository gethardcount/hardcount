package billing

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// A plan's pin of a period is the later of the period start and the first
// instant in the period at which the subscription had an item on the plan
// (ADR-130 §4 step 3, amended 2026-10-26). A quantity change does not move
// it; a swap onto the plan, an add and the subscription's start do. A plan
// left and joined again in the period keeps the first pin. A current item
// pins its plan even when the read has no stretch of it, and an item added
// after the period has no stretch in it.
//
// Mutation: take the latest join instead of the first, or pin from the
// current items alone, and the pins below move. Give an item with no
// interval in the period the whole period, and item v has a stretch.
func TestPinsOf_EachPlanAtItsFirstJoinInThePeriod(t *testing.T) {
	ps := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	pe := ps.AddDate(0, 1, 0)
	at := func(t *testing.T, pins periodPins, plan string, want time.Time) {
		t.Helper()
		if got := pins.of(plan); !got.at.Equal(want) {
			t.Errorf("plan %s pinned %v, want %v", plan, got.at, want)
		}
	}

	t.Run("start, quantity change, swap", func(t *testing.T) {
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
		pins := pinsOf(sub, planStretches(sub, intervalSegmentsByItem(rows, ps, pe), ps, pe), ps)
		at(t, pins, "pln_a", started)
		at(t, pins, "pln_b", swap)
		// A read that ends before the swap has no stretch on plan B: the
		// current item pins it at its last swap.
		early := pinsOf(sub, planStretches(sub, intervalSegmentsByItem(rows, ps, qty), ps, qty), ps)
		at(t, early, "pln_b", swap)
		// The next period: every plan was joined before it.
		next := pe.AddDate(0, 1, 0)
		later := pinsOf(sub, planStretches(sub, intervalSegmentsByItem(rows, pe, next), pe, next), pe)
		at(t, later, "pln_a", pe)
		at(t, later, "pln_b", pe)
	})

	t.Run("left and joined again", func(t *testing.T) {
		created := ps.AddDate(0, -2, 0)
		xLeft, yAdded := ps.Add(2*time.Hour), ps.Add(4*time.Hour)
		wAdded, toD, backToC := ps.AddDate(0, 0, 2), ps.AddDate(0, 0, 5), ps.AddDate(0, 0, 8)
		late := pe.Add(time.Hour)
		sub := domain.Subscription{
			CreatedAt: created, StartedAt: &created,
			Items: []domain.SubscriptionItem{
				{ID: "y", PlanID: "pln_a", CreatedAt: yAdded},
				{ID: "w", PlanID: "pln_c", CreatedAt: wAdded, PlanChangedAt: &backToC},
				{ID: "v", PlanID: "pln_e", CreatedAt: late},
			},
		}
		rows := map[string][]domain.ItemInterval{
			// x was on plan A from before the period and left on its first
			// day; y joined A later that day (its interval opens at the
			// period start, ADR-012).
			"x": {{SubscriptionItemID: "x", PlanID: "pln_a", Quantity: 1, StartsAt: created, EndsAt: &xLeft, ItemCreatedAt: created}},
			"y": {{SubscriptionItemID: "y", PlanID: "pln_a", Quantity: 1, StartsAt: ps, ItemCreatedAt: yAdded}},
			// w was added on plan C, swapped to D and back to C.
			"w": {
				{SubscriptionItemID: "w", PlanID: "pln_c", Quantity: 1, StartsAt: wAdded, EndsAt: &toD, ItemCreatedAt: wAdded},
				{SubscriptionItemID: "w", PlanID: "pln_d", Quantity: 1, StartsAt: toD, EndsAt: &backToC, ItemCreatedAt: wAdded},
				{SubscriptionItemID: "w", PlanID: "pln_c", Quantity: 1, StartsAt: backToC, ItemCreatedAt: wAdded},
			},
			// v was added after the period ended, before its close.
			"v": {{SubscriptionItemID: "v", PlanID: "pln_e", Quantity: 1, StartsAt: late, ItemCreatedAt: late}},
		}
		stretches := planStretches(sub, intervalSegmentsByItem(rows, ps, pe), ps, pe)
		for _, s := range stretches {
			if s.itemID == "v" {
				t.Errorf("an item added after the period has a stretch in it: %+v", s)
			}
		}
		pins := pinsOf(sub, stretches, ps)
		at(t, pins, "pln_a", ps)
		at(t, pins, "pln_c", wAdded)
		at(t, pins, "pln_d", toD)
		at(t, pins, "pln_e", late)
	})
}

// One meter's usage at one instant is in one window. The meter-overlap guard
// keeps two live items of a subscription off one meter, but an item added on
// a period's first day bills from the period start (ADR-012), so its window
// reaches back over items that left the meter earlier that day. On the first
// day an item x on plan p leaves the meter, an item y joins it (on plan q, or
// on p), y leaves, and an item z on p joins. Each instant stays with the item
// that was on the meter then: y keeps the time between x leaving and z
// joining. On one plan, the windows are one window over the period. Both
// maps.
//
// Mutation: drop the overlap step, and q's window starts at the period
// start. Order the windows by their plan's pin instead of each item's join,
// and plan p takes the whole period: q's window is gone.
func TestMeterUsageIntervals_EachInstantByTheItemOnTheMeterThen(t *testing.T) {
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	pe := d.AddDate(0, 1, 0)
	created := d.AddDate(0, 0, -20)
	xLeft, yAdded, yLeft, zAdded, wm := d.Add(2*time.Hour), d.Add(3*time.Hour), d.Add(5*time.Hour), d.Add(6*time.Hour), d.Add(time.Hour)
	for _, yPlan := range []string{"p", "q"} {
		t.Run("y on "+yPlan, func(t *testing.T) {
			sub := domain.Subscription{CreatedAt: created, StartedAt: &created, Items: []domain.SubscriptionItem{{ID: "z", PlanID: "p", CreatedAt: zAdded}}}
			segs := map[string][]baseSegment{
				"x": {{start: d, end: xLeft, planID: "p", itemCreatedAt: created}},
				"y": {{start: d, end: yLeft, planID: yPlan, itemCreatedAt: yAdded}},
				"z": {{start: d, end: pe, planID: "p", itemCreatedAt: zAdded}},
			}
			plans := map[string][]string{"p": {"m"}, yPlan: {"m"}}
			x := planMeter{planID: "p", meterID: "m"}
			y := planMeter{planID: yPlan, meterID: "m"}
			clamped, full := meterUsageIntervals(planStretches(sub, segs, d, pe), plans, &wm, usageFloors{})
			for name, m := range map[string]map[planMeter][]usageInterval{"clamped": clamped, "full": full} {
				start := d
				if name == "clamped" {
					start = wm
				}
				want := windowsText(map[planMeter][]usageInterval{x: {{start, xLeft}, {yLeft, pe}}, y: {{xLeft, yLeft}}})
				if yPlan == "p" {
					want = windowsText(map[planMeter][]usageInterval{x: {{start, pe}}})
				}
				if got := windowsText(m); got != want {
					t.Errorf("%s = %s, want %s", name, got, want)
				}
			}
		})
	}
}

// An estimate over a window that starts inside a period (customer usage or a
// preview over a chosen window) clips each stretch to the window's start.
// The time a meter had before an item left it on the period's first day
// still stays with that item: the item added later that day joined at its
// add, not at the window's start, even though its interval opens at the
// period start (ADR-012) and the plan it is on sorts first.
//
// Mutation: take a stretch's join from its clipped start alone, and both
// stretches join at the window's start; plan 1 then takes 09:00 to 10:00.
func TestMeterUsageIntervals_EstimateWindowKeepsEachInstantWithTheItemOnTheMeter(t *testing.T) {
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	pe := d.AddDate(0, 1, 0)
	created := d.AddDate(0, 0, -20)
	from, xLeft, yAdded := d.Add(9*time.Hour), d.Add(10*time.Hour), d.Add(11*time.Hour)
	sub := domain.Subscription{CreatedAt: created, StartedAt: &created, Items: []domain.SubscriptionItem{{ID: "y", PlanID: "pln_1", CreatedAt: yAdded}}}
	rows := map[string][]domain.ItemInterval{
		"x": {{SubscriptionItemID: "x", PlanID: "pln_2", Quantity: 1, StartsAt: created, EndsAt: &xLeft, ItemCreatedAt: created}},
		"y": {{SubscriptionItemID: "y", PlanID: "pln_1", Quantity: 1, StartsAt: d, ItemCreatedAt: yAdded}},
	}
	plans := map[string][]string{"pln_1": {"m"}, "pln_2": {"m"}}
	clamped, _ := meterUsageIntervals(planStretches(sub, intervalSegmentsByItem(rows, from, pe), from, pe), plans, nil, usageFloors{})
	want := windowsText(map[planMeter][]usageInterval{
		{planID: "pln_2", meterID: "m"}: {{from, xLeft}},
		{planID: "pln_1", meterID: "m"}: {{xLeft, pe}},
	})
	if got := windowsText(clamped); got != want {
		t.Errorf("windows = %s, want %s", got, want)
	}
}

// windowsText is windows merged, one key per line in key order, each instant
// as an RFC 3339 time.
func windowsText(windows map[planMeter][]usageInterval) string {
	var lines []string
	for k, ivs := range windows {
		line := fmt.Sprintf("%s/%s:", k.planID, k.meterID)
		for _, iv := range mergeUsageIntervals(ivs) {
			line += fmt.Sprintf(" [%s, %s)", iv.start.Format(time.RFC3339), iv.end.Format(time.RFC3339))
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return strings.Join(lines, "; ")
}
