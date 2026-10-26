package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/doctor"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// One usage window for every closer (ADR-012 amendment 2026-10-22). Usage is
// aggregated per customer and meter, with no subscription key, and a
// subscription's period starts at midnight of the day it started. So the
// window a subscription bills for a meter starts at the latest of its period
// start, its trial end, its threshold watermark, and the instant another
// subscription of the customer stopped covering that meter that day. The
// tests below fail when one of those terms is missing from a closer: the
// cycle close, the immediate cancel, the threshold fire, or the preview.
//
// Day D is 2026-04-10 (the tenant zone is UTC). The meter prices 1c per call.

var uwDay = time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)

// dayAt is h:m on day D.
func dayAt(h, m int) time.Time {
	return uwDay.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

// newSub creates a subscription for the rig's customer at `at` on planID,
// billed on the anniversary of its start, and points the rig at it. extra
// adds fields to the request; without trial_days it starts now.
func (r *lagRig) newSub(code, planID string, at time.Time, extra map[string]any) string {
	r.t.Helper()
	body := map[string]any{
		"code": code, "display_name": code, "customer_id": r.custID, "billing_time": "anniversary",
		"items": []map[string]any{{"plan_id": planID, "quantity": 1}},
	}
	for k, v := range extra {
		body[k] = v
	}
	out := r.must(http.MethodPost, "/v1/subscriptions", at, body, http.StatusCreated)
	r.subID = out["id"].(string)
	return r.subID
}

// newPlan creates a monthly plan on the rig's meter.
func (r *lagRig) newPlan(code, timing string, base int64) string {
	r.t.Helper()
	p := r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
		"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": base, "base_bill_timing": timing, "prices": centPrices([]string{r.meterID}),
	}, http.StatusCreated)
	return p["id"].(string)
}

// invoicesOf is invoices() for another subscription of the rig's customer.
func (r *lagRig) invoicesOf(subID string) []lagInvoice {
	r.t.Helper()
	keep := r.subID
	r.subID = subID
	defer func() { r.subID = keep }()
	return r.invoices()
}

// usageOf sums the usage lines of invs.
func usageOf(invs []lagInvoice) int64 {
	var n int64
	for _, i := range invs {
		n += i.sum("usage")
	}
	return n
}

// wantUsageStarts checks that every usage line on inv starts at want.
func wantUsageStarts(t *testing.T, what string, inv lagInvoice, want time.Time) {
	t.Helper()
	seen := false
	for _, l := range inv.lines {
		if l.typ != "usage" {
			continue
		}
		seen = true
		if l.start == nil || !l.start.Equal(want) {
			t.Errorf("%s: usage line (%dc) starts %v, want %v", what, l.amount, l.start, want)
		}
	}
	if !seen {
		t.Errorf("%s: no usage line", what)
	}
}

// uwCtx is a scheduler ctx at the instant at, in live mode (the rig's key is
// a live key).
func uwCtx(at time.Time) context.Context {
	return postgres.WithLivemode(clock.WithEffectiveNow(context.Background(), at), true)
}

// expireTrials runs the trial-expiry scan at `at` and wants one activation.
func (r *lagRig) expireTrials(at time.Time) {
	r.t.Helper()
	r.clk.Set(at)
	n, errs := r.srv.SubscriptionSvc.ProcessExpiredTrials(uwCtx(at), 100)
	if n != 1 || len(errs) != 0 {
		r.t.Fatalf("trial expiry at %v: activated %d, errors %v; want 1 and none", at, n, errs)
	}
	if s := r.sub(); s.Status != domain.SubscriptionActive {
		r.t.Fatalf("status after the trial expiry = %s, want active", s.Status)
	}
}

// scanThresholds runs the spend-threshold scan at `at` and returns how many
// invoices it fired.
func (r *lagRig) scanThresholds(at time.Time) int {
	r.t.Helper()
	r.clk.Set(at)
	n, errs := r.srv.BillingEngine.ScanThresholds(uwCtx(at), 50)
	if len(errs) != 0 {
		r.t.Fatalf("threshold scan at %v: %v", at, errs)
	}
	return n
}

// previewUsage is the usage total of the subscription's preview at `at`.
func (r *lagRig) previewUsage(at time.Time) int64 {
	r.t.Helper()
	return r.previewSum(at, "usage")
}

// previewSum sums the amounts of the default-window preview's lines of one
// line type.
func (r *lagRig) previewSum(at time.Time, lineType string) int64 {
	r.t.Helper()
	out := r.must(http.MethodPost, "/v1/invoices/create_preview", at,
		map[string]any{"customer_id": r.custID, "subscription_id": r.subID}, http.StatusOK)
	lines, _ := out["lines"].([]any)
	var n int64
	for _, raw := range lines {
		l, _ := raw.(map[string]any)
		if l["line_type"] == lineType {
			c, _ := l["amount_cents"].(float64)
			n += int64(c)
		}
	}
	return n
}

// closePeriod runs billing one hour after the subscription's current period
// ends and returns its cycle invoice for the period that started at start.
func (r *lagRig) closePeriod(start time.Time) lagInvoice {
	r.t.Helper()
	pe := r.sub().CurrentBillingPeriodEnd
	if pe == nil {
		r.t.Fatalf("subscription has no period end")
	}
	r.run(pe.Add(time.Hour))
	return r.cycleInvoice(r.invoices(), start)
}

const usageOnceCheck = "usage_billed_once_per_customer_meter_window"

// wantUsageBilledOnce runs the doctor and wants no usage instant of a
// customer's meter on two live invoices.
func wantUsageBilledOnce(t *testing.T) {
	t.Helper()
	found := false
	for _, c := range doctor.Checks {
		if c.Name == usageOnceCheck {
			found = true
		}
	}
	if !found {
		t.Fatalf("the doctor has no %s check", usageOnceCheck)
	}
	res := doctor.Run(context.Background(), testutil.AdminPool(t), doctor.Checks)
	for _, e := range res.Errors {
		t.Errorf("doctor check failed to run: %v", e)
	}
	for _, v := range res.Violations {
		if v.Check.Name == usageOnceCheck {
			t.Errorf("doctor: usage billed twice: %s %s", v.RowID, v.Detail)
		}
	}
}

// --- B2: trial hours are not billed -----------------------------------------

// newTrialEndingAt creates a trialing subscription on plan A whose trial ends
// at trialEnd (created one day before it), and checks that its first paid
// period starts at midnight of day D.
func (r *lagRig) newTrialEndingAt(trialEnd time.Time) {
	r.t.Helper()
	r.newSub("trial", r.planA, trialEnd.Add(-24*time.Hour), map[string]any{"trial_days": 1})
	s := r.sub()
	if s.TrialEndAt == nil || !s.TrialEndAt.Equal(trialEnd) || s.CurrentBillingPeriodStart == nil || !s.CurrentBillingPeriodStart.Equal(uwDay) {
		r.t.Fatalf("trial sub: trial end %v, period start %v; want %v and %v", s.TrialEndAt, s.CurrentBillingPeriodStart, trialEnd, uwDay)
	}
}

// TestUsageWindow_TrialEndsMidDay_CloseBillsUsageFromTrialEnd: the trial ends
// at 18:00 on day D, and the first paid period starts at midnight of D. The
// base fee bills that whole day (ADR-012); the usage before 18:00 is trial
// usage and is not billed. The preview of the trialing subscription, read
// after the trial ended but before the expiry scan flipped it, agrees with
// the invoice.
func TestUsageWindow_TrialEndsMidDay_CloseBillsUsageFromTrialEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	trialEnd := dayAt(18, 0)
	r.newTrialEndingAt(trialEnd)
	r.ingest(dayAt(10, 0), 40) // in the trial
	r.ingest(dayAt(19, 0), 7)  // after it

	if got := r.previewUsage(dayAt(19, 30)); got != 7 {
		t.Errorf("preview usage of the trialing subscription = %dc, want 7c: the 40c before the trial end is free", got)
	}
	r.expireTrials(dayAt(19, 45))

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 7 {
		t.Errorf("first paid invoice usage = %dc, want 7c: the 40c sent during the trial is billed", got)
	}
	wantUsageStarts(t, "first paid invoice", cyc, trialEnd)
	if got := cyc.sum("base_fee"); got != 3000 {
		t.Errorf("first paid invoice base = %dc, want the whole period's 3000c (the trial-end day counts whole, ADR-012)", got)
	}
	wantUsageBilledOnce(t)
}

// TestUsageWindow_TrialEndsMidDay_CancelBillsUsageFromTrialEnd: the same
// trial, canceled at 20:00 the day it ended. The final invoice bills only the
// usage after the trial end.
func TestUsageWindow_TrialEndsMidDay_CancelBillsUsageFromTrialEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	trialEnd := dayAt(18, 0)
	r.newTrialEndingAt(trialEnd)
	r.ingest(dayAt(10, 0), 40)
	r.ingest(dayAt(19, 0), 7)
	r.expireTrials(dayAt(19, 45))
	r.cancelNow(dayAt(20, 0))

	invs := r.invoices()
	if len(invs) != 1 {
		r.logInvoices(invs)
		t.Fatalf("%d invoices after the cancel, want the final one", len(invs))
	}
	if got := invs[0].sum("usage"); got != 7 {
		t.Errorf("final invoice usage = %dc, want 7c: the 40c sent during the trial is billed", got)
	}
	wantUsageStarts(t, "final invoice", invs[0], trialEnd)
}

// TestUsageWindow_EndTrialEarly_BillsUsageFromTheEnd: the operator ends a
// three-day trial at 14:00 on day D. The period restarts at midnight of D and
// the trial end is 14:00, so the usage of the morning is not billed.
func TestUsageWindow_EndTrialEarly_BillsUsageFromTheEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newSub("trial", r.planA, dayAt(18, 0).Add(-24*time.Hour), map[string]any{"trial_days": 3})
	r.ingest(dayAt(10, 0), 40)
	endAt := dayAt(14, 0)
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/end-trial", endAt, nil, http.StatusOK)
	s := r.sub()
	if s.TrialEndAt == nil || !s.TrialEndAt.Equal(endAt) || s.CurrentBillingPeriodStart == nil || !s.CurrentBillingPeriodStart.Equal(uwDay) {
		t.Fatalf("after the early end: trial end %v, period start %v; want %v and %v", s.TrialEndAt, s.CurrentBillingPeriodStart, endAt, uwDay)
	}
	r.ingest(dayAt(15, 0), 7)

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 7 {
		t.Errorf("first paid invoice usage = %dc, want 7c: the 40c sent during the trial is billed", got)
	}
	wantUsageStarts(t, "first paid invoice", cyc, endAt)
}

// TestUsageWindow_TrialEndsAtMidnight_Unchanged is the control: a trial that
// ends at midnight leaves the first paid period's usage window as it was.
func TestUsageWindow_TrialEndsAtMidnight_Unchanged(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newTrialEndingAt(uwDay)
	r.ingest(uwDay.Add(-14*time.Hour), 5) // in the trial, before the period
	r.ingest(dayAt(1, 0), 9)
	r.expireTrials(dayAt(0, 30))

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 9 {
		t.Errorf("first paid invoice usage = %dc, want 9c", got)
	}
	wantUsageStarts(t, "first paid invoice", cyc, uwDay)
}

// TestUsageWindow_ThresholdFireAfterTrial_BillsUsageFromTrialEndOnce: a
// spend threshold fires at 21:00 on the day the trial ended at 18:00. The
// fire bills the usage from the trial end and says so on its line; the close
// bills what came after the fire. The plan is priced at the trial end, where
// the first paid period starts (ADR-130 §4 step 3, amended 2026-10-26). A
// price edit at 18:30, after that, applies from the next period on both
// invoices: neither the fire's watermark nor the trial-end floor moves the
// plan's pin.
func TestUsageWindow_ThresholdFireAfterTrial_BillsUsageFromTrialEndOnce(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	trialEnd := dayAt(18, 0)
	r.newTrialEndingAt(trialEnd)
	plan := r.must(http.MethodGet, "/v1/plans/"+r.planA, dayAt(18, 30), nil, http.StatusOK)
	priceID := flowStr(flowList(plan, "prices")[0], "id")
	r.must(http.MethodPatch, "/v1/plans/"+r.planA+"/prices/"+priceID, dayAt(18, 30), map[string]any{
		"mode": "per_unit", "unit_amount_cents": "2",
	}, http.StatusOK)
	// The database stamps the edit with its own clock; put it at 18:30 of
	// the simulated day, after the paid window opened at the trial end.
	r.exec(`UPDATE plan_price_versions SET valid_from = $1 WHERE price_id = $2 AND valid_from = (SELECT max(valid_from) FROM plan_price_versions WHERE price_id = $2)`, dayAt(18, 30), priceID)
	r.ingest(dayAt(10, 0), 40)
	r.ingest(dayAt(19, 0), 7)
	r.expireTrials(dayAt(19, 45))
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", dayAt(20, 0),
		map[string]any{"amount_gte": 5}, http.StatusOK)
	if n := r.scanThresholds(dayAt(21, 0)); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	r.ingest(uwDay.AddDate(0, 0, 1).Add(9*time.Hour), 3)

	invs := r.invoices()
	var fire []lagInvoice
	for _, i := range invs {
		if i.reason == string(domain.BillingReasonThreshold) {
			fire = append(fire, i)
		}
	}
	if len(fire) != 1 {
		r.logInvoices(invs)
		t.Fatalf("%d threshold invoices, want 1", len(fire))
	}
	if got := fire[0].sum("usage"); got != 7 {
		t.Errorf("threshold invoice usage = %dc, want 7c (7 calls at the trial end's price, after the trial end)", got)
	}
	wantUsageStarts(t, "threshold invoice", fire[0], trialEnd)

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 3 {
		t.Errorf("cycle close usage = %dc, want 3c: only the usage after the fire", got)
	}
	if got := usageOf(r.invoices()); got != 10 {
		t.Errorf("usage over all live invoices = %dc, want 10c", got)
	}
	wantUsageBilledOnce(t)
}

// TestUsageWindow_PriceEditDuringTrial_FirstPaidPeriodAtTheEdit: the trial
// ends at 18:00 on day D, and the plan's price was edited at 12:00, during
// the trial. The plan is priced at the trial end, where the first paid
// period starts (ADR-130 §4 step 3, amended 2026-10-26), so the preview and
// the first paid invoice bill the edited price.
//
// Mutation: pin every plan at the period start, and both bill the old
// price.
func TestUsageWindow_PriceEditDuringTrial_FirstPaidPeriodAtTheEdit(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newTrialEndingAt(dayAt(18, 0))
	plan := r.must(http.MethodGet, "/v1/plans/"+r.planA, dayAt(12, 0), nil, http.StatusOK)
	priceID := flowStr(flowList(plan, "prices")[0], "id")
	edit := r.must(http.MethodPatch, "/v1/plans/"+r.planA+"/prices/"+priceID, dayAt(12, 0), map[string]any{
		"mode": "per_unit", "unit_amount_cents": "2",
	}, http.StatusOK)
	if flowStr(edit, "effective") != "next_period" {
		t.Fatalf("edit during the trial: effective %q, want next_period (the plan is used)", flowStr(edit, "effective"))
	}
	// The database stamps the edit with its own clock; put it at 12:00 of
	// the simulated day, during the trial.
	r.exec(`UPDATE plan_price_versions SET valid_from = $1 WHERE price_id = $2 AND valid_from = (SELECT max(valid_from) FROM plan_price_versions WHERE price_id = $2)`, dayAt(12, 0), priceID)
	r.ingest(dayAt(19, 0), 7)

	if got := r.previewUsage(dayAt(19, 30)); got != 14 {
		t.Errorf("preview usage = %dc, want 14c: 7 calls at the price edited during the trial", got)
	}
	r.expireTrials(dayAt(19, 45))
	if got := r.closePeriod(uwDay).sum("usage"); got != 14 {
		t.Errorf("first paid invoice usage = %dc, want 14c: 7 calls at the price edited during the trial", got)
	}
}

// TestUsageWindow_ItemThresholdAfterTrial_CountsUsageFromTrialEnd: an item
// threshold counts the item's usage from the trial end, the window the
// invoice bills, not the trial usage before it.
func TestUsageWindow_ItemThresholdAfterTrial_CountsUsageFromTrialEnd(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.newTrialEndingAt(dayAt(18, 0))
	r.ingest(dayAt(10, 0), 40)
	r.ingest(dayAt(19, 0), 7)
	r.expireTrials(dayAt(19, 45))
	item := r.sub().Items[0].ID
	setItemThreshold := func(at time.Time, gte string) {
		r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", at, map[string]any{
			"item_thresholds": []map[string]any{{"subscription_item_id": item, "usage_gte": gte}},
		}, http.StatusOK)
	}
	setItemThreshold(dayAt(20, 0), "45")
	if n := r.scanThresholds(dayAt(21, 0)); n != 0 {
		t.Errorf("an item threshold of 45 calls fired with 7 calls after the trial end (it counted the 40 trial calls)")
	}
	setItemThreshold(dayAt(21, 30), "7")
	if n := r.scanThresholds(dayAt(22, 0)); n != 1 {
		t.Errorf("an item threshold of 7 calls fired %d invoices with 7 calls after the trial end, want 1", n)
	}
}

// --- B4: a same-day cancel and recreate bills the day's usage once ----------

// TestUsageWindow_CancelRecreateSameDay_BillsUsageOnce: subscription A (in
// advance) starts at 08:00 on day D and is canceled at 10:00; its final
// invoice bills the usage to 10:00. Subscription B (in arrears) starts on the
// same meter at 11:00, so its period starts at midnight. B bills its base for
// the whole day (ADR-012) but its usage only from 10:00, where A's ended.
func TestUsageWindow_CancelRecreateSameDay_BillsUsageOnce(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_advance", 0)
	arrears := r.newPlan("plan_arrears", "in_arrears", 2000)
	subA := r.newSub("sub-a", r.planA, dayAt(8, 0), nil)
	r.ingest(dayAt(9, 0), 100)
	r.cancelNow(dayAt(10, 0))
	if got := usageOf(r.invoicesOf(subA)); got != 100 {
		t.Errorf("A's usage = %dc, want 100c (to its cancel)", got)
	}
	r.ingest(dayAt(10, 30), 50)
	subB := r.newSub("sub-b", arrears, dayAt(11, 0), nil)
	r.ingest(dayAt(12, 0), 20)

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 70 {
		t.Errorf("B's usage = %dc, want 70c: the 100c before A's cancel is already on A's final invoice", got)
	}
	wantUsageStarts(t, "B's cycle invoice", cyc, dayAt(10, 0))
	if got := cyc.sum("base_fee"); got != 2000 {
		t.Errorf("B's base = %dc, want the whole period's 2000c (the start day counts whole, ADR-012)", got)
	}
	if got := usageOf(r.invoicesOf(subA)) + usageOf(r.invoicesOf(subB)); got != 170 {
		t.Errorf("usage over the customer's live invoices = %dc, want the 170c sent", got)
	}
	wantUsageBilledOnce(t)
}

// TestUsageWindow_TrialCanceledRecreatedSameDay_TrialUsageNotBilled: A is a
// trial from 08:00, canceled at 10:00 (no invoice: trials are free). B starts
// on the same meter at 11:00. The trial's usage is not B's.
func TestUsageWindow_TrialCanceledRecreatedSameDay_TrialUsageNotBilled(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	subA := r.newSub("sub-a", r.planA, dayAt(8, 0), map[string]any{"trial_days": 1})
	r.ingest(dayAt(9, 0), 100)
	r.cancelNow(dayAt(10, 0))
	if invs := r.invoicesOf(subA); len(invs) != 0 {
		t.Fatalf("the canceled trial has %d invoices, want none", len(invs))
	}
	r.newSub("sub-b", r.planB, dayAt(11, 0), nil)
	r.ingest(dayAt(12, 0), 20)

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 20 {
		t.Errorf("B's usage = %dc, want 20c: the 100c was sent during A's trial", got)
	}
	wantUsageStarts(t, "B's cycle invoice", cyc, dayAt(10, 0))
}

// TestUsageWindow_NoPredecessor_StartDayUsageBilled is a control: with no
// other subscription on the meter, B started at 11:00 bills the usage of its
// whole first day, including 09:00 (ADR-012: the start day counts whole).
func TestUsageWindow_NoPredecessor_StartDayUsageBilled(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	r.ingest(dayAt(9, 0), 30)
	r.newSub("sub-b", r.planB, dayAt(11, 0), nil)
	r.ingest(dayAt(12, 0), 20)

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 50 {
		t.Errorf("B's usage = %dc, want 50c: the start day's usage before the start is B's", got)
	}
	wantUsageStarts(t, "B's cycle invoice", cyc, uwDay)
}

// TestUsageWindow_PredecessorCanceledDayBefore_StartDayUsageBilled is a
// control: A was canceled the day before, so nothing of day D is A's.
func TestUsageWindow_PredecessorCanceledDayBefore_StartDayUsageBilled(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	dayBefore := uwDay.AddDate(0, 0, -1)
	subA := r.newSub("sub-a", r.planA, dayBefore.Add(8*time.Hour), nil)
	r.ingest(dayBefore.Add(9*time.Hour), 100)
	r.cancelNow(dayBefore.Add(10 * time.Hour))
	r.ingest(dayAt(9, 0), 30)
	subB := r.newSub("sub-b", r.planB, dayAt(11, 0), nil)
	r.ingest(dayAt(12, 0), 20)

	cyc := r.closePeriod(uwDay)
	if got := cyc.sum("usage"); got != 50 {
		t.Errorf("B's usage = %dc, want 50c", got)
	}
	wantUsageStarts(t, "B's cycle invoice", cyc, uwDay)
	if got := usageOf(r.invoicesOf(subA)) + usageOf(r.invoicesOf(subB)); got != 150 {
		t.Errorf("usage over the customer's live invoices = %dc, want 150c", got)
	}
	wantUsageBilledOnce(t)
}

// TestUsageWindow_ItemRemovedThenMeterOnNewSub_EachBillsItsOwnPart: A (live
// since Apr 1, with an add-on) removes its metered item at 10:00 on day D. B
// takes the meter at 11:00 and is canceled at 12:00. A keeps everything to
// 10:00 (a later subscription never moves an earlier one's window), and B
// bills from 10:00 to its cancel.
func TestUsageWindow_ItemRemovedThenMeterOnNewSub_EachBillsItsOwnPart(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_arrears", true)
	subA := r.subID
	r.ingest(time.Date(2026, 4, 5, 9, 0, 0, 0, time.UTC), 100)
	r.ingest(dayAt(9, 0), 30)
	if code, raw := r.do(http.MethodDelete, "/v1/subscriptions/"+subA+"/items/"+r.itemA, dayAt(10, 0), nil); code != http.StatusNoContent {
		t.Fatalf("remove A's metered item: %d %s, want 204", code, raw)
	}
	r.ingest(dayAt(10, 30), 40)
	subB := r.newSub("sub-b", r.planB, dayAt(11, 0), nil)
	r.ingest(dayAt(11, 30), 5)
	r.cancelNow(dayAt(12, 0))
	r.ingest(dayAt(13, 0), 7) // nobody's

	finalB := r.invoicesOf(subB)
	if got := usageOf(finalB); got != 45 {
		t.Errorf("B's final usage = %dc, want 45c: from A's removal at 10:00 to B's cancel", got)
	}
	if len(finalB) == 1 {
		wantUsageStarts(t, "B's final invoice", finalB[0], dayAt(10, 0))
	}

	r.subID = subA
	r.run(lagB.Add(time.Hour))
	cycA := r.cycleInvoice(r.invoices(), lagP0)
	if got := cycA.sum("usage"); got != 130 {
		t.Errorf("A's usage = %dc, want 130c: everything to the removal at 10:00", got)
	}
	if got := usageOf(r.invoicesOf(subA)) + usageOf(finalB); got != 175 {
		t.Errorf("usage over the customer's live invoices = %dc, want 175c", got)
	}
	wantUsageBilledOnce(t)
}
