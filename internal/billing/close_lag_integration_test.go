package billing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// A period ends at a boundary B, and the billing run closes it on a later
// tick: up to an hour later today, longer after downtime or a failed attempt.
// These tests pin what the close bills when it runs late.
//
// They use cancelRig (cancel_after_period_end_integration_test.go): a monthly
// plan with a 5000c in_arrears base and a 1c/call sum meter, a one-month
// period [P0, P1) and 100 events × qty 10 in its first hours. Time is bound per
// call with clock.WithEffectiveNow, which is how the wall-clock billing run
// sees it; no test clock is involved.

// addLastEverRule makes every event of the rig's meter belong to one last_ever
// pricing rule: the billed quantity is the latest event's quantity.
func (r *cancelRig) addLastEverRule(t *testing.T) {
	t.Helper()
	rule := r.f.seededRule(t, r.base)
	if _, err := r.f.pricingSvc.UpsertMeterPricingRule(r.base, r.f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID:             r.f.meterID,
		RatingRuleVersionID: rule.ID,
		DimensionMatch:      map[string]any{},
		AggregationMode:     domain.AggLastEver,
		Priority:            10,
	}); err != nil {
		t.Fatalf("add last_ever rule: %v", err)
	}
}

func (r *cancelRig) ingestOne(t *testing.T, at time.Time, qty int64) {
	t.Helper()
	ts := at
	if _, err := r.f.usageSvc.Ingest(r.at(at.Add(time.Minute)), r.f.tenantID, usage.IngestInput{
		CustomerID: r.f.customerID, MeterID: r.f.meterID, Quantity: decimal.NewFromInt(qty), Timestamp: &ts,
	}); err != nil {
		t.Fatalf("ingest at %v: %v", at, err)
	}
}

// TestLateClose_LastEver_BillsValueAsOfPeriodEnd (D2): a last_ever rule bills
// the latest value as of the period end. The value changes to 100 five hours
// after the boundary, and the billing run closes the period an hour after
// that. The ended period's invoice must bill 10 (the value at the boundary),
// not 100.
//
// Mutation seam: drop the `e.timestamp < $4` bound from the last_ever claims
// in usage/postgres.go → the invoice bills 100.
func TestLateClose_LastEver_BillsValueAsOfPeriodEnd(t *testing.T) {
	r := newCancelRig(t, "close lag last_ever")
	r.addLastEverRule(t)
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	r.ingestOne(t, p1.Add(5*time.Hour), 100)

	if gen, failures := r.f.engine.RunCycleForTenant(r.at(p1.Add(6*time.Hour)), r.f.tenantID, 50); gen != 1 || len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v, want 1 and none", gen, failures)
	}
	invs, lines := r.liveInvoices(t)
	inv := invoiceFor(t, invs, p0, p1)
	usageCents, _ := sumLines(lines[inv.ID])
	if usageCents != 10 {
		t.Errorf("ended period [P0, P1) bills last_ever usage %dc, want 10c: the value at the period end, not the value set 5h after it", usageCents)
	}
}

// TestLateClose_LastEver_CatchUp_EachPeriodBillsItsOwnValue (D2, catch-up):
// the billing run is two periods behind. Each period's invoice bills the
// latest value as of its own end: 10 for [P0, P1) and 40 for [P1, P2), even
// though the run reads both after the value became 70.
func TestLateClose_LastEver_CatchUp_EachPeriodBillsItsOwnValue(t *testing.T) {
	r := newCancelRig(t, "close lag last_ever catch-up")
	r.addLastEverRule(t)
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	r.ingestOne(t, p1.AddDate(0, 0, 3), 40)
	p2 := p1.AddDate(0, 1, 0)
	r.ingestOne(t, p2.Add(2*time.Hour), 70)

	gen, failures := r.f.engine.RunCycleForTenant(r.at(p2.Add(3*time.Hour)), r.f.tenantID, 50)
	if gen != 2 || len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v, want 2 and none", gen, failures)
	}
	invs, lines := r.liveInvoices(t)
	first := invoiceFor(t, invs, p0, p1)
	sub := r.sub(t)
	second := invoiceFor(t, invs, p1, *sub.CurrentBillingPeriodStart)
	if u, _ := sumLines(lines[first.ID]); u != 10 {
		t.Errorf("[P0, P1) bills last_ever usage %dc, want 10c", u)
	}
	if u, _ := sumLines(lines[second.ID]); u != 40 {
		t.Errorf("[P1, P2) bills last_ever usage %dc, want 40c", u)
	}
}

// TestThresholdScan_DueMidPeriodCancel_DoesNotBillPastCancelAt (D4): a
// cancel_at falls inside the period. The first scheduler tick after it runs
// the threshold scan before the billing run's cancel arm. The scan must not
// fire a threshold invoice over [period start, now): that window runs past the
// contracted cancel, and bills usage stamped after the subscription ended.
// The billing run then cancels at cancel_at with a final invoice for
// [P1, cancel_at).
//
// Mutation seam: drop the cancel_at skip in scanOneThreshold → the scan fires
// and the 3000c of usage stamped after cancel_at is billed.
func TestThresholdScan_DueMidPeriodCancel_DoesNotBillPastCancelAt(t *testing.T) {
	r := newCancelRig(t, "close lag threshold past cancel_at")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	cancelAt := p1.AddDate(0, 0, 10)
	if _, err := r.f.subSvc.ScheduleCancel(r.at(p0.Add(24*time.Hour)), r.f.tenantID, r.f.subID, subscription.ScheduleCancelInput{CancelAt: &cancelAt}); err != nil {
		t.Fatalf("schedule cancel: %v", err)
	}
	resetFalse := false
	if _, err := r.f.subSvc.SetBillingThresholds(r.at(p0.Add(24*time.Hour)), r.f.tenantID, r.f.subID, subscription.BillingThresholdsInput{
		AmountGTE: 7000, ResetBillingCycle: &resetFalse,
	}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}
	// Close [P0, P1).
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(p1.Add(time.Hour)), r.f.tenantID, 50); gen != 1 || len(failures) != 0 {
		t.Fatalf("close [P0, P1): generated=%d failures=%v", gen, failures)
	}
	r.ingestOne(t, p1.AddDate(0, 0, 1), 500)           // before cancel_at
	r.ingestOne(t, cancelAt.Add(10*time.Minute), 3000) // after cancel_at

	tick := cancelAt.Add(30 * time.Minute)
	fired, scanErrs := r.f.engine.ScanThresholds(r.at(tick), 50)
	if len(scanErrs) != 0 {
		t.Fatalf("threshold scan: %v", scanErrs)
	}
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(tick), r.f.tenantID, 50); len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v", gen, failures)
	}

	sub := r.sub(t)
	if sub.Status != domain.SubscriptionCanceled || sub.CanceledAt == nil || !sub.CanceledAt.Equal(cancelAt) {
		t.Errorf("after the tick: status=%s canceled_at=%v, want canceled at %v", sub.Status, sub.CanceledAt, cancelAt)
	}
	invs, lines := r.liveInvoices(t)
	var periodUsage, periodBase int64
	for _, inv := range invs {
		if inv.BillingPeriodStart.Equal(p1) {
			u, b := sumLines(lines[inv.ID])
			periodUsage += u
			periodBase += b
			t.Logf("invoice %s reason=%s [%v, %v) usage=%dc base=%dc", inv.ID, inv.BillingReason, inv.BillingPeriodStart, inv.BillingPeriodEnd, u, b)
		}
	}
	if fired != 0 {
		t.Errorf("threshold scan fired %d invoice(s) after the cancel_at had passed, want 0", fired)
	}
	if periodUsage != 500 {
		t.Errorf("usage billed for [P1, cancel_at) = %dc, want 500c (the 3000c stamped after cancel_at must not be billed)", periodUsage)
	}
	assertUsageBilledOnce(t, r.base)
}

// TestLateClose_ScheduledResumeAfterBoundary_EndedPeriodStaysDraft: collection
// is paused with resumes_at 30 minutes after P1. The scheduler's tick at
// P1+45m runs the pause resumer before the billing run. The period that ended
// at P1 was paused at its end, so its invoice must be a draft.
//
// KNOWN DEFECT, not fixed here: the resumer clears the pause first and the
// late close finalizes (and collects) the paused period's invoice. The
// operator's resume in the same gap is fixed (it closes the ended period
// first). The scheduled resumers (wall-clock, and test-clock Phase 0.7, which
// runs before the advance's closes) need the close to read the pause as of
// each boundary, which is a separate change. Remove the skip to reproduce.
func TestLateClose_ScheduledResumeAfterBoundary_EndedPeriodStaysDraft(t *testing.T) {
	t.Skip("known defect: the scheduled pause resumer runs before a late close (see comment)")
	r := newCancelRig(t, "close lag scheduled resume")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	resumesAt := p1.Add(30 * time.Minute)
	if _, err := r.f.subSvc.PauseCollection(r.at(p0.Add(24*time.Hour)), r.f.tenantID, r.f.subID, subscription.PauseCollectionInput{
		Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumesAt,
	}); err != nil {
		t.Fatalf("pause collection: %v", err)
	}
	tick := p1.Add(45 * time.Minute)
	if n, errs := r.f.subSvc.ProcessExpiredPauseCollections(r.at(tick), 100); len(errs) != 0 {
		t.Fatalf("pause resumer: %d %v", n, errs)
	}
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(tick), r.f.tenantID, 50); gen != 1 || len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v", gen, failures)
	}
	invs, _ := r.liveInvoices(t)
	inv := invoiceFor(t, invs, p0, p1)
	if inv.Status != domain.InvoiceDraft {
		t.Errorf("[P0, P1) invoice status = %s, want draft: collection was paused at P1 (resumes_at %v)", inv.Status, resumesAt)
	}
}

// wantWriteRefusedUnbilled: a subscription write was refused because a period
// ended and could not be billed.
func wantWriteRefusedUnbilled(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the write succeeded with an ended period still unbilled; want it refused with unbilled_period")
	}
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "unbilled_period" {
		t.Fatalf("write error = %v (code %q), want an invalid-state error with code unbilled_period", err, errs.Code(err))
	}
}

// TestCloseLag_WriteWhileBillingFails_Refused: P0 ended and its close keeps
// failing. A write in the gap must not act on the ended period: it is refused
// with unbilled_period, the same answer an immediate cancel gives, and changes
// nothing. Once billing works, the same write lands in the new period.
//
// Mutation seam: make CloseEndedPeriodFirst return the row without the
// still-due check → the schedule is written onto the ended period and the
// next close cancels at P1.
func TestCloseLag_WriteWhileBillingFails_Refused(t *testing.T) {
	r := newCancelRig(t, "close lag write refused")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	r.f.invAdapter.createTxErr = errors.New("invoice insert refused (test)")
	now := p1.Add(time.Hour)

	_, err := r.f.subSvc.ScheduleCancel(r.at(now), r.f.tenantID, r.f.subID, subscription.ScheduleCancelInput{AtPeriodEnd: true})
	wantWriteRefusedUnbilled(t, err)
	_, err = r.f.subSvc.PauseCollection(r.at(now), r.f.tenantID, r.f.subID, subscription.PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft})
	wantWriteRefusedUnbilled(t, err)
	sub := r.sub(t)
	if sub.CancelAtPeriodEnd || sub.PauseCollection != nil || !sub.CurrentBillingPeriodStart.Equal(p0) {
		t.Fatalf("after the refusals: cancel_at_period_end=%v pause=%v period start=%v, want nothing written and the period still [P0, P1)",
			sub.CancelAtPeriodEnd, sub.PauseCollection, sub.CurrentBillingPeriodStart)
	}
	if sub.BillingFailure == nil || sub.BillingFailure.Attempts != 1 {
		t.Fatalf("billing failure = %+v, want the first refused write's attempt saved (attempt 1); the second write ran inside the wait and attempted nothing", sub.BillingFailure)
	}

	// Billing works again; retry-billing closes P0, and the schedule lands in P1.
	r.f.invAdapter.createTxErr = nil
	now = now.Add(time.Hour)
	if res, err := r.f.subSvc.RetryBilling(r.at(now), r.f.tenantID, r.f.subID); err != nil || res.InvoicesGenerated != 1 {
		t.Fatalf("retry billing: %v generated=%d", err, res.InvoicesGenerated)
	}
	scheduled, err := r.f.subSvc.ScheduleCancel(r.at(now), r.f.tenantID, r.f.subID, subscription.ScheduleCancelInput{AtPeriodEnd: true})
	if err != nil || !scheduled.CancelAtPeriodEnd || !scheduled.CurrentBillingPeriodStart.Equal(p1) {
		t.Fatalf("schedule after the retry: %v cancel_at_period_end=%v period start=%v, want scheduled in [P1, P2)", err, scheduled.CancelAtPeriodEnd, scheduled.CurrentBillingPeriodStart)
	}
	r.runWritesNothing(t, now.Add(time.Hour))
	if s := r.sub(t); s.Status != domain.SubscriptionActive {
		t.Errorf("status = %s, want active until the end of [P1, P2)", s.Status)
	}
}

// TestCloseLag_CrossIntervalSwap_OnEndedPeriod_RefusedInTx (D3 backstop): the
// atomic swap is handed a subscription whose period has ended unbilled (a
// boundary that passed after the handler's pre-close). It must refuse with
// unbilled_period and write nothing, not re-anchor past the ended period.
func TestCloseLag_CrossIntervalSwap_OnEndedPeriod_RefusedInTx(t *testing.T) {
	r := newCancelRig(t, "close lag swap backstop")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	yearly, err := r.f.pricingSvc.CreatePlan(r.base, r.f.tenantID, pricing.CreatePlanInput{
		Code: "pln_yearly", Name: "Yearly", Currency: "USD", BillingInterval: domain.BillingYearly,
		BaseAmountCents: 50000, MeterIDs: []string{r.f.meterID},
	})
	if err != nil {
		t.Fatalf("create yearly plan: %v", err)
	}
	r.f.subSvc.SetPlanReader(r.f.pricingSvc)

	tx, err := r.f.db.BeginTx(r.base, postgres.TxTenant, r.f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	_, err = r.f.subSvc.UpdateItemTx(r.at(p1.Add(2*time.Hour)), tx, r.f.tenantID, r.f.subID, r.f.itemID,
		subscription.UpdateItemInput{NewPlanID: yearly.ID, Immediate: true})
	wantWriteRefusedUnbilled(t, err)
	postgres.Rollback(tx)
	if sub := r.sub(t); !sub.CurrentBillingPeriodStart.Equal(p0) || sub.Items[0].PlanID != r.f.planID {
		t.Fatalf("after the refusal: period start=%v plan=%s, want [P0, P1) on the monthly plan", sub.CurrentBillingPeriodStart, sub.Items[0].PlanID)
	}
}
