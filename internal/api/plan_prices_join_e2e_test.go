package api

import (
	"net/http"
	"testing"
	"time"
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
