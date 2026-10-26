package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Usage that no invoice will bill is answered with a billing_status (D07,
// ADR-130 §10 and PR-6): outside_subscription, not_on_plan, no_price and
// after_close. The status is absent when an open period's invoice bills the
// event. Each test that flags an event also closes the period and checks
// that the invoice really leaves it out, and each event the invoice does
// bill is checked to carry no status.
//
// The rig is the close-lag rig (close_lag_e2e_test.go): the real server over
// HTTP, April is P0 = [Apr 1, May 1) and B = May 1.

const unbilledMetric = "velox_usage_unbilled_total"

// unbilledCount reads velox_usage_unbilled_total{reason, origin}.
func unbilledCount(t *testing.T, reason, origin string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != unbilledMetric {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["reason"] == reason && labels["origin"] == origin {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// labelled is one event of meter at ts with the given labels and key.
func (r *lagRig) labelled(meter, key string, ts time.Time, qty int64, dims map[string]any) map[string]any {
	ev := map[string]any{
		"external_customer_id": r.custExt, "event_name": meter, "quantity": qty,
		"timestamp": ts.Format(time.RFC3339), "idempotency_key": key,
	}
	if dims != nil {
		ev["dimensions"] = dims
	}
	return ev
}

// postLabelled posts one labelled event reaching Velox at arrival, wants 201
// and returns the answer.
func (r *lagRig) postLabelled(meter, key string, ts, arrival time.Time, qty int64, dims map[string]any) map[string]any {
	r.t.Helper()
	code, out, _ := r.doH(http.MethodPost, "/v1/usage-events", arrival, r.labelled(meter, key, ts, qty, dims))
	if code != http.StatusCreated {
		r.t.Fatalf("POST %s at %v: %d %v, want 201", key, arrival, code, out)
	}
	return out
}

// planPricing creates a monthly in-arrears plan with the given prices, each
// {meter_id, dimension_match} at 1c per unit.
func (r *lagRig) planPricing(code string, prices ...map[string]any) string {
	r.t.Helper()
	body := []map[string]any{}
	for _, p := range prices {
		price := map[string]any{"mode": "per_unit", "unit_amount_cents": "1"}
		for k, v := range p {
			price[k] = v
		}
		body = append(body, price)
	}
	p := r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
		"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 0, "base_bill_timing": "in_arrears", "prices": body,
	}, http.StatusCreated)
	return p["id"].(string)
}

// TestBillingStatus_OutsideSubscription: a customer with no subscription is
// answered outside_subscription and counted; so is an event stamped before
// the subscription started. An event in the open period carries no status.
//
// Mutation check: answer no status for Outside → the first two are absent.
func TestBillingStatus_OutsideSubscription(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	n0 := unbilledCount(t, "outside_subscription", "api")

	out := r.postLabelled("calls", "no-sub", lagP0.AddDate(0, 0, 2), lagP0.AddDate(0, 0, 2), 5, nil)
	wantStatus(t, "event of a customer with no subscription", out, "outside_subscription")

	r.newSub("sub", r.planA, lagP0.AddDate(0, 0, 3), nil)
	start := *r.sub().CurrentBillingPeriodStart
	out = r.postLabelled("calls", "before-start", start.Add(-time.Hour), lagP0.AddDate(0, 0, 4), 7, nil)
	wantStatus(t, "event before the subscription started", out, "outside_subscription")
	out = r.postLabelled("calls", "open", lagP0.AddDate(0, 0, 5), lagP0.AddDate(0, 0, 5), 11, nil)
	wantStatus(t, "event in the open period", out, "")

	if got := unbilledCount(t, "outside_subscription", "api") - n0; got != 2 {
		t.Errorf("%s{reason=outside_subscription,origin=api} moved by %v, want 2", unbilledMetric, got)
	}

	end := *r.sub().CurrentBillingPeriodEnd
	r.run(end.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 11 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 11c: only the open-period event", got)
	}
}

// TestBillingStatus_OutsideSubscription_SameDaySubscriptionBillsIt pins the
// stated exception: a subscription's first period starts at the start of
// its first day (ADR-012), so a subscription created later that day bills an
// event that was answered outside_subscription earlier that day.
func TestBillingStatus_OutsideSubscription_SameDaySubscriptionBillsIt(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	morning := lagP0.Add(9 * time.Hour)
	out := r.postLabelled("calls", "morning", morning, morning, 5, nil)
	wantStatus(t, "event before any subscription", out, "outside_subscription")

	r.newSub("sub", r.planA, lagP0.Add(12*time.Hour), nil)
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagP0) {
		t.Fatalf("period start = %v, want the start of the day %v", s.CurrentBillingPeriodStart, lagP0)
	}
	r.run(r.sub().CurrentBillingPeriodEnd.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 5 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 5c: the morning event is in the first period", got)
	}
}

// TestBillingStatus_NotOnPlan: an event of a meter the subscription's plan
// does not price is answered not_on_plan, in the open period and in an
// invoiced one; the invoices leave it out.
//
// Mutation check: answer no status for NotOnPlan → absent.
func TestBillingStatus_NotOnPlan(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.newMeter("seats", "sum")
	n0 := unbilledCount(t, "not_on_plan", "api")

	out := r.postLabelled("seats", "seats-open", lagP0.AddDate(0, 0, 3), lagP0.AddDate(0, 0, 3), 9, nil)
	wantStatus(t, "unpriced meter in the open period", out, "not_on_plan")
	out = r.postLabelled("calls", "calls-open", lagP0.AddDate(0, 0, 3), lagP0.AddDate(0, 0, 3), 4, nil)
	wantStatus(t, "priced meter in the open period", out, "")

	r.run(lagB.Add(2 * time.Hour))
	out = r.postLabelled("seats", "seats-late", lagP0.AddDate(0, 0, 20), lagB.Add(3*time.Hour), 6, nil)
	wantStatus(t, "unpriced meter in the invoiced period", out, "not_on_plan")
	out = r.postLabelled("calls", "calls-late", lagP0.AddDate(0, 0, 20), lagB.Add(3*time.Hour), 6, nil)
	wantStatus(t, "priced meter in the invoiced period", out, "after_close")

	if got := unbilledCount(t, "not_on_plan", "api") - n0; got != 2 {
		t.Errorf("%s{reason=not_on_plan,origin=api} moved by %v, want 2", unbilledMetric, got)
	}
	r.run(lagP2.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 4 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 4c: only the priced open-period event", got)
	}
}

// TestBillingStatus_NoPrice: the plan prices the tokens meter for model a
// only. An event labelled model b, with no model, or with the number 1
// where the price names the string "1", matches no price: no_price, and the
// invoice leaves it out. A label the price does not name does not stop a
// match.
//
// Mutation checks: classify by meter only → no status on the unmatched
// events; compare label values with fmt.Sprint → the number event is
// answered with no status but not billed.
func TestBillingStatus_NoPrice(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_labels",
		map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "a"}},
		map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "1"}},
	)
	r.newSub("sub", plan, lagP0, nil)
	n0 := unbilledCount(t, "no_price", "api")
	at := lagP0.AddDate(0, 0, 4)

	cases := []struct {
		key  string
		qty  int64
		dims map[string]any
		want string
	}{
		{"model-a", 3, map[string]any{"model": "a"}, ""},
		{"model-a-extra", 5, map[string]any{"model": "a", "region": "eu"}, ""},
		{"model-string-1", 7, map[string]any{"model": "1"}, ""},
		{"model-b", 100, map[string]any{"model": "b"}, "no_price"},
		{"no-model", 200, nil, "no_price"},
		{"model-number-1", 400, map[string]any{"model": 1}, "no_price"},
		{"model-bool", 800, map[string]any{"model": true}, "no_price"},
	}
	for _, c := range cases {
		out := r.postLabelled("tokens", c.key, at, at, c.qty, c.dims)
		wantStatus(t, c.key, out, c.want)
	}
	if got := unbilledCount(t, "no_price", "api") - n0; got != 4 {
		t.Errorf("%s{reason=no_price,origin=api} moved by %v, want 4", unbilledMetric, got)
	}

	r.run(lagB.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 15 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 15c: the three matched events only", got)
	}
}

// TestBillingStatus_NoPriceStaysTrueAfterAnEdit is C8: a price added to a
// plan in use applies from each subscriber's next period, so an event the
// new price would take, sent after the edit, is still no_price in the
// current period, and the current invoice leaves it out. In the next period
// the same labels are billed and carry no status.
//
// A used plan's price versions are stamped with the database's wall clock
// (ADR-130 §4), so this test runs its subscription around the real now: the
// period starts three days ago, the edit is stamped now, and the event
// arrives after the edit.
//
// Mutation check: read the prices at the ingest instant instead of the pin
// → the current-period event carries no status but is not billed.
func TestBillingStatus_NoPriceStaysTrueAfterAnEdit(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_edit", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "a"}})
	started := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	r.newSub("sub", plan, started.Add(time.Hour), nil)
	s := r.sub()
	if !s.CurrentBillingPeriodStart.Equal(started) {
		t.Fatalf("period start = %v, want %v", s.CurrentBillingPeriodStart, started)
	}
	end := *s.CurrentBillingPeriodEnd

	edit := r.must(http.MethodPost, "/v1/plans/"+plan+"/prices", started.AddDate(0, 0, 1), map[string]any{
		"meter_id": tokens, "dimension_match": map[string]any{"model": "c"}, "mode": "per_unit", "unit_amount_cents": "1",
	}, http.StatusCreated)
	if edit["effective"] != "next_period" {
		t.Fatalf("price add on a used plan: effective = %v, want next_period", edit["effective"])
	}
	arrive := time.Now().UTC().Add(time.Minute)
	out := r.postLabelled("tokens", "now-c", arrive.Add(-30*time.Second), arrive, 50, map[string]any{"model": "c"})
	wantStatus(t, "current-period event the new price would take", out, "no_price")

	r.run(end.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 0 {
		r.logInvoices(r.invoices())
		t.Errorf("first period usage = %dc, want 0c: the flagged event is not billed", got)
	}
	out = r.postLabelled("tokens", "next-c", end.AddDate(0, 0, 3), end.AddDate(0, 0, 3), 6, map[string]any{"model": "c"})
	wantStatus(t, "next-period event the new price takes", out, "")
	r.run(r.sub().CurrentBillingPeriodEnd.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 6 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want the next period's 6c", got)
	}
}

// TestBillingStatus_SwappedAwayPlanStillBillsItsWindow: the subscription
// swaps from plan A (which prices calls) to plan C (which does not) on April
// 15. A late event stamped April 10 is in plan A's window, which April's
// invoice bills: no status. An event stamped April 18 is in plan C's window:
// not_on_plan, and not billed.
//
// Mutation check: classify against the current items' plans only → the
// April 10 event is answered not_on_plan and then billed.
func TestBillingStatus_SwappedAwayPlanStillBillsItsWindow(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	swapAt := lagP0.AddDate(0, 0, 14).Add(12 * time.Hour)
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, swapAt,
		map[string]any{"new_plan_id": r.planC, "immediate": true}, http.StatusOK)
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagP0) {
		t.Fatalf("after the swap: period start %v, want April's %v", s.CurrentBillingPeriodStart, lagP0)
	}

	arrive := lagP0.AddDate(0, 0, 19)
	out := r.postLabelled("calls", "before-swap", lagP0.AddDate(0, 0, 9), arrive, 8, nil)
	wantStatus(t, "event in plan A's window", out, "")
	out = r.postLabelled("calls", "after-swap", lagP0.AddDate(0, 0, 17), arrive, 300, nil)
	wantStatus(t, "event in plan C's window", out, "not_on_plan")

	r.run(lagB.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 8 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 8c: plan A's window only", got)
	}
}

// TestBillingStatus_RemovedItemStillBillsItsWindow: the subscription has an
// item on plan D, which prices seats, until April 15. A late seats event
// stamped April 10 is in that item's window: no status, billed. One stamped
// April 18 is after it: not_on_plan, not billed.
//
// Mutation check: read billing intervals only when an item joined its plan
// in the period (as the pins do) → the April 10 event is answered
// not_on_plan and then billed.
func TestBillingStatus_RemovedItemStillBillsItsWindow(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	seats := r.newMeter("seats", "sum")
	planD := r.planPricing("plan_d", map[string]any{"meter_id": seats})
	sub := r.must(http.MethodPost, "/v1/subscriptions", lagP0, map[string]any{
		"code": "two-items", "display_name": "Two items", "customer_id": r.custID,
		"items": []map[string]any{{"plan_id": r.planA, "quantity": 1}, {"plan_id": planD, "quantity": 1}},
	}, http.StatusCreated)
	r.subID = sub["id"].(string)
	var itemD string
	for _, it := range r.sub().Items {
		if it.PlanID == planD {
			itemD = it.ID
		}
	}
	removeAt := lagP0.AddDate(0, 0, 14).Add(12 * time.Hour)
	if code, raw := r.do(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/items/"+itemD, removeAt, nil); code != http.StatusNoContent {
		t.Fatalf("remove plan D's item: %d %s, want 204", code, raw)
	}

	arrive := lagP0.AddDate(0, 0, 19)
	out := r.postLabelled("seats", "before-removal", lagP0.AddDate(0, 0, 9), arrive, 8, nil)
	wantStatus(t, "event in the removed item's window", out, "")
	out = r.postLabelled("seats", "after-removal", lagP0.AddDate(0, 0, 17), arrive, 300, nil)
	wantStatus(t, "event after the removal", out, "not_on_plan")

	r.run(lagB.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 8 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 8c: the removed item's window only", got)
	}
}

// TestBillingStatus_BatchAnswersPerIndex: /batch answers each event's result
// and billing_status at its index, and counts each status. A replayed event
// is deduplicated and carries no status.
//
// Mutation check: index the statuses by inserted row instead of by request
// index → the statuses shift past the replayed event.
func TestBillingStatus_BatchAnswersPerIndex(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	r.newMeter("seats", "sum")
	plan := r.planPricing("plan_batch", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "a"}})
	r.newSub("sub", plan, lagP0.AddDate(0, 0, 1), nil)
	at := lagP0.AddDate(0, 0, 4)
	r.postLabelled("tokens", "stored", at, at, 1, map[string]any{"model": "a"})

	batch := []map[string]any{
		r.labelled("tokens", "b-billed", at, 2, map[string]any{"model": "a"}),
		r.labelled("tokens", "stored", at, 1, map[string]any{"model": "a"}),
		r.labelled("tokens", "b-no-price", at, 4, map[string]any{"model": "z"}),
		r.labelled("seats", "b-not-on-plan", at, 8, nil),
		r.labelled("tokens", "b-outside", lagP0.Add(-time.Hour), 16, map[string]any{"model": "a"}),
	}
	code, out, _ := r.doH(http.MethodPost, "/v1/usage-events/batch", at.Add(time.Hour), batch)
	if code != http.StatusCreated {
		t.Fatalf("batch: %d %v, want 201", code, out)
	}
	want := map[string]int{"ingested": 4, "deduplicated": 1, "total": 5, "no_price": 1, "not_on_plan": 1, "outside_subscription": 1, "after_close": 0}
	for k, v := range want {
		if got, ok := out[k].(float64); !ok || int(got) != v {
			t.Errorf("batch %s = %v, want %d", k, out[k], v)
		}
	}
	results, _ := out["results"].([]any)
	if len(results) != len(batch) {
		t.Fatalf("batch results = %v, want one per event", out["results"])
	}
	wantResults := []struct{ status, billing string }{
		{"ingested", ""}, {"deduplicated", ""}, {"ingested", "no_price"}, {"ingested", "not_on_plan"}, {"ingested", "outside_subscription"},
	}
	for i, w := range wantResults {
		res, _ := results[i].(map[string]any)
		if res["status"] != w.status {
			t.Errorf("results[%d].status = %v, want %s", i, res["status"], w.status)
		}
		wantStatus(t, fmt.Sprintf("results[%d]", i), res, w.billing)
	}

	// A replay of the whole batch: every event deduplicated, no status.
	code, out, _ = r.doH(http.MethodPost, "/v1/usage-events/batch", at.Add(2*time.Hour), batch)
	if code != http.StatusCreated || count(out, "deduplicated") != 5 || count(out, "no_price") != 0 {
		t.Errorf("batch replay: %d %v, want deduplicated 5 and no_price 0", code, out)
	}
	results, _ = out["results"].([]any)
	for i, res := range results {
		m, _ := res.(map[string]any)
		if m["status"] != "deduplicated" {
			t.Errorf("replay results[%d].status = %v, want deduplicated", i, m["status"])
		}
		wantStatus(t, fmt.Sprintf("replay results[%d]", i), m, "")
	}
}

// TestBillingStatus_LiteLLMCounts: a LiteLLM call whose model no price takes
// answers no_price 2 (its input and output rows), and a call for a customer
// with no subscription answers outside_subscription 2. Neither is billed.
func TestBillingStatus_LiteLLMCounts(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_llm", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "claude-x"}})

	out := r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagP0.AddDate(0, 0, 1),
		litellmCall("call-no-sub", r.custExt, lagP0.AddDate(0, 0, 1), 10, 5), http.StatusOK)
	if count(out, "accepted") != 2 || count(out, "outside_subscription") != 2 {
		t.Errorf("call before any subscription = %v, want accepted 2 and outside_subscription 2", out)
	}

	r.newSub("llm", plan, lagP0.AddDate(0, 0, 2), nil)
	out = r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagP0.AddDate(0, 0, 4),
		litellmCall("call-gpt", r.custExt, lagP0.AddDate(0, 0, 4), 7, 3), http.StatusOK)
	if count(out, "accepted") != 2 || count(out, "no_price") != 2 || count(out, "not_on_plan") != 0 {
		t.Errorf("gpt-4o call = %v, want accepted 2 and no_price 2", out)
	}
	out = r.must(http.MethodPost, "/v1/integrations/litellm/spend", lagP0.AddDate(0, 0, 5),
		litellmCall("call-gpt", r.custExt, lagP0.AddDate(0, 0, 4), 7, 3), http.StatusOK)
	if count(out, "deduplicated") != 2 || count(out, "no_price") != 0 {
		t.Errorf("replay = %v, want deduplicated 2 and no_price 0", out)
	}

	r.run(r.sub().CurrentBillingPeriodEnd.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 0 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 0c", got)
	}
}

// TestBillingStatus_HeldReleaseCountsNoPrice: held LiteLLM rows released
// into a subscription whose plan prices tokens for another model only count
// as no_price, never in_open_period.
//
// Mutation check: classify released rows without their labels →
// in_open_period 2.
func TestBillingStatus_HeldReleaseCountsNoPrice(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_held", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "claude-x"}})
	r.newSub("held", plan, lagP0, nil)
	out := r.heldRelease(lagP0.AddDate(0, 0, 3), lagP0.AddDate(0, 0, 4))
	if count(out, "released") != 2 || count(out, "no_price") != 2 || count(out, "in_open_period") != 0 {
		t.Errorf("release answer = %v, want released 2, no_price 2, in_open_period 0", out)
	}
	r.run(lagB.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 0 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 0c", got)
	}
}
