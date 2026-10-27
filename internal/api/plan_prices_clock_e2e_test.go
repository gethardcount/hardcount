package api

import (
	"net/http"
	"testing"
	"time"
)

// Test clocks follow the plan price edit rule in simulated time (ADR-130
// §7): a test-clock subscriber pays a new price from its next SIMULATED
// period, as a live subscriber does from its next real one. A price version
// on a used test-mode plan is stamped for every test clock of the account
// with the clock's time plus 1 microsecond, and a test-clock subscription
// compares its simulated period start with that stamp.

func (r *ppRig) clock(name string, frozen time.Time) string {
	r.t.Helper()
	return flowStr(r.must(time.Time{}, http.MethodPost, "/v1/test-clocks", map[string]any{
		"name": name, "frozen_time": frozen.Format(time.RFC3339),
	}, http.StatusCreated), "id")
}

// advance moves the clock to target. The test server runs the catch-up
// inside the request.
func (r *ppRig) advance(clockID string, target time.Time) {
	r.t.Helper()
	r.must(time.Time{}, http.MethodPost, "/v1/test-clocks/"+clockID+"/advance", map[string]any{
		"frozen_time": target.Format(time.RFC3339),
	}, http.StatusOK)
	if st := flowStr(r.must(time.Time{}, http.MethodGet, "/v1/test-clocks/"+clockID, nil, http.StatusOK), "status"); st != "ready" {
		r.t.Fatalf("clock status after the advance = %q, want ready", st)
	}
}

// clockSub creates a customer on clockID and a subscription for it on planID
// at the clock's time, and returns the subscription id and its first period.
func (r *ppRig) clockSub(ext, clockID, planID string) (string, time.Time, time.Time) {
	r.t.Helper()
	cust := r.customer(ext, clockID)
	return r.subscribe(time.Time{}, cust, planID)
}

// editTo sets the per_unit price's rate and wants effective.
func (r *ppRig) editTo(planID, priceID, cents, effective string) {
	r.t.Helper()
	out := r.must(time.Time{}, http.MethodPatch, "/v1/plans/"+planID+"/prices/"+priceID, map[string]any{"mode": "per_unit", "unit_amount_cents": cents}, http.StatusOK)
	if got := flowStr(out, "effective"); got != effective {
		r.t.Fatalf("edit to %s: effective %q, want %s", cents, got, effective)
	}
}

// clockFlow is the shared story: a clock frozen at frozen, a subscriber on it
// using a plan, an edit from 1c to 2c, and usage in the first two simulated
// periods. It wants the first period at the old rate and the second at the
// new one.
func clockFlow(t *testing.T, name string, frozen time.Time) {
	r := newPPRig(t, name)
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	clk := r.clock(name, frozen)
	sub, p0, p1 := r.clockSub("pp-clk", clk, plan)
	p2 := p1.AddDate(0, 1, 0)
	r.ingest(time.Time{}, "pp-clk", "calls", 10, midpoint(p0, frozen), nil)

	r.editTo(plan, ids[0], "2", "next_period")

	r.advance(clk, p1.Add(time.Hour))
	wantUsage(t, "the simulated period open at the edit: the old rate", r.cycle(sub, p0), map[string]int64{ids[0]: 10})
	r.ingest(time.Time{}, "pp-clk", "calls", 10, p1.Add(30*time.Minute), nil)
	r.advance(clk, p2.Add(time.Hour))
	wantUsage(t, "the next simulated period: the new rate", r.cycle(sub, p1), map[string]int64{ids[0]: 20})
}

// TestE2E_PlanPrices_ClockAheadOfWallTime: a clock 40 days ahead of wall
// time. Before ADR-130 a version was stamped with wall time only, so the
// simulated period already open (which starts after the wall stamp) was
// repriced.
//
// Mutation: compare simulated pins with wall valid_from, and the first
// simulated period bills the new rate.
func TestE2E_PlanPrices_ClockAheadOfWallTime(t *testing.T) {
	clockFlow(t, "clock ahead", time.Now().UTC().AddDate(0, 0, 40).Truncate(time.Second))
}

// TestE2E_PlanPrices_ClockInThePast: a clock 60 days in the past. Before
// ADR-130 the edit waited until simulated time passed wall time, so the next
// simulated period still billed the old rate.
func TestE2E_PlanPrices_ClockInThePast(t *testing.T) {
	clockFlow(t, "clock behind", time.Now().UTC().AddDate(0, 0, -60).Truncate(time.Second))
}

// TestE2E_PlanPrices_ClockAtAPeriodBoundary: the clock is advanced to exactly
// a period start, then the price is edited. The period that just opened keeps
// the old price, as a live period that opened before an edit does: the stamp
// is the clock's time plus 1 microsecond.
//
// Mutation: drop the microsecond, and the period that opened at the clock's
// time bills the new rate.
func TestE2E_PlanPrices_ClockAtAPeriodBoundary(t *testing.T) {
	r := newPPRig(t, "clock boundary")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	frozen := time.Now().UTC().AddDate(0, 0, -100).Truncate(time.Second)
	clk := r.clock("boundary", frozen)
	sub, _, p1 := r.clockSub("pp-bnd", clk, plan)
	p2, p3 := p1.AddDate(0, 1, 0), p1.AddDate(0, 2, 0)
	r.advance(clk, p1)
	r.editTo(plan, ids[0], "2", "next_period")
	r.ingest(time.Time{}, "pp-bnd", "calls", 10, p1, nil)
	r.advance(clk, p2.Add(time.Hour))
	wantUsage(t, "the period that opened at the clock's time", r.cycle(sub, p1), map[string]int64{ids[0]: 10})
	r.ingest(time.Time{}, "pp-bnd", "calls", 10, p2.Add(30*time.Minute), nil)
	r.advance(clk, p3.Add(time.Hour))
	wantUsage(t, "the period after", r.cycle(sub, p2), map[string]int64{ids[0]: 20})
}

// TestE2E_PlanPrices_ClockCreatedAfterAnEdit: a clock made after an edit has
// no stamp for that edit's version, which counts as always in force on it:
// its first period has the new price.
func TestE2E_PlanPrices_ClockCreatedAfterAnEdit(t *testing.T) {
	r := newPPRig(t, "clock after edit")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	r.subscribe(time.Time{}, r.customer("pp-wall", ""), plan) // the plan is used
	r.editTo(plan, ids[0], "3", "next_period")
	frozen := time.Now().UTC().AddDate(0, 0, -30).Truncate(time.Second)
	clk := r.clock("after edit", frozen)
	sub, p0, p1 := r.clockSub("pp-late", clk, plan)
	r.ingest(time.Time{}, "pp-late", "calls", 10, midpoint(p0, frozen), nil)
	r.advance(clk, p1.Add(time.Hour))
	wantUsage(t, "first period", r.cycle(sub, p0), map[string]int64{ids[0]: 30})
}

// TestE2E_PlanPrices_ClockFirstPeriodOnANewPlan: a price written on a plan
// no subscription has used stamps no clock, so a clock customer's first
// period, which starts at midnight before the clock's time, has a price.
//
// Mutation: stamp the never-used write too, and the first period bills
// nothing.
func TestE2E_PlanPrices_ClockFirstPeriodOnANewPlan(t *testing.T) {
	r := newPPRig(t, "clock new plan")
	frozen := time.Now().UTC().AddDate(0, 0, 20).Truncate(time.Hour).Add(13 * time.Hour)
	clk := r.clock("new plan", frozen)
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	r.editTo(plan, ids[0], "7", "now")
	sub, p0, p1 := r.clockSub("pp-new", clk, plan)
	r.ingest(time.Time{}, "pp-new", "calls", 10, midpoint(p0, frozen), nil)
	r.advance(clk, p1.Add(time.Hour))
	wantUsage(t, "first period", r.cycle(sub, p0), map[string]int64{ids[0]: 70})
}

// TestE2E_PlanPrices_TestClockOverride: an override for a clock customer is
// stamped with the clock's time, so it applies from the customer's next
// simulated period, and its delete leaves the period in flight at the
// negotiated rate (ADR-130 §6). Before ADR-130 overrides were stamped with
// wall time.
//
// Mutation: stamp overrides with wall time, and a clock behind wall time
// never sees the override.
func TestE2E_PlanPrices_TestClockOverride(t *testing.T) {
	r := newPPRig(t, "clock override")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "10", nil))
	frozen := time.Now().UTC().AddDate(0, 0, -90).Truncate(time.Second)
	clk := r.clock("override", frozen)
	cust := r.customer("pp-co", clk)
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	p2, p3 := p1.AddDate(0, 1, 0), p1.AddDate(0, 2, 0)
	o := r.must(time.Time{}, http.MethodPost, "/v1/price-overrides", map[string]any{
		"customer_id": cust, "price_id": ids[0], "mode": "per_unit", "unit_amount_cents": "4",
	}, http.StatusCreated)
	if got := ppTime(t, flowStr(o, "created_at")); !got.Equal(frozen.Add(time.Microsecond)) {
		t.Errorf("override created_at = %v, want the clock's time plus 1µs (%v)", got, frozen.Add(time.Microsecond))
	}
	r.ingest(time.Time{}, "pp-co", "calls", 10, midpoint(p0, frozen), nil)
	r.advance(clk, p1.Add(time.Hour))
	wantUsage(t, "P0: list", r.cycle(sub, p0), map[string]int64{ids[0]: 100})

	r.ingest(time.Time{}, "pp-co", "calls", 10, p1.Add(30*time.Minute), nil)
	// Deleted while P1 is open, in simulated time: P1 keeps the override.
	r.must(time.Time{}, http.MethodDelete, "/v1/price-overrides/"+flowStr(o, "id"), nil, http.StatusOK)
	r.advance(clk, p2.Add(time.Hour))
	wantUsage(t, "P1: the override, deleted after P1 started", r.cycle(sub, p1), map[string]int64{ids[0]: 40})
	r.ingest(time.Time{}, "pp-co", "calls", 10, p2.Add(30*time.Minute), nil)
	r.advance(clk, p3.Add(time.Hour))
	wantUsage(t, "P2: list again", r.cycle(sub, p2), map[string]int64{ids[0]: 100})
}

// TestE2E_PlanPrices_NeverUsedAgainAfterTeardown: a test-clock teardown
// deletes the clock's subscriptions, so their plan can be never-used again
// with several versions. A write then replaces every one of them, so it
// applies at once and no older version wins a later pin (ADR-130 §5).
//
// Mutation: update only the version from the beginning of time, and the
// version appended while the plan was used keeps winning.
func TestE2E_PlanPrices_NeverUsedAgainAfterTeardown(t *testing.T) {
	r := newPPRig(t, "teardown")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	frozen := time.Now().UTC().AddDate(0, 0, -45).Truncate(time.Second)
	clk := r.clock("teardown", frozen)
	r.clockSub("pp-td", clk, plan)
	r.editTo(plan, ids[0], "2", "next_period")
	if n := r.count(`SELECT count(*) FROM plan_price_version_clocks WHERE test_clock_id = $1`, clk); n != 1 {
		t.Fatalf("stamps for the clock = %d, want 1", n)
	}
	r.must(time.Time{}, http.MethodDelete, "/v1/test-clocks/"+clk, nil, http.StatusOK)
	if got := r.must(time.Time{}, http.MethodGet, "/v1/plans/"+plan, nil, http.StatusOK); got["used"] != false {
		t.Fatalf("used after the teardown = %v, want false", got["used"])
	}
	r.editTo(plan, ids[0], "5", "now")
	if n := r.count(`SELECT count(*) FROM plan_price_versions WHERE price_id = $1`, ids[0]); n != 1 {
		t.Errorf("versions after the never-used write = %d, want 1", n)
	}
	clk2 := r.clock("after teardown", frozen)
	sub, p0, p1 := r.clockSub("pp-td2", clk2, plan)
	r.ingest(time.Time{}, "pp-td2", "calls", 10, midpoint(p0, frozen), nil)
	r.advance(clk2, p1.Add(time.Hour))
	wantUsage(t, "first period after the teardown", r.cycle(sub, p0), map[string]int64{ids[0]: 50})
}

// TestE2E_PlanPrices_NeverUsedAgainForgetsRemovedPrices: a price removed
// while the plan was used keeps its old version before the removal. Once a
// teardown leaves the plan never-used, a write applies at once, so it puts
// every price of the plan at its latest content from the beginning of time,
// with no clock stamps, a removed price included (ADR-130 §5). An add that
// ties with the removed price then takes its events at every pin, and a
// price edited while the plan was used bills its latest rate: in wall time
// and on a clock that had stamps for the edit and the removal.
//
// Mutation: replace only the written price's versions, and the removed
// price's old version is in force at the new subscribers' first period
// pins, ties with the added price and wins by age; the edited price bills
// its old rate in wall time. Keep the other prices' clock stamps, and on
// the clock the edited price takes no part (its only version starts at the
// stamp).
func TestE2E_PlanPrices_NeverUsedAgainForgetsRemovedPrices(t *testing.T) {
	r := newPPRig(t, "never used removed")
	tokens := r.meter("tokens", "sum")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(tokens, "1", map[string]string{"model": "a"}), perUnit(calls, "1", nil))
	removed, edited := ids[0], ids[1]
	frozen := time.Now().UTC().AddDate(0, 0, -45).Truncate(time.Second)
	clk := r.clock("used", frozen)
	r.clockSub("pp-nr", clk, plan)
	other := r.clock("other", time.Now().UTC().AddDate(0, 0, -20).Truncate(time.Second))
	r.editTo(plan, edited, "3", "next_period")
	r.must(time.Time{}, http.MethodDelete, "/v1/plans/"+plan+"/prices/"+removed, nil, http.StatusOK)
	if n := r.count(`SELECT count(*) FROM plan_price_version_clocks WHERE test_clock_id = $1`, other); n != 2 {
		t.Fatalf("stamps for the other clock = %d, want 2 (the edit's and the removal's)", n)
	}
	r.must(time.Time{}, http.MethodDelete, "/v1/test-clocks/"+clk, nil, http.StatusOK)

	add := r.must(time.Time{}, http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(tokens, "9", map[string]string{"token_type": "input"}), http.StatusCreated)
	if flowStr(add, "effective") != "now" {
		t.Fatalf("add on a never-used plan: effective %q, want now", flowStr(add, "effective"))
	}
	added := flowStr(add, "id")
	if n := r.count(`SELECT count(*) FROM plan_price_versions v JOIN plan_prices p ON p.id = v.price_id
		WHERE p.plan_id = $1 AND v.valid_from <> '-infinity'`, plan); n != 0 {
		t.Errorf("versions not from the beginning of time after the never-used write = %d, want 0", n)
	}
	if n := r.count(`SELECT count(*) FROM plan_price_version_clocks WHERE test_clock_id = $1`, other); n != 0 {
		t.Errorf("stamps for the other clock after the never-used write = %d, want 0", n)
	}
	dims := map[string]string{"model": "a", "token_type": "input"}

	wallSub, w0, w1 := r.subscribe(time.Time{}, r.customer("pp-nr-wall", ""), plan)
	r.ingest(time.Time{}, "pp-nr-wall", "tokens", 10, recent(w0), dims)
	r.ingest(time.Time{}, "pp-nr-wall", "calls", 10, recent(w0), nil)
	r.run(w1.Add(time.Hour))
	wantUsage(t, "wall time: the added price and the edited rate", r.cycle(wallSub, w0), map[string]int64{added: 90, edited: 30})

	clockSub, c0, c1 := r.clockSub("pp-nr-clk", other, plan)
	r.ingest(time.Time{}, "pp-nr-clk", "tokens", 10, c0.Add(time.Hour), dims)
	r.ingest(time.Time{}, "pp-nr-clk", "calls", 10, c0.Add(time.Hour), nil)
	r.advance(other, c1.Add(time.Hour))
	wantUsage(t, "the other clock: the added price and the edited rate", r.cycle(clockSub, c0), map[string]int64{added: 90, edited: 30})
}
