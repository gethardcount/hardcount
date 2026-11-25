package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// billing_status follows the windows the close bills a meter's usage over,
// not only the period: another subscription's coverage end moves a window's
// start, and an instant of a meter is billed by one plan only. Each test
// checks the answers against the invoices the close builds.

// TestBillingStatus_BeforeAnotherSubscriptionsCoverageEnd: subscription A is
// canceled at 10:00 on day D and its final invoice bills the meter to 10:00.
// B starts on the meter at 11:00, so its period starts at midnight, but its
// close bills the meter only from 10:00. A late event stamped 09:30 is on no
// invoice: outside_subscription (A has ended). One stamped 12:15 is B's.
//
// Mutation checks: build the windows without the coverage ends, or classify
// by the stretches, not the windows → the 09:30 event carries no status.
func TestBillingStatus_BeforeAnotherSubscriptionsCoverageEnd(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	arrears := r.newPlan("plan_arrears", "in_arrears", 2000)
	subA := r.newSub("sub-a", r.planA, dayAt(8, 0))
	r.ingest(dayAt(9, 0), 100)
	r.cancelNow(dayAt(10, 0))
	r.newSub("sub-b", arrears, dayAt(11, 0))
	r.ingest(dayAt(12, 0), 20)

	out := r.postLabelled("calls", "late-0930", dayAt(9, 30), dayAt(12, 30), 13, nil)
	wantStatus(t, "event before A's coverage end, sent after B started", out, "outside_subscription")
	out = r.postLabelled("calls", "b-1215", dayAt(12, 15), dayAt(12, 30), 4, nil)
	wantStatus(t, "event in B's window", out, "")

	if got := r.closePeriod(uwDay).sum("usage"); got != 24 {
		t.Errorf("B's usage = %dc, want 24c: the 09:30 event is before B's window", got)
	}
	if got := usageOf(r.invoicesOf(subA)); got != 100 {
		t.Errorf("A's usage = %dc, want 100c (unchanged)", got)
	}
}

// TestBillingStatus_FirstDayReachBackKeepsTheEarlierPlan: an item on plan X
// (tokens priced for model a only) is removed at 10:00 on May 1, the first
// day of May, and an item on plan Y (every tokens event) is added at 12:00.
// The added item bills from the period start (ADR-012), but the close gives
// the time before 10:00 to X's item, which was on the meter then. So a model
// b event at 09:00 is billed by X, whose prices do not take it: no_price. A
// model b event at 11:00 is Y's and billed.
//
// Mutation check: classify by the best of every stretch holding the instant,
// not the one window that holds it → the 09:00 model b event carries no
// status.
func TestBillingStatus_FirstDayReachBackKeepsTheEarlierPlan(t *testing.T) {
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	planX := r.planPricing("plan_x", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "a"}})
	planY := r.planPricing("plan_y_all", map[string]any{"meter_id": tokens})
	sub := r.must(http.MethodPost, "/v1/subscriptions", lagP0, map[string]any{
		"code": "reach-back", "display_name": "Reach back", "customer_id": r.custID,
		"items": []map[string]any{{"plan_id": planX, "quantity": 1}, {"plan_id": r.planC, "quantity": 1}},
	}, http.StatusCreated)
	r.subID = sub["id"].(string)
	var itemX string
	for _, it := range r.sub().Items {
		if it.PlanID == planX {
			itemX = it.ID
		}
	}
	r.run(lagB.Add(2 * time.Hour))

	if code, raw := r.do(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/items/"+itemX, lagB.Add(10*time.Hour), nil); code != http.StatusNoContent {
		t.Fatalf("remove X's item: %d %s, want 204", code, raw)
	}
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/items", lagB.Add(12*time.Hour), map[string]any{"plan_id": planY, "quantity": 1}, http.StatusCreated)

	arrive := lagB.Add(13 * time.Hour)
	cases := []struct {
		key   string
		at    time.Time
		qty   int64
		model string
		want  string
	}{
		{"x-a-0900", lagB.Add(9 * time.Hour), 5, "a", ""},
		{"x-b-0900", lagB.Add(9 * time.Hour), 17, "b", "no_price"},
		{"y-b-1100", lagB.Add(11 * time.Hour), 3, "b", ""},
	}
	for _, c := range cases {
		out := r.postLabelled("tokens", c.key, c.at, arrive, c.qty, map[string]any{"model": c.model})
		wantStatus(t, c.key, out, c.want)
	}

	r.run(lagP2.Add(2 * time.Hour))
	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagB).sum("usage"); got != 8 {
		r.logInvoices(invs)
		t.Errorf("May usage = %dc, want 8c: model a at 09:00 (X) and model b at 11:00 (Y)", got)
	}
}

// TestBillingStatus_HeldReleaseCountsTheUnbilledMetric: rows the held-usage
// release writes with a billing_status are counted in
// hardcount_usage_unbilled_total, as every other written row is (origin
// backfill, as the release writes them).
//
// Mutation check: drop the count from Release → the counter does not move.
func TestBillingStatus_HeldReleaseCountsTheUnbilledMetric(t *testing.T) {
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_held_metric", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "gpt-4o", "token_type": "input"}})
	r.newSub("held", plan, lagP0)
	n0 := unbilledCount(t, "no_price", string(domain.UsageOriginBackfill))
	out := r.heldRelease(lagP0.AddDate(0, 0, 3), lagP0.AddDate(0, 0, 4))
	if count(out, "no_price") != 1 {
		t.Fatalf("release answer = %v, want no_price 1", out)
	}
	if got := unbilledCount(t, "no_price", string(domain.UsageOriginBackfill)) - n0; got != 1 {
		t.Errorf("%s{reason=no_price,origin=backfill} moved by %v, want 1", unbilledMetric, got)
	}
}

// TestBillingStatus_AnotherSubscriptionsGraceIsNotChecked: subscription A
// (plan A, calls only) runs from Apr 1, and B (plan C, no meter) from Apr
// 10. Neither prices seats. A seats event inside both open periods is
// not_on_plan. A seats event after B's period end, before B's invoice is
// built, is in B's next period, which Hardcount does not check: it carries no
// status, although A's open period holds it and does not price seats. This
// is the stated rule (review round 4 on #1390). After B's invoice is built,
// the same kind of event is not_on_plan again. No invoice bills a seats
// event.
func TestBillingStatus_AnotherSubscriptionsGraceIsNotChecked(t *testing.T) {
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	subA := r.subID
	r.newMeter("seats", "sum")
	subB := r.newSub("sub-b", r.planC, lagP0.AddDate(0, 0, 9))
	r.run(lagB.Add(2 * time.Hour))
	bEnd := *r.sub().CurrentBillingPeriodEnd

	out := r.postLabelled("seats", "seats-before", bEnd.Add(-2*time.Hour), bEnd.Add(-time.Hour), 3, nil)
	wantStatus(t, "seats event inside both open periods", out, "not_on_plan")
	out = r.postLabelled("seats", "seats-in-b-grace", bEnd.Add(10*time.Minute), bEnd.Add(20*time.Minute), 3, nil)
	wantStatus(t, "seats event after B's period end, before B's invoice", out, "")

	r.run(bEnd.Add(2 * time.Hour))
	out = r.postLabelled("seats", "seats-after-b-close", bEnd.Add(3*time.Hour), bEnd.Add(3*time.Hour), 3, nil)
	wantStatus(t, "seats event after B's invoice", out, "not_on_plan")

	r.run(lagP2.Add(2 * time.Hour))
	if got := usageOf(r.invoicesOf(subA)) + usageOf(r.invoicesOf(subB)); got != 0 {
		t.Errorf("usage billed = %dc, want 0c: no plan prices seats", got)
	}
}

// TestBillingStatus_OutsideSubscription_CreatedLaterThatDay pins a later
// change, a create: a subscription created at 15:00 on day D starts its
// first period at midnight of D (ADR-012), so it bills an event answered
// outside_subscription at 09:00 that day.
func TestBillingStatus_OutsideSubscription_CreatedLaterThatDay(t *testing.T) {
	r := newLagRigBare(t, "in_arrears", 0)
	out := r.postLabelled("calls", "d-0900", dayAt(9, 0), dayAt(9, 5), 9, nil)
	wantStatus(t, "event before the subscription is created", out, "outside_subscription")

	r.newSub("later", r.planA, dayAt(15, 0))
	if s := r.sub(); s.CurrentBillingPeriodStart == nil || !s.CurrentBillingPeriodStart.Equal(uwDay) {
		t.Fatalf("period start after the create = %v, want midnight of the day %v", s.CurrentBillingPeriodStart, uwDay)
	}
	if got := r.closePeriod(uwDay).sum("usage"); got != 9 {
		t.Errorf("first invoice usage = %dc, want 9c: the 09:00 event is in the first period", got)
	}
}

// TestBillingStatus_AfterCloseOnAPlanSwappedAwayAtTheBoundary: the
// subscription is on plan A (prices calls) in April, with a change to plan C
// (no meter) scheduled for May 1. April's invoice bills calls on plan A. An
// April event sent after that invoice is after_close: its period was
// invoiced, whatever plan the subscription is on now. It is not not_on_plan,
// which would say that adding a price to the plan bills it.
//
// Mutation check: answer an invoiced period by the current plans' meters →
// the April event is not_on_plan.
func TestBillingStatus_AfterCloseOnAPlanSwappedAwayAtTheBoundary(t *testing.T) {
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 3), 5)
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagP0.AddDate(0, 0, 9),
		map[string]any{"new_plan_id": r.planC, "immediate": false}, http.StatusOK)
	r.run(lagB.Add(2 * time.Hour))
	if s := r.sub(); s.Items[0].PlanID != r.planC {
		t.Fatalf("plan after April's close = %s, want plan C", s.Items[0].PlanID)
	}

	n0 := unbilledCount(t, "after_close", "api")
	out := r.postLabelled("calls", "april-late", lagP0.AddDate(0, 0, 20), lagB.Add(3*time.Hour), 6, nil)
	wantStatus(t, "April event after April's invoice, now on a plan without the meter", out, "after_close")
	if got := unbilledCount(t, "after_close", "api") - n0; got != 1 {
		t.Errorf("%s{reason=after_close,origin=api} moved by %v, want 1", unbilledMetric, got)
	}

	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagP0).sum("usage"); got != 5 {
		r.logInvoices(invs)
		t.Errorf("April usage = %dc, want 5c: plan A billed calls, not the late event", got)
	}
}
