package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Review round 3 on #1390. A subscription's first period starts at midnight
// of its first day (ADR-012), and the close bills its usage from there, not
// from the instant it started. And an event after the current period's end
// is billed by the prices of its own period, which ingest does not know yet
// when the close is behind. Each test checks the answers against the
// invoices the close builds.

// TestBillingStatus_FirstDayBeforeTheStartBeforeAPaidThreshold: the
// subscription starts at 11:00 on day D with no trial, so its first period
// starts at midnight of D. A threshold fire at 20:00 bills calls from
// midnight to 20:00, and its invoice is paid. A late 09:00 event is before
// the fire's end, like one at 11:30: the close bills calls only from 20:00,
// so neither is billed, and both are after_close.
//
// Mutation check: draw the threshold flag only past the subscription's
// start instant → the 09:00 event carries no status.
func TestBillingStatus_FirstDayBeforeTheStartBeforeAPaidThreshold(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newSub("mid-day", r.planA, dayAt(11, 0), nil)
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(uwDay) {
		t.Fatalf("period start = %v, want midnight of day D %v", s.CurrentBillingPeriodStart, uwDay)
	}
	r.ingest(dayAt(12, 0), 100)
	fireAt := dayAt(20, 0)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(-time.Hour),
		map[string]any{"amount_gte": 100}, http.StatusOK)
	if n := r.scanThresholds(fireAt); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	var thr string
	for _, inv := range r.invoices() {
		if inv.reason == string(domain.BillingReasonThreshold) {
			thr = inv.id
		}
	}
	if thr == "" {
		t.Fatal("no threshold invoice")
	}
	r.must(http.MethodPost, "/v1/invoices/"+thr+"/record-payment", fireAt.Add(30*time.Minute), map[string]any{"note": "wire"}, http.StatusOK)

	out := r.postLabelled("calls", "late-0900", dayAt(9, 0), fireAt.Add(time.Hour), 7, nil)
	wantStatus(t, "late event before the start instant, before the paid fire's end", out, "after_close")
	out = r.postLabelled("calls", "late-1130", dayAt(11, 30), fireAt.Add(time.Hour), 3, nil)
	wantStatus(t, "late event after the start instant, before the paid fire's end", out, "after_close")

	r.closePeriod(uwDay)
	if got := usageBilled(r.invoices()); got != 100 {
		r.logInvoices(r.invoices())
		t.Errorf("usage on all invoices = %dc, want 100c: the fire's 100 calls, neither late event", got)
	}
}

// TestBillingStatus_FirstDayBeforeTheStartInTheInvoicedFirstPeriod: the
// subscription starts at 11:00 on day D with no trial. A 09:00 event sent
// at 12:00 is billed by the first invoice: the period starts at midnight. A
// 09:30 event sent after that invoice is after_close, as any late event of
// an invoiced period is. It is not outside_subscription: the subscription
// billed that time.
//
// Mutation check: take the floor from the start instant, not the first
// billed instant → the late event is outside_subscription.
func TestBillingStatus_FirstDayBeforeTheStartInTheInvoicedFirstPeriod(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newSub("mid-day", r.planA, dayAt(11, 0), nil)
	out := r.postLabelled("calls", "early-0900", dayAt(9, 0), dayAt(12, 0), 5, nil)
	wantStatus(t, "event before the start instant, in the open first period", out, "")
	if got := r.closePeriod(uwDay).sum("usage"); got != 5 {
		t.Fatalf("first invoice usage = %dc, want 5c: the period starts at midnight", got)
	}

	n0 := unbilledCount(t, "after_close", "api")
	arrive := r.sub().CurrentBillingPeriodStart.Add(3 * time.Hour)
	out = r.postLabelled("calls", "late-0930", dayAt(9, 30), arrive, 4, nil)
	wantStatus(t, "late event before the start instant, first period invoiced", out, "after_close")
	if got := unbilledCount(t, "after_close", "api") - n0; got != 1 {
		t.Errorf("%s{reason=after_close,origin=api} moved by %v, want 1", unbilledMetric, got)
	}
}

// TestBillingStatus_LaterPeriodWhileTheCloseIsBehind: no billing run has
// closed April, so on June 10 the current period is still April. A price
// for model b is added on May 20 and the model c price is removed then. The
// close bills June at June's prices: the June model b event is billed, and
// the model c event is not. Ingest does not know June's prices while the
// close is behind, so it answers neither with a status. It must not answer
// no_price for the model b event, which is billed.
//
// Mutation check: classify an event after the period end by the current
// period's plans → the model b event is no_price, and then billed.
func TestBillingStatus_LaterPeriodWhileTheCloseIsBehind(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_behind",
		map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "a"}},
		map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "c"}})
	r.newSub("behind", plan, lagP0, nil)

	// A used plan's price write is in force from the instant it is saved,
	// on the database clock (ADR-130 §4). The rig serves May 20 on a fake
	// clock, so the versions are moved to May 20, where that write would
	// have put them.
	priceAt := lagB.AddDate(0, 0, 19)
	added := r.must(http.MethodPost, "/v1/plans/"+plan+"/prices", priceAt, map[string]any{
		"meter_id": tokens, "dimension_match": map[string]any{"model": "b"}, "mode": "per_unit", "unit_amount_cents": "1",
	}, http.StatusCreated)
	if added["effective"] != "next_period" {
		t.Fatalf("price add on a used plan: effective = %v, want next_period", added["effective"])
	}
	var priceC string
	for _, p := range r.must(http.MethodGet, "/v1/plans/"+plan+"/prices", priceAt, nil, http.StatusOK)["data"].([]any) {
		pm := p.(map[string]any)
		if dm, _ := pm["dimension_match"].(map[string]any); dm["model"] == "c" {
			priceC = pm["id"].(string)
		}
	}
	if priceC == "" {
		t.Fatal("no model c price")
	}
	r.must(http.MethodDelete, "/v1/plans/"+plan+"/prices/"+priceC, priceAt, nil, http.StatusOK)
	r.exec(`UPDATE plan_price_versions SET valid_from = $1 WHERE id IN (
		SELECT DISTINCT ON (price_id) id FROM plan_price_versions WHERE price_id IN ($2, $3) ORDER BY price_id, valid_from DESC)`,
		priceAt, added["id"], priceC)

	arrive := lagP2.AddDate(0, 0, 9)
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagP0) {
		t.Fatalf("period start = %v, want April's %v: no run has closed it", s.CurrentBillingPeriodStart, lagP0)
	}
	out := r.postLabelled("tokens", "june-b", lagP2.AddDate(0, 0, 8), arrive, 10, map[string]any{"model": "b"})
	wantStatus(t, "June model b event, close behind", out, "")
	out = r.postLabelled("tokens", "june-c", lagP2.AddDate(0, 0, 8), arrive, 100, map[string]any{"model": "c"})
	wantStatus(t, "June model c event, close behind", out, "")

	r.run(lagP2.AddDate(0, 1, 0).Add(2 * time.Hour))
	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagP2).sum("usage"); got != 10 {
		r.logInvoices(invs)
		t.Errorf("June usage = %dc, want 10c: the model b price is in force in June, the model c price is not", got)
	}
	if got := usageBilled(invs); got != 10 {
		r.logInvoices(invs)
		t.Errorf("usage on all invoices = %dc, want 10c", got)
	}
}
