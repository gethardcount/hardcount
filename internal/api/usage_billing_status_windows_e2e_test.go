package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// billing_status follows the windows the close bills a meter's usage over,
// not only the period: the trial end and another subscription's coverage end
// move a window's start, a scheduled cancel ends it, and an instant of a
// meter is billed by one plan only. And a price set is a flag's evidence only
// once its pin has passed: a price saved before the pin is in force there.
// Each test checks the answers against the invoices the close builds.

// TestBillingStatus_TrialEndDayBeforeTheTrialEnd: the trial ends at 18:00 on
// day D, and the first paid period starts at midnight of D. Usage before
// 18:00 is trial usage, which no invoice bills: outside_subscription, sent
// while trialing or after the expiry scan made the subscription active. An
// event after the trial end is billed and carries no status. So is an event
// of a meter the plan does not price: trial time, not not_on_plan.
//
// Mutation check: drop the trial end from classify → the seats event is
// not_on_plan. (The calls events are also outside the close's windows,
// which start at the trial end.)
func TestBillingStatus_TrialEndDayBeforeTheTrialEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newMeter("seats", "sum")
	r.newTrialEndingAt(dayAt(18, 0))

	out := r.postLabelled("calls", "trial-0900", dayAt(9, 0), dayAt(9, 1), 40, nil)
	wantStatus(t, "trial-end-day event sent while trialing", out, "outside_subscription")
	out = r.postLabelled("seats", "trial-seats-0930", dayAt(9, 30), dayAt(9, 31), 2, nil)
	wantStatus(t, "trial-end-day event of an unpriced meter", out, "outside_subscription")
	r.expireTrials(dayAt(19, 45))
	out = r.postLabelled("calls", "trial-1100", dayAt(11, 0), dayAt(19, 50), 3, nil)
	wantStatus(t, "trial-end-day event sent after the expiry scan", out, "outside_subscription")
	out = r.postLabelled("calls", "paid-1900", dayAt(19, 0), dayAt(19, 55), 7, nil)
	wantStatus(t, "event after the trial end", out, "")

	if got := r.closePeriod(uwDay).sum("usage"); got != 7 {
		t.Errorf("first paid invoice usage = %dc, want 7c: only the event after the trial end", got)
	}
}

// TestBillingStatus_AfterAScheduledMidPeriodCancel: a cancel is scheduled for
// May 11, inside May. Until the run fires it the subscription is active, but
// its final invoice bills usage only to May 11. An event stamped after May 11
// is outside_subscription; one before it is billed and carries no status.
//
// Mutation check: drop the scheduled cancel from classify → the event after
// the cancel carries no status.
func TestBillingStatus_AfterAScheduledMidPeriodCancel(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	cancelAt := lagB.AddDate(0, 0, 10)
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/schedule-cancel", lagP0.AddDate(0, 0, 5),
		map[string]any{"cancel_at": cancelAt.Format(time.RFC3339)}, http.StatusOK)
	r.run(lagB.Add(2 * time.Hour))

	out := r.postLabelled("calls", "before-cancel", cancelAt.Add(-time.Hour), cancelAt.Add(-50*time.Minute), 5, nil)
	wantStatus(t, "event before the scheduled cancel", out, "")
	out = r.postLabelled("calls", "after-cancel", cancelAt.Add(20*time.Minute), cancelAt.Add(30*time.Minute), 11, nil)
	if s := r.sub(); s.Status != domain.SubscriptionActive {
		t.Fatalf("status when the late event arrived = %s, want active (the cancel has not fired)", s.Status)
	}
	wantStatus(t, "event after the scheduled cancel", out, "outside_subscription")

	r.run(cancelAt.Add(e2eGrace + 5*time.Minute))
	if s := r.sub(); s.Status != domain.SubscriptionCanceled {
		t.Fatalf("status after the cancel ran = %s, want canceled", s.Status)
	}
	if got := usageBilled(r.invoices()); got != 5 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 5c: the final invoice ends at the cancel", got)
	}
}

// TestBillingStatus_BeforeAnotherSubscriptionsCoverageEnd: subscription A is
// canceled at 10:00 on day D and its final invoice bills the meter to 10:00.
// B starts on the meter at 11:00, so its period starts at midnight, but its
// close bills the meter only from 10:00. A late event stamped 09:30 is on no
// invoice: outside_subscription (A has ended). One stamped 12:15 is B's.
//
// Mutation checks: build the windows without the coverage ends, or classify
// by the stretches, not the windows → the 09:30 event carries no status.
func TestBillingStatus_BeforeAnotherSubscriptionsCoverageEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_advance", 0)
	arrears := r.newPlan("plan_arrears", "in_arrears", 2000)
	subA := r.newSub("sub-a", r.planA, dayAt(8, 0), nil)
	r.ingest(dayAt(9, 0), 100)
	r.cancelNow(dayAt(10, 0))
	r.newSub("sub-b", arrears, dayAt(11, 0), nil)
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
	skipShort(t)
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

// TestBillingStatus_GraceEventBeforeTheNextPinIsNotFlagged: a test clock is
// parked three minutes before its subscription's period end P1. An event
// stamped one minute after P1 (inside the future skew) is in the next period,
// priced at P1. P1 has not passed on the clock, so a price saved now is in
// force at P1: the answer cannot say no_price. The price for model c is
// saved, and the next period's invoice bills the event. Since review round 3
// an event after the period end is not checked at all.
//
// Mutation check: classify an event after the period end by the period's
// plans → the event is answered no_price and then billed.
func TestBillingStatus_GraceEventBeforeTheNextPinIsNotFlagged(t *testing.T) {
	r := newPPRig(t, "next pin ahead")
	tokens := r.meter("tokens", "sum")
	plan, _ := r.plan("pro", perUnit(tokens, "1", map[string]string{"model": "a"}))
	clk := r.clock("pin ahead", time.Now().UTC().AddDate(0, 0, -100).Truncate(time.Second))
	sub, _, p1 := r.clockSub("pp-pin", clk, plan)
	parked := p1.Add(-3 * time.Minute)
	r.advance(clk, parked)

	code, out, raw := r.do(parked, http.MethodPost, "/v1/usage-events", map[string]any{
		"external_customer_id": "pp-pin", "event_name": "tokens", "quantity": 9,
		"timestamp": p1.Add(time.Minute).Format(time.RFC3339Nano), "idempotency_key": "pin-ahead",
		"dimensions": map[string]any{"model": "c"},
	})
	if code != http.StatusCreated {
		t.Fatalf("POST event: %d %s, want 201", code, raw)
	}
	wantStatus(t, "event after P1 sent before P1 on the clock", out, "")

	added := r.must(time.Time{}, http.MethodPost, "/v1/plans/"+plan+"/prices", perUnit(tokens, "1", map[string]string{"model": "c"}), http.StatusCreated)
	if got := flowStr(added, "effective"); got != "next_period" {
		t.Fatalf("price add: effective %q, want next_period", got)
	}
	r.advance(clk, p1.AddDate(0, 1, 0).Add(time.Hour))
	if got := r.cycle(sub, p1).usageCents(); got != 9 {
		t.Errorf("next period usage = %dc, want 9c: the price saved before P1 bills the event", got)
	}
}

// TestBillingStatus_HeldReleaseCountsTheUnbilledMetric: rows the held-usage
// release writes with a billing_status are counted in
// velox_usage_unbilled_total, as every other written row is (origin
// backfill, as the release writes them).
//
// Mutation check: drop the count from Release → the counter does not move.
func TestBillingStatus_HeldReleaseCountsTheUnbilledMetric(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_held_metric", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"model": "gpt-4o", "token_type": "input"}})
	r.newSub("held", plan, lagP0, nil)
	n0 := unbilledCount(t, "no_price", string(domain.UsageOriginBackfill))
	out := r.heldRelease(lagP0.AddDate(0, 0, 3), lagP0.AddDate(0, 0, 4))
	if count(out, "no_price") != 1 {
		t.Fatalf("release answer = %v, want no_price 1", out)
	}
	if got := unbilledCount(t, "no_price", string(domain.UsageOriginBackfill)) - n0; got != 1 {
		t.Errorf("%s{reason=no_price,origin=backfill} moved by %v, want 1", unbilledMetric, got)
	}
}

// TestBillingStatus_TrialCanceledAtPeriodEnd: the trial ends at 18:00 on day
// D and a cancel at period end is scheduled at 10:00. If nothing changes,
// the trial scan cancels the subscription free at its trial end (ADR-069)
// and no invoice bills its usage. But until that scan runs the cancel can
// still be undone, and then the usage is billed
// (TestBillingStatus_TrialCancelUndoneAfterTheTrialEnd). So an event after
// the trial end, sent before the scan, is not checked and carries no
// status, also one sent inside the future skew before the trial end
// (review round 4 on #1390; round 2 had flagged both).
//
// Mutation check: answer the trial end of a trialing subscription with a
// cancel at period end as its end → both events are outside_subscription.
func TestBillingStatus_TrialCanceledAtPeriodEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newTrialEndingAt(dayAt(18, 0))
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/schedule-cancel", dayAt(10, 0),
		map[string]any{"at_period_end": true}, http.StatusOK)

	out := r.postLabelled("calls", "skew-1802", dayAt(18, 2), dayAt(17, 58), 3, nil)
	wantStatus(t, "event after the trial end, sent inside the skew before it", out, "")
	out = r.postLabelled("calls", "after-1830", dayAt(18, 30), dayAt(18, 40), 7, nil)
	if s := r.sub(); s.Status != domain.SubscriptionTrialing {
		t.Fatalf("status when the event arrived = %s, want trialing (the expiry scan has not run)", s.Status)
	}
	wantStatus(t, "event after the trial end, before the expiry scan", out, "")

	r.clk.Set(dayAt(19, 0))
	if _, errs := r.srv.SubscriptionSvc.ProcessExpiredTrials(uwCtx(dayAt(19, 0)), 100); len(errs) != 0 {
		t.Fatalf("trial expiry: %v", errs)
	}
	if s := r.sub(); s.Status != domain.SubscriptionCanceled {
		t.Fatalf("status after the expiry scan = %s, want canceled", s.Status)
	}
	out = r.postLabelled("calls", "after-1830-late", dayAt(18, 35), dayAt(19, 5), 1, nil)
	wantStatus(t, "event after the trial end, sent after the scan canceled it", out, "outside_subscription")
	r.run(dayAt(20, 0))
	if got := usageBilled(r.invoices()); got != 0 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 0c: the trial canceled free at its end", got)
	}
}

// TestBillingStatus_TrialCancelUndoneAfterTheTrialEnd: the trial ends at
// 18:00 on day D with a cancel at period end. An event at 18:30 arrives at
// 18:40, before the trial scan. At 18:45 the operator undoes the cancel,
// which the subscription still allows: it is trialing. The scan then
// activates it at its trial end, and the first paid invoice bills the event.
// The event is in time Velox does not check (after the trial end of a
// subscription still trialing), so it carries no status (review round 4 on
// #1390); the undo is a later change, and the invoice is the final answer.
//
// Mutation check: answer the trial end of a trialing subscription with a
// cancel at period end as its end → the event is outside_subscription, and
// then billed.
func TestBillingStatus_TrialCancelUndoneAfterTheTrialEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newTrialEndingAt(dayAt(18, 0))
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/schedule-cancel", dayAt(10, 0),
		map[string]any{"at_period_end": true}, http.StatusOK)

	out := r.postLabelled("calls", "after-1830", dayAt(18, 30), dayAt(18, 40), 7, nil)
	wantStatus(t, "event after the trial end, before the expiry scan", out, "")
	r.must(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/scheduled-cancel", dayAt(18, 45), nil, http.StatusOK)

	r.expireTrials(dayAt(19, 0))
	if got := r.closePeriod(uwDay).sum("usage"); got != 7 {
		t.Errorf("first paid invoice usage = %dc, want 7c: the event after the trial end", got)
	}
}

// TestBillingStatus_TrialExtendedAfterItsEnd: the trial ends at 18:00 on day
// D. An event at 18:30 arrives at 18:40, before the trial scan. At 18:45 the
// operator extends the trial to 22:00, which the subscription still allows:
// it is trialing. The event is then trial usage, which no invoice bills. The
// answer is the stated one: an event after a trial end that the scan has not
// processed is not checked, so it carries no status (review round 4 on
// #1390).
func TestBillingStatus_TrialExtendedAfterItsEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newTrialEndingAt(dayAt(18, 0))

	out := r.postLabelled("calls", "after-1830", dayAt(18, 30), dayAt(18, 40), 7, nil)
	wantStatus(t, "event after the trial end, before the expiry scan", out, "")
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/extend-trial", dayAt(18, 45),
		map[string]any{"trial_end": dayAt(22, 0).Format(time.RFC3339)}, http.StatusOK)

	r.expireTrials(dayAt(22, 30))
	if got := r.closePeriod(uwDay).sum("usage"); got != 0 {
		t.Errorf("first paid invoice usage = %dc, want 0c: the event became trial usage", got)
	}
}

// TestBillingStatus_AnotherSubscriptionsGraceIsNotChecked: subscription A
// (plan A, calls only) runs from Apr 1, and B (plan C, no meter) from Apr
// 10. Neither prices seats. A seats event inside both open periods is
// not_on_plan. A seats event after B's period end, before B's invoice is
// built, is in B's next period, which Velox does not check: it carries no
// status, although A's open period holds it and does not price seats. This
// is the stated rule (review round 4 on #1390). After B's invoice is built,
// the same kind of event is not_on_plan again. No invoice bills a seats
// event.
func TestBillingStatus_AnotherSubscriptionsGraceIsNotChecked(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	subA := r.subID
	r.newMeter("seats", "sum")
	subB := r.newSub("sub-b", r.planC, lagP0.AddDate(0, 0, 9), nil)
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

// TestBillingStatus_OutsideSubscription_DraftActivatedLaterThatDay pins a
// later change, activation: a draft created the day before and
// activated at 15:00 on day D starts its first period at midnight of D
// (ADR-012), so it bills an event answered outside_subscription at 09:00
// that day (review round 4 on #1390).
func TestBillingStatus_OutsideSubscription_DraftActivatedLaterThatDay(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newSub("draft", r.planA, dayAt(-6, 0), map[string]any{"status": "draft"})
	out := r.postLabelled("calls", "d-0900", dayAt(9, 0), dayAt(9, 5), 9, nil)
	wantStatus(t, "event while the subscription is a draft", out, "outside_subscription")

	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/activate", dayAt(15, 0), nil, http.StatusOK)
	if s := r.sub(); s.CurrentBillingPeriodStart == nil || !s.CurrentBillingPeriodStart.Equal(uwDay) {
		t.Fatalf("period start after the activation = %v, want midnight of the day %v", s.CurrentBillingPeriodStart, uwDay)
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
	skipShort(t)
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
