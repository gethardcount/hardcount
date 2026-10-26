package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// A subscriber pays a plan as it stands when they join it (ADR-130 §4 step 3,
// amended 2026-10-26). Each window of a billing period is priced at its own
// start: a new subscription's first window at the instant it starts, a window
// opened by an immediate plan swap at the swap, an item added mid-period at
// the add. A window that started before a price change keeps the prices it
// started with until the subscription's next period.
//
// Each story runs in wall time and on a test clock. The readers of the open
// period (the preview, customer usage, the live balance) must say what the
// close then bills.

// joinRig runs one story in wall time (clk == "") or on a test clock.
type joinRig struct {
	*ppRig
	clk string
	now time.Time // the clock's time
}

func newJoinRig(t *testing.T, name string, onClock bool) *joinRig {
	t.Helper()
	j := &joinRig{ppRig: newPPRig(t, name)}
	if onClock {
		j.now = time.Now().UTC().AddDate(0, 0, -60).Truncate(24 * time.Hour).Add(9 * time.Hour)
		j.clk = j.clock(name, j.now)
	}
	return j
}

// joinModes runs story in wall time and on a test clock.
func joinModes(t *testing.T, story func(t *testing.T, onClock bool)) {
	t.Run("wall_time", func(t *testing.T) { story(t, false) })
	t.Run("test_clock", func(t *testing.T) { story(t, true) })
}

func (j *joinRig) cust(ext string) string { return j.customer(ext, j.clk) }

// later moves the subscriptions' time on, so the next write is later than
// the last one: a test clock by an hour. In wall time each request is later
// than the one before.
func (j *joinRig) later() {
	if j.clk != "" {
		j.now = j.now.Add(time.Hour)
		j.advance(j.clk, j.now)
	}
}

// soon is an instant after every write so far, inside the open period.
func (j *joinRig) soon() time.Time {
	if j.clk != "" {
		j.later()
		return j.now.Add(-30 * time.Minute)
	}
	return time.Now().UTC().Add(time.Minute)
}

// before is an instant in the open period p0 before every write so far.
func (j *joinRig) before(p0 time.Time) time.Time {
	if j.clk != "" {
		return p0.Add(time.Hour)
	}
	return p0.Add(time.Second)
}

// closeAt bills every period that ended by at.
func (j *joinRig) closeAt(at time.Time) {
	if j.clk != "" {
		j.now = at
		j.advance(j.clk, at)
		return
	}
	j.run(at)
}

// item is the subscription's one item on planID.
func (j *joinRig) item(subID, planID string) string {
	j.t.Helper()
	for _, it := range flowList(j.must(time.Time{}, http.MethodGet, "/v1/subscriptions/"+subID, nil, http.StatusOK), "items") {
		if flowStr(it, "plan_id") == planID {
			return flowStr(it, "id")
		}
	}
	j.t.Fatalf("subscription %s has no item on plan %s", subID, planID)
	return ""
}

// agreeOpen checks that the open period's readers price the customer's usage
// at want cents: the preview of the next invoice, customer usage and the live
// balance's unbilled usage.
func (j *joinRig) agreeOpen(what, custID, subID string, want int64) {
	j.t.Helper()
	preview := j.must(time.Time{}, http.MethodPost, "/v1/invoices/create_preview", map[string]any{"customer_id": custID, "subscription_id": subID}, http.StatusOK)
	var previewCents int64
	for _, l := range flowList(preview, "lines") {
		if flowStr(l, "line_type") == "usage" {
			previewCents += flowInt(l, "amount_cents")
		}
	}
	var usageCents int64
	for _, tot := range flowList(j.must(time.Time{}, http.MethodGet, "/v1/customers/"+custID+"/usage", nil, http.StatusOK), "totals") {
		if flowStr(tot, "currency") == "USD" {
			usageCents += flowInt(tot, "amount_cents")
		}
	}
	var liveCents int64
	for _, b := range flowList(j.must(time.Time{}, http.MethodGet, "/v1/credits/balance/"+custID+"/live", nil, http.StatusOK), "balances") {
		if flowStr(b, "currency") == "USD" {
			liveCents += flowInt(b, "unbilled_usage_cents")
		}
	}
	if previewCents != want || usageCents != want || liveCents != want {
		j.t.Errorf("%s: preview %dc, customer usage %dc, live balance unbilled %dc, want %dc each", what, previewCents, usageCents, liveCents, want)
	}
}

// TestE2E_PlanPrices_SubscribeAfterAChange: a subscription that starts on a
// used plan after a price edit, add and removal earlier the same day pays the
// plan as it stands then, from its first period: the edited rate, the added
// price, and not the removed one. A subscriber whose window started before
// the changes keeps the old prices for that period, also after a quantity
// change, which does not start a new window on the plan.
//
// Mutation: pin every window at the period start, and the late subscriber's
// first period bills the old prices. Pin a window at its segment's start
// without walking back over quantity changes, and the early subscriber's
// usage after its quantity change bills the new prices. Leave the
// subscription's start out of the pin, and the late subscriber is priced at
// midnight.
func TestE2E_PlanPrices_SubscribeAfterAChange(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		j := newJoinRig(t, "subscribe after a change", onClock)
		calls, extra, tokens := j.meter("calls", "sum"), j.meter("extra", "sum"), j.meter("tokens", "sum")
		plan, ids := j.plan("pro", perUnit(calls, "1", nil), perUnit(extra, "7", nil))
		callsPrice, extraPrice := ids[0], ids[1]
		early := j.cust("pp-join-early")
		earlySub, p0, p1 := j.subscribe(time.Time{}, early, plan) // the plan is used from here

		j.later()
		j.editTo(plan, callsPrice, "5", "next_period")
		if w := j.must(time.Time{}, http.MethodDelete, "/v1/plans/"+plan+"/prices/"+extraPrice, nil, http.StatusOK); flowStr(w, "effective") != "next_period" {
			t.Fatalf("removal on a used plan: effective %q, want next_period", flowStr(w, "effective"))
		}
		tokensPrice := flowStr(j.must(time.Time{}, http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(tokens, "2", nil), http.StatusCreated), "id")
		j.later()
		j.must(time.Time{}, http.MethodPatch, "/v1/subscriptions/"+earlySub+"/items/"+j.item(earlySub, plan), map[string]any{"quantity": 2}, http.StatusOK)

		j.later()
		late := j.cust("pp-join-late")
		lateSub, lateP0, _ := j.subscribe(time.Time{}, late, plan)
		if !lateP0.Equal(p0) {
			t.Fatalf("late subscriber's first period starts %v, want %v (the same day)", lateP0, p0)
		}
		at := j.soon()
		for _, ext := range []string{"pp-join-early", "pp-join-late"} {
			for _, ev := range []string{"calls", "extra", "tokens"} {
				j.ingest(time.Time{}, ext, ev, 10, at, nil)
			}
		}

		j.agreeOpen("early subscriber", early, earlySub, 80)
		j.agreeOpen("late subscriber", late, lateSub, 70)
		j.closeAt(p1.Add(time.Hour))
		wantUsage(t, "early subscriber: the prices its window started with", j.cycle(earlySub, p0), map[string]int64{callsPrice: 10, extraPrice: 70})
		wantUsage(t, "late subscriber: the plan as it stood when it started", j.cycle(lateSub, p0), map[string]int64{callsPrice: 50, tokensPrice: 20})
	})
}

// TestE2E_PlanPrices_OverrideBeforeTheStart: an override created before a
// subscription starts, on the same day, applies to its first period. One
// created after the subscription started applies from its next period.
//
// Mutation: resolve overrides at the period start, and the first period of
// the customer whose override came first bills the list price.
func TestE2E_PlanPrices_OverrideBeforeTheStart(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		j := newJoinRig(t, "override before the start", onClock)
		calls := j.meter("calls", "sum")
		plan, ids := j.plan("pro", perUnit(calls, "10", nil))
		override := func(cust string) {
			j.must(time.Time{}, http.MethodPost, "/v1/price-overrides", map[string]any{
				"customer_id": cust, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "4",
			}, http.StatusCreated)
		}
		first := j.cust("pp-ovr-first")
		override(first)
		j.later()
		firstSub, p0, p1 := j.subscribe(time.Time{}, first, plan)
		then := j.cust("pp-ovr-then")
		thenSub, _, _ := j.subscribe(time.Time{}, then, plan)
		j.later()
		override(then)
		at := j.soon()
		j.ingest(time.Time{}, "pp-ovr-first", "calls", 10, at, nil)
		j.ingest(time.Time{}, "pp-ovr-then", "calls", 10, at, nil)

		j.agreeOpen("override before the start", first, firstSub, 40)
		j.agreeOpen("override after the start", then, thenSub, 100)
		j.closeAt(p1.Add(time.Hour))
		wantUsage(t, "override before the start: its first period", j.cycle(firstSub, p0), map[string]int64{ids[0]: 40})
		wantUsage(t, "override after the start: list price this period", j.cycle(thenSub, p0), map[string]int64{ids[0]: 100})
	})
}

// TestE2E_PlanPrices_SwapOntoAPlanAfterAChange: an immediate swap onto a
// used plan with the same billing interval, after the plan's price was edited
// and a price added earlier in the period, prices the window from the swap at
// the plan as it stands then. The window before the swap keeps its own plan's
// prices. A subscriber already on the plan keeps the old prices for this
// period.
//
// Mutation: pin the swapped-in window at the period start, and it bills the
// old rate and not the added price.
func TestE2E_PlanPrices_SwapOntoAPlanAfterAChange(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		j := newJoinRig(t, "swap after a change", onClock)
		calls, tokens := j.meter("calls", "sum"), j.meter("tokens", "sum")
		planA, aIDs := j.plan("plan_a", perUnit(calls, "1", nil))
		planB, bIDs := j.plan("plan_b", perUnit(calls, "3", nil))
		callsA, callsB := aIDs[0], bIDs[0]
		on := j.cust("pp-swap-on")
		onSub, p0, p1 := j.subscribe(time.Time{}, on, planB) // plan B is used
		mover := j.cust("pp-swap-mover")
		moverSub, _, _ := j.subscribe(time.Time{}, mover, planA)
		j.ingest(time.Time{}, "pp-swap-mover", "calls", 10, j.before(p0), nil)

		j.later()
		j.editTo(planB, callsB, "5", "next_period")
		tokensB := flowStr(j.must(time.Time{}, http.MethodPost, "/v1/plans/"+planB+"/prices", perUnit(tokens, "2", nil), http.StatusCreated), "id")

		j.later()
		j.must(time.Time{}, http.MethodPatch, "/v1/subscriptions/"+moverSub+"/items/"+j.item(moverSub, planA),
			map[string]any{"new_plan_id": planB, "immediate": true}, http.StatusOK)
		at := j.soon()
		for _, ext := range []string{"pp-swap-on", "pp-swap-mover"} {
			j.ingest(time.Time{}, ext, "calls", 10, at, nil)
			j.ingest(time.Time{}, ext, "tokens", 10, at, nil)
		}

		j.agreeOpen("already on the plan", on, onSub, 30)
		j.agreeOpen("moved onto the plan", mover, moverSub, 80)
		j.closeAt(p1.Add(time.Hour))
		wantUsage(t, "already on the plan: the prices its window started with", j.cycle(onSub, p0), map[string]int64{callsB: 30})
		wantUsage(t, "moved onto the plan: plan A before the swap, plan B as it stood at the swap", j.cycle(moverSub, p0), map[string]int64{callsA: 10, callsB: 50, tokensB: 20})
	})
}

// TestE2E_PlanPrices_ItemAddedAfterAChange: an item added on the first day of
// a period, after its plan's price was edited and a price added that day,
// prices its window at the plan as it stands at the add. Its billing
// interval opens at the period start (the day rule of ADR-012), so the pin
// comes from the item's own creation. A subscriber already on the plan keeps
// the old prices for this period.
//
// Mutation: pin the added item's window at its interval's start, and it
// bills the old rate and not the added price.
func TestE2E_PlanPrices_ItemAddedAfterAChange(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		j := newJoinRig(t, "item added after a change", onClock)
		calls, tokens, extra := j.meter("calls", "sum"), j.meter("tokens", "sum"), j.meter("extra", "sum")
		planA, aIDs := j.plan("plan_a", perUnit(calls, "1", nil))
		planB, bIDs := j.plan("plan_b", perUnit(tokens, "2", nil))
		callsA, tokensB := aIDs[0], bIDs[0]
		on := j.cust("pp-add-on")
		onSub, p0, p1 := j.subscribe(time.Time{}, on, planB) // plan B is used
		adder := j.cust("pp-add-adder")
		adderSub, _, _ := j.subscribe(time.Time{}, adder, planA)

		j.later()
		j.editTo(planB, tokensB, "4", "next_period")
		extraB := flowStr(j.must(time.Time{}, http.MethodPost, "/v1/plans/"+planB+"/prices", perUnit(extra, "7", nil), http.StatusCreated), "id")

		j.later()
		j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+adderSub+"/items", map[string]any{"plan_id": planB, "quantity": 1}, http.StatusCreated)
		at := j.soon()
		for _, ext := range []string{"pp-add-on", "pp-add-adder"} {
			for _, ev := range []string{"calls", "tokens", "extra"} {
				j.ingest(time.Time{}, ext, ev, 10, at, nil)
			}
		}

		j.agreeOpen("already on the plan", on, onSub, 20)
		j.agreeOpen("added the plan", adder, adderSub, 120)
		j.closeAt(p1.Add(time.Hour))
		wantUsage(t, "already on the plan: the prices its window started with", j.cycle(onSub, p0), map[string]int64{tokensB: 20})
		wantUsage(t, "added the plan: plan B as it stood at the add", j.cycle(adderSub, p0), map[string]int64{callsA: 10, tokensB: 40, extraB: 70})
	})
}

// subscribeItems starts a subscription now on one item per plan, billed on
// the anniversary of its start, and returns its id and first period.
func (j *joinRig) subscribeItems(customerID string, planIDs ...string) (string, time.Time, time.Time) {
	j.t.Helper()
	j.seq++
	items := make([]map[string]any, 0, len(planIDs))
	for _, p := range planIDs {
		items = append(items, map[string]any{"plan_id": p, "quantity": 1})
	}
	out := j.must(time.Time{}, http.MethodPost, "/v1/subscriptions", map[string]any{
		"code": fmt.Sprintf("pp-sub-%d", j.seq), "display_name": "PP", "customer_id": customerID, "billing_time": "anniversary", "items": items,
	}, http.StatusCreated)
	return flowStr(out, "id"), ppTime(j.t, flowStr(out, "current_billing_period_start")), ppTime(j.t, flowStr(out, "current_billing_period_end"))
}

// TestE2E_PlanPrices_ItemOffAndBackOnTheFirstDay: an item leaves a plan on
// the first day of a period, by removal or by an immediate swap to another
// plan, and an item on the same plan is added later that day, after the
// plan's price was edited. The added item bills from the period start (the
// day rule of ADR-012), so its window reaches back over the first item's.
// Each event is billed once: the events before the first item left at the
// prices its window started with, the events after the add at the plan as
// it stood at the add.
//
// Mutation: keep the windows of one meter with different pins apart without
// taking the earlier window's time out of the later one, and the events
// before the first item left are billed twice (once at each pin).
func TestE2E_PlanPrices_ItemOffAndBackOnTheFirstDay(t *testing.T) {
	for _, how := range []string{"removed", "swapped_off"} {
		t.Run(how, func(t *testing.T) {
			joinModes(t, func(t *testing.T, onClock bool) {
				j := newJoinRig(t, "item off and back "+how, onClock)
				calls, tokens := j.meter("calls", "sum"), j.meter("tokens", "sum")
				planA, aIDs := j.plan("plan_a", perUnit(calls, "1", nil))
				planB, _ := j.plan("plan_b", perUnit(tokens, "2", nil))
				callsA := aIDs[0]
				cust := j.cust("pp-off-back")
				var subID string
				var p0, p1 time.Time
				if how == "removed" {
					subID, p0, p1 = j.subscribeItems(cust, planA, planB)
				} else {
					subID, p0, p1 = j.subscribeItems(cust, planA)
				}
				j.ingest(time.Time{}, "pp-off-back", "calls", 10, j.before(p0), nil)

				j.later()
				j.later()
				itemA := j.item(subID, planA)
				if how == "removed" {
					j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+itemA, nil, http.StatusNoContent)
				} else {
					j.must(time.Time{}, http.MethodPatch, "/v1/subscriptions/"+subID+"/items/"+itemA,
						map[string]any{"new_plan_id": planB, "immediate": true}, http.StatusOK)
				}
				j.later()
				j.editTo(planA, callsA, "5", "next_period")
				j.later()
				j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planA, "quantity": 1}, http.StatusCreated)
				j.ingest(time.Time{}, "pp-off-back", "calls", 10, j.soon(), nil)

				j.agreeOpen("item off and back on the plan", cust, subID, 60)
				j.closeAt(p1.Add(time.Hour))
				wantUsage(t, "each event once: 10 at 1c before the item left, 10 at 5c after the add", j.cycle(subID, p0), map[string]int64{callsA: 60})
			})
		})
	}
}

// TestE2E_PlanPrices_MeterMovesPlansOnTheFirstDay: an item on plan A is
// removed on the first day of a period, and an item on plan C, which prices
// the same meter, is added later that day. The added item bills from the
// period start (ADR-012), so its window reaches back over the removed
// item's. Each event is billed once: before the removal by plan A, after it
// by plan C.
//
// Mutation: take the earlier window's time out of later windows only within
// one plan, and the events before the removal are billed by both plans.
func TestE2E_PlanPrices_MeterMovesPlansOnTheFirstDay(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		j := newJoinRig(t, "meter moves plans", onClock)
		calls, tokens := j.meter("calls", "sum"), j.meter("tokens", "sum")
		planA, aIDs := j.plan("plan_a", perUnit(calls, "1", nil))
		planB, _ := j.plan("plan_b", perUnit(tokens, "2", nil))
		planC, cIDs := j.plan("plan_c", perUnit(calls, "3", nil))
		callsA, callsC := aIDs[0], cIDs[0]
		cust := j.cust("pp-moves")
		subID, p0, p1 := j.subscribeItems(cust, planA, planB)
		j.ingest(time.Time{}, "pp-moves", "calls", 10, j.before(p0), nil)

		j.later()
		j.later()
		j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+j.item(subID, planA), nil, http.StatusNoContent)
		j.later()
		j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planC, "quantity": 1}, http.StatusCreated)
		j.ingest(time.Time{}, "pp-moves", "calls", 10, j.soon(), nil)

		j.agreeOpen("meter moved plans", cust, subID, 40)
		j.closeAt(p1.Add(time.Hour))
		wantUsage(t, "each event once: plan A before the removal, plan C after", j.cycle(subID, p0), map[string]int64{callsA: 10, callsC: 30})
	})
}

// between is an instant after every write so far and before the next one,
// inside the open period: a test clock moves on an hour, and in wall time it
// is now.
func (j *joinRig) between() time.Time {
	if j.clk != "" {
		return j.soon()
	}
	return time.Now().UTC()
}

// TestE2E_PlanPrices_MeterMovesPlansAndBackOnTheFirstDay: on the first day
// of a period a meter moves from plan A (1c) to plan C (3c) and back: A's
// item is removed, C's added, C's removed and A's added again. The added
// items bill from the period start (ADR-012). A's second window is priced as
// its first, but each event is billed by the plan that priced the meter
// when it came: before the move and after the move back by plan A, in
// between by plan C.
//
// Mutation: bill A's second window under its first window's pin before
// taking earlier windows' time out of later ones, and plan A's first window
// takes the whole period: C's events bill at 1c.
func TestE2E_PlanPrices_MeterMovesPlansAndBackOnTheFirstDay(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		j := newJoinRig(t, "meter moves plans and back", onClock)
		calls, tokens := j.meter("calls", "sum"), j.meter("tokens", "sum")
		planA, aIDs := j.plan("plan_a", perUnit(calls, "1", nil))
		planB, _ := j.plan("plan_b", perUnit(tokens, "2", nil))
		planC, cIDs := j.plan("plan_c", perUnit(calls, "3", nil))
		callsA, callsC := aIDs[0], cIDs[0]
		cust := j.cust("pp-moves-back")
		subID, p0, p1 := j.subscribeItems(cust, planA, planB)
		j.ingest(time.Time{}, "pp-moves-back", "calls", 10, j.before(p0), nil)

		j.later()
		j.later()
		j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+j.item(subID, planA), nil, http.StatusNoContent)
		j.later()
		j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planC, "quantity": 1}, http.StatusCreated)
		j.ingest(time.Time{}, "pp-moves-back", "calls", 10, j.between(), nil)
		j.later()
		j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+j.item(subID, planC), nil, http.StatusNoContent)
		j.later()
		j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planA, "quantity": 1}, http.StatusCreated)
		j.ingest(time.Time{}, "pp-moves-back", "calls", 10, j.soon(), nil)

		j.agreeOpen("meter moved plans and back", cust, subID, 50)
		j.closeAt(p1.Add(time.Hour))
		wantUsage(t, "each event by the plan pricing the meter then: 20 at A's 1c, 10 at C's 3c", j.cycle(subID, p0), map[string]int64{callsA: 20, callsC: 30})
	})
}

// TestE2E_PlanPrices_PriceChangesAndBackOnTheFirstDay: on the first day of a
// period a plan's calls price is edited from 1c to 5c, the subscription's
// item on the plan is removed and another added (at 5c), that one is removed,
// the price is edited back to 1c and a third item is added. The added items
// bill from the period start (ADR-012). The third item is priced as the
// first, but the events in between are billed at the 5c they came at.
//
// Mutation: bill the third item's window under the first item's pin before
// taking earlier windows' time out of later ones, and the first item's
// window takes the whole period: the 5c events bill at 1c.
func TestE2E_PlanPrices_PriceChangesAndBackOnTheFirstDay(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		j := newJoinRig(t, "price changes and back", onClock)
		calls, tokens := j.meter("calls", "sum"), j.meter("tokens", "sum")
		planA, aIDs := j.plan("plan_a", perUnit(calls, "1", nil))
		planB, _ := j.plan("plan_b", perUnit(tokens, "2", nil))
		callsA := aIDs[0]
		cust := j.cust("pp-price-back")
		subID, p0, p1 := j.subscribeItems(cust, planA, planB)
		j.ingest(time.Time{}, "pp-price-back", "calls", 10, j.before(p0), nil)

		j.later()
		j.later()
		j.editTo(planA, callsA, "5", "next_period")
		j.later()
		j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+j.item(subID, planA), nil, http.StatusNoContent)
		j.later()
		j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planA, "quantity": 1}, http.StatusCreated)
		j.ingest(time.Time{}, "pp-price-back", "calls", 10, j.between(), nil)
		j.later()
		j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+j.item(subID, planA), nil, http.StatusNoContent)
		j.later()
		j.editTo(planA, callsA, "1", "next_period")
		j.later()
		j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planA, "quantity": 1}, http.StatusCreated)
		j.ingest(time.Time{}, "pp-price-back", "calls", 10, j.soon(), nil)

		j.agreeOpen("price changed and back", cust, subID, 70)
		j.closeAt(p1.Add(time.Hour))
		wantUsage(t, "each event at the price it came at: 10 at 1c, 10 at 5c, 10 at 1c", j.cycle(subID, p0), map[string]int64{callsA: 70})
	})
}

// graduated is a graduated price body: the first free units at freeCents
// each, every unit above at aboveCents.
func graduated(meterID string, free int64, freeCents, aboveCents string) map[string]any {
	return map[string]any{"meter_id": meterID, "dimension_match": map[string]any{}, "mode": "graduated", "tiers": []map[string]any{
		{"up_to": free, "unit_amount_cents": freeCents}, {"up_to": nil, "unit_amount_cents": aboveCents},
	}}
}

// TestE2E_PlanPrices_ItemOffAndBackPricedAlike: an item leaves a plan on the
// first day of a period, by removal or by an immediate swap to another plan,
// and an item on the same plan is added later that day. The prices of the
// plan's peak (max) and calls (graduated) meters did not change in between,
// so each of those meters bills one window over the period: the peak once,
// and the calls' free tier once. A price edit of another meter of the plan in
// between does not split them.
//
// Mutation: keep windows of one plan and meter with different pins apart,
// and the peak bills twice and the free tier is granted twice. Compare a
// plan's prices as a whole instead of each meter's, and the other meter's
// edit splits them too.
func TestE2E_PlanPrices_ItemOffAndBackPricedAlike(t *testing.T) {
	for _, how := range []string{"removed", "swapped_off"} {
		t.Run(how, func(t *testing.T) {
			joinModes(t, func(t *testing.T, onClock bool) {
				j := newJoinRig(t, "item off and back alike "+how, onClock)
				peak, calls, other, tokens := j.meter("peak", "max"), j.meter("calls", "sum"), j.meter("other", "sum"), j.meter("tokens", "sum")
				planA, aIDs := j.plan("plan_a", perUnit(peak, "1", nil), graduated(calls, 10, "0", "2"), perUnit(other, "1", nil))
				planB, _ := j.plan("plan_b", perUnit(tokens, "2", nil))
				peakA, callsA, otherA := aIDs[0], aIDs[1], aIDs[2]
				cust := j.cust("pp-alike")
				var subID string
				var p0, p1 time.Time
				if how == "removed" {
					subID, p0, p1 = j.subscribeItems(cust, planA, planB)
				} else {
					subID, p0, p1 = j.subscribeItems(cust, planA)
				}
				first := j.before(p0)
				j.ingest(time.Time{}, "pp-alike", "peak", 10, first, nil)
				j.ingest(time.Time{}, "pp-alike", "calls", 10, first, nil)

				j.later()
				j.later()
				itemA := j.item(subID, planA)
				if how == "removed" {
					j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+itemA, nil, http.StatusNoContent)
				} else {
					j.must(time.Time{}, http.MethodPatch, "/v1/subscriptions/"+subID+"/items/"+itemA,
						map[string]any{"new_plan_id": planB, "immediate": true}, http.StatusOK)
				}
				j.later()
				j.editTo(planA, otherA, "5", "next_period")
				j.later()
				j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planA, "quantity": 1}, http.StatusCreated)
				at := j.soon()
				j.ingest(time.Time{}, "pp-alike", "peak", 10, at, nil)
				j.ingest(time.Time{}, "pp-alike", "calls", 10, at, nil)

				j.agreeOpen("item off and back, priced alike", cust, subID, 30)
				j.closeAt(p1.Add(time.Hour))
				inv := j.cycle(subID, p0)
				wantUsage(t, "one window per meter: a peak of 10 at 1c, 20 calls with 10 free at 2c", inv, map[string]int64{peakA: 10, callsA: 20})
				if n := len(inv.usage()); n != 2 {
					t.Errorf("%d usage lines, want 2 (one per meter): %+v", n, inv.usage())
				}
			})
		})
	}
}

// TestE2E_PlanPrices_ItemOffAndBackLaterInThePeriod: on a test clock, an
// item on a plan is removed on day 3 of a period and an item on the plan is
// added on day 6, with no price change. The time in between is on no item,
// so the two parts do not touch, and each bills on its own (as the pricing
// guide says): the peak (max) meter bills each part's peak, and the calls'
// free tier is granted to each part.
//
// Mutation: merge a key's ranges across a gap into one window, and the peak
// bills once and the free tier is granted once.
func TestE2E_PlanPrices_ItemOffAndBackLaterInThePeriod(t *testing.T) {
	j := newJoinRig(t, "item off and back later", true)
	peak, calls, tokens := j.meter("peak", "max"), j.meter("calls", "sum"), j.meter("tokens", "sum")
	planA, aIDs := j.plan("plan_a", perUnit(peak, "1", nil), graduated(calls, 10, "0", "2"))
	planB, _ := j.plan("plan_b", perUnit(tokens, "2", nil))
	peakA, callsA := aIDs[0], aIDs[1]
	cust := j.cust("pp-off-back-later")
	subID, p0, p1 := j.subscribeItems(cust, planA, planB)
	first := j.before(p0)
	j.ingest(time.Time{}, "pp-off-back-later", "peak", 10, first, nil)
	j.ingest(time.Time{}, "pp-off-back-later", "calls", 10, first, nil)

	j.now = p0.Add(3 * 24 * time.Hour)
	j.advance(j.clk, j.now)
	j.must(time.Time{}, http.MethodDelete, "/v1/subscriptions/"+subID+"/items/"+j.item(subID, planA), nil, http.StatusNoContent)
	j.now = p0.Add(6 * 24 * time.Hour)
	j.advance(j.clk, j.now)
	j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/items", map[string]any{"plan_id": planA, "quantity": 1}, http.StatusCreated)
	at := j.soon()
	j.ingest(time.Time{}, "pp-off-back-later", "peak", 10, at, nil)
	j.ingest(time.Time{}, "pp-off-back-later", "calls", 10, at, nil)

	j.agreeOpen("item off and back later in the period", cust, subID, 20)
	j.closeAt(p1.Add(time.Hour))
	got := map[string]int64{}
	peakLines := 0
	for _, l := range j.cycle(subID, p0).usage() {
		got[l.priceID] += l.amount
		if l.priceID == peakA {
			peakLines++
		}
	}
	if got[peakA] != 20 || peakLines != 2 || got[callsA] != 0 {
		t.Errorf("peak %dc on %d lines, calls %dc; want each part on its own: peak 20c on 2 lines (10 each), calls 0c (10 free in each part)", got[peakA], peakLines, got[callsA])
	}
}

// lateJoiner is the shared start of the stories below: a subscriber on the
// plan from before a price edit (early), and one that subscribes after it the
// same day (late), each with usage at the start of their first period.
type lateJoiner struct {
	j                 *joinRig
	plan, price       string
	early, late       string // customers
	earlySub, lateSub string
	p0, p1            time.Time
}

// With inAdvance the plan has a base fee billed in advance.
func newLateJoiner(t *testing.T, name string, onClock, inAdvance bool) *lateJoiner {
	j := newJoinRig(t, name, onClock)
	calls := j.meter("calls", "sum")
	body := map[string]any{"code": "pro", "name": "pro", "currency": "USD", "billing_interval": "monthly", "prices": []map[string]any{perUnit(calls, "1", nil)}}
	if inAdvance {
		body["base_amount_cents"], body["base_bill_timing"] = 1000, "in_advance"
	}
	out := j.must(time.Time{}, http.MethodPost, "/v1/plans", body, http.StatusCreated)
	plan, ids := flowStr(out, "id"), []string{flowStr(flowList(out, "prices")[0], "id")}
	l := &lateJoiner{j: j, plan: plan, price: ids[0]}
	l.early = j.cust("pp-late-early")
	l.earlySub, l.p0, l.p1 = j.subscribe(time.Time{}, l.early, plan) // the plan is used from here
	j.later()
	j.editTo(plan, ids[0], "5", "next_period")
	j.later()
	l.late = j.cust("pp-late-late")
	var lateP0 time.Time
	l.lateSub, lateP0, _ = j.subscribe(time.Time{}, l.late, plan)
	if !lateP0.Equal(l.p0) {
		t.Fatalf("late subscriber's first period starts %v, want %v (the same day)", lateP0, l.p0)
	}
	first := j.before(l.p0)
	j.ingest(time.Time{}, "pp-late-early", "calls", 10, first, nil)
	j.ingest(time.Time{}, "pp-late-late", "calls", 10, first, nil)
	return l
}

// usageOn sums the usage lines of the subscription's live invoices with
// the billing reason.
func (j *joinRig) usageOn(subID, reason string) int64 {
	j.t.Helper()
	var n int64
	for _, i := range j.invoices(subID) {
		if i.reason == reason {
			n += i.usageCents()
		}
	}
	return n
}

// TestE2E_PlanPrices_LateJoinerLeavesMidPeriod: a subscriber that joined
// after a price edit is billed at the edited price when its period ends
// early: by an immediate cancel, or by an immediate swap onto a plan with
// another billing interval, which closes the period at the swap. A
// subscriber on the plan from before the edit is billed the old price.
//
// Mutation: pin the windows of the cancel and the swap at the cancel
// instant, or at the period start, and one of the two bills the other's
// price.
func TestE2E_PlanPrices_LateJoinerLeavesMidPeriod(t *testing.T) {
	for _, how := range []string{"cancel", "swap_to_yearly"} {
		t.Run(how, func(t *testing.T) {
			joinModes(t, func(t *testing.T, onClock bool) {
				// An immediate swap onto another interval bills the old
				// window's usage only on an in_advance plan (on the swap's
				// invoice, beside the new period's base).
				swap := how == "swap_to_yearly"
				l := newLateJoiner(t, "late joiner leaves "+how, onClock, swap)
				j := l.j
				reason := "subscription_cancel"
				var yearly string
				if swap {
					reason = "subscription_create"
					yearly = flowStr(j.must(time.Time{}, http.MethodPost, "/v1/plans", map[string]any{
						"code": "yearly", "name": "yearly", "currency": "USD", "billing_interval": "yearly",
						"base_amount_cents": 12000, "base_bill_timing": "in_advance", "prices": []map[string]any{},
					}, http.StatusCreated), "id")
				}
				j.later()
				for _, subID := range []string{l.earlySub, l.lateSub} {
					if how == "cancel" {
						j.must(time.Time{}, http.MethodPost, "/v1/subscriptions/"+subID+"/cancel", nil, http.StatusOK)
					} else {
						j.must(time.Time{}, http.MethodPatch, "/v1/subscriptions/"+subID+"/items/"+j.item(subID, l.plan),
							map[string]any{"new_plan_id": yearly, "immediate": true}, http.StatusOK)
					}
				}
				if got := j.usageOn(l.earlySub, reason); got != 10 {
					t.Errorf("on the plan before the edit: %s usage %dc, want 10c (the old price) [%+v]", reason, got, j.invoices(l.earlySub))
				}
				if got := j.usageOn(l.lateSub, reason); got != 50 {
					t.Errorf("joined after the edit: %s usage %dc, want 50c (the edited price) [%+v]", reason, got, j.invoices(l.lateSub))
				}
			})
		})
	}
}

// TestE2E_PlanPrices_LateJoinerThreshold: a subscriber that joined after a
// price edit crosses its spend threshold. The threshold invoice and the
// period close both bill at the edited price; a subscriber on the plan from
// before the edit is billed the old price on both.
//
// Mutation: pin the threshold fire's windows at the fire, or at the period
// start, and its invoice bills the other subscriber's price.
func TestE2E_PlanPrices_LateJoinerThreshold(t *testing.T) {
	joinModes(t, func(t *testing.T, onClock bool) {
		l := newLateJoiner(t, "late joiner threshold", onClock, false)
		j := l.j
		for _, subID := range []string{l.earlySub, l.lateSub} {
			j.must(time.Time{}, http.MethodPut, "/v1/subscriptions/"+subID+"/billing-thresholds", map[string]any{"amount_gte": 5}, http.StatusOK)
		}
		if onClock {
			j.later() // the clock's catch-up scans its thresholds
		} else {
			now := time.Now().UTC()
			j.ppRig.clk.Set(now)
			if _, errs := j.srv.BillingEngine.ScanThresholds(postgres.WithLivemode(clock.WithEffectiveNow(context.Background(), now), false), 50); len(errs) != 0 {
				t.Fatalf("threshold scan: %v", errs)
			}
		}
		at := j.soon()
		j.ingest(time.Time{}, "pp-late-early", "calls", 3, at, nil)
		j.ingest(time.Time{}, "pp-late-late", "calls", 3, at, nil)
		j.closeAt(l.p1.Add(time.Hour))

		for _, c := range []struct {
			what        string
			subID       string
			fire, cycle int64
		}{{"on the plan before the edit", l.earlySub, 10, 3}, {"joined after the edit", l.lateSub, 50, 15}} {
			if got := j.usageOn(c.subID, "threshold"); got != c.fire {
				t.Errorf("%s: threshold invoice usage %dc, want %dc [%+v]", c.what, got, c.fire, j.invoices(c.subID))
			}
			if got := j.cycle(c.subID, l.p0).usageCents(); got != c.cycle {
				t.Errorf("%s: cycle usage %dc, want %dc (the usage after the fire)", c.what, got, c.cycle)
			}
		}
	})
}
