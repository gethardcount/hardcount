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
	"github.com/sagarsuperuser/velox/internal/testclock"
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

// pauseUntil pauses the rig's subscription's collection a day into [P0, P1),
// until resumesAt (nil: until an operator resumes it).
func (r *cancelRig) pauseUntil(t *testing.T, resumesAt *time.Time) {
	t.Helper()
	if _, err := r.f.subSvc.PauseCollection(r.at(r.f.cycleStart.Add(24*time.Hour)), r.f.tenantID, r.f.subID, subscription.PauseCollectionInput{
		Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: resumesAt,
	}); err != nil {
		t.Fatalf("pause collection: %v", err)
	}
}

// resumeExpired runs the wall-clock pause resumer at the given instant.
func (r *cancelRig) resumeExpired(t *testing.T, at time.Time) {
	t.Helper()
	if _, errs := r.f.subSvc.ProcessExpiredPauseCollections(r.at(at), 100); len(errs) != 0 {
		t.Fatalf("pause resumer: %v", errs)
	}
}

func wantStatus(t *testing.T, inv domain.Invoice, want domain.InvoiceStatus, why string) {
	t.Helper()
	if inv.Status != want {
		t.Errorf("[%v, %v) invoice status = %s, want %s: %s", inv.BillingPeriodStart, inv.BillingPeriodEnd, inv.Status, want, why)
	}
}

// TestLateClose_ScheduledResumeAfterBoundary_EndedPeriodStaysDraft: collection
// is paused with resumes_at 30 minutes after P1, and the billing run closes
// [P0, P1) late, at P1+45m. The period was paused at its end, so its invoice
// must be a draft. The resumer runs first here, the order the tick used to
// have and the state a failed or unfinished run leaves: it must leave the
// pause on the row while that close is owed. Once the period is closed, the
// next pass clears the pause.
//
// Mutation seams: drop resumeNotWaitingOnCloseSQL from
// ListExpiredPauseCollections → the resumer clears the pause and the close
// finalizes; make InvoiceFinalizationStatus read `pause != nil` → the catch-up
// test below drafts the period that ended after resumes_at.
func TestLateClose_ScheduledResumeAfterBoundary_EndedPeriodStaysDraft(t *testing.T) {
	r := newCancelRig(t, "close lag scheduled resume")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	resumesAt := p1.Add(30 * time.Minute)
	r.pauseUntil(t, &resumesAt)

	tick := p1.Add(45 * time.Minute)
	r.resumeExpired(t, tick)
	if sub := r.sub(t); sub.PauseCollection == nil {
		t.Fatal("the resumer cleared the pause while [P0, P1), which ended before resumes_at, was still unclosed")
	}
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(tick), r.f.tenantID, 50); gen != 1 || len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v", gen, failures)
	}
	invs, _ := r.liveInvoices(t)
	wantStatus(t, invoiceFor(t, invs, p0, p1), domain.InvoiceDraft, "collection was paused at P1 (resumes_at P1+30m)")

	r.resumeExpired(t, tick)
	if sub := r.sub(t); sub.PauseCollection != nil {
		t.Errorf("after the close, pause = %+v, want cleared: no period ending before resumes_at is left to close", sub.PauseCollection)
	}
}

// TestLateClose_ScheduledResume_CatchUp_EachPeriodReadsPauseAtItsEnd: the
// billing run is two periods behind and resumes_at falls between the two
// boundaries. [P0, P1) ended while paused and is a draft; [P1, P2) ended after
// the resume and is finalized, although both close in one run with the pause
// still on the row.
func TestLateClose_ScheduledResume_CatchUp_EachPeriodReadsPauseAtItsEnd(t *testing.T) {
	r := newCancelRig(t, "close lag scheduled resume catch-up")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	resumesAt := p1.AddDate(0, 0, 3)
	r.pauseUntil(t, &resumesAt)

	tick := p1.AddDate(0, 1, 0).Add(45 * time.Minute)
	r.resumeExpired(t, tick)
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(tick), r.f.tenantID, 50); gen != 2 || len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v, want 2 and none", gen, failures)
	}
	invs, _ := r.liveInvoices(t)
	sub := r.sub(t)
	wantStatus(t, invoiceFor(t, invs, p0, p1), domain.InvoiceDraft, "collection was paused at P1")
	wantStatus(t, invoiceFor(t, invs, p1, *sub.CurrentBillingPeriodStart), domain.InvoiceFinalized, "collection resumed at P1+3d, before P2")

	r.resumeExpired(t, tick)
	if sub := r.sub(t); sub.PauseCollection != nil {
		t.Errorf("after the catch-up, pause = %+v, want cleared", sub.PauseCollection)
	}
}

// TestLateClose_ScheduledResumeAfterCancelAt_FinalInvoiceStaysDraft: the same
// window at a mid-period cancel_at. Collection resumes 30 minutes after
// cancel_at, and the run that fires the cancel comes 45 minutes after it. The
// final invoice bills [P1, cancel_at), which ended while paused: a draft.
//
// Mutation seams: drop the cancel_at arm of resumeNotWaitingOnCloseSQL → the
// resumer clears the pause first and the final invoice is finalized; pass the
// cancel's `now` instead of canceledAt to InvoiceFinalizationStatus → the same.
func TestLateClose_ScheduledResumeAfterCancelAt_FinalInvoiceStaysDraft(t *testing.T) {
	r := newCancelRig(t, "close lag scheduled resume at cancel_at")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	cancelAt := p1.AddDate(0, 0, 10)
	if _, err := r.f.subSvc.ScheduleCancel(r.at(p0.Add(24*time.Hour)), r.f.tenantID, r.f.subID, subscription.ScheduleCancelInput{CancelAt: &cancelAt}); err != nil {
		t.Fatalf("schedule cancel: %v", err)
	}
	resumesAt := cancelAt.Add(30 * time.Minute)
	r.pauseUntil(t, &resumesAt)
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(p1.Add(time.Hour)), r.f.tenantID, 50); gen != 1 || len(failures) != 0 {
		t.Fatalf("close [P0, P1): generated=%d failures=%v", gen, failures)
	}
	r.ingestOne(t, p1.AddDate(0, 0, 1), 500)

	tick := cancelAt.Add(45 * time.Minute)
	r.resumeExpired(t, tick)
	if _, failures := r.f.engine.RunCycleForTenant(r.at(tick), r.f.tenantID, 50); len(failures) != 0 {
		t.Fatalf("billing run: failures=%v", failures)
	}
	sub := r.sub(t)
	if sub.Status != domain.SubscriptionCanceled || sub.CanceledAt == nil || !sub.CanceledAt.Equal(cancelAt) {
		t.Fatalf("status=%s canceled_at=%v, want canceled at %v", sub.Status, sub.CanceledAt, cancelAt)
	}
	invs, _ := r.liveInvoices(t)
	wantStatus(t, invoiceFor(t, invs, p1, cancelAt), domain.InvoiceDraft, "collection was paused at cancel_at (resumes_at cancel_at+30m)")
}

// TestLateClose_PauseWithoutResumesAt_CatchUp_AllDraft (control): a pause with
// no resumes_at holds every period, and no scheduled resume clears it.
func TestLateClose_PauseWithoutResumesAt_CatchUp_AllDraft(t *testing.T) {
	r := newCancelRig(t, "close lag indefinite pause")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	r.pauseUntil(t, nil)

	tick := p1.AddDate(0, 1, 0).Add(45 * time.Minute)
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(tick), r.f.tenantID, 50); gen != 2 || len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v, want 2 and none", gen, failures)
	}
	r.resumeExpired(t, tick)
	invs, _ := r.liveInvoices(t)
	sub := r.sub(t)
	wantStatus(t, invoiceFor(t, invs, p0, p1), domain.InvoiceDraft, "collection is paused with no resume date")
	wantStatus(t, invoiceFor(t, invs, p1, *sub.CurrentBillingPeriodStart), domain.InvoiceDraft, "collection is paused with no resume date")
	if sub.PauseCollection == nil {
		t.Error("the scheduled resumer cleared a pause that has no resumes_at")
	}
}

// TestLateClose_ScheduledResume_TestClock_AdvancePastResume: the same window
// on a test clock. One advance lands at P1+45m, past a resumes_at of P1+30m;
// the catchup bills [P0, P1) as a draft, and the pause is cleared by the end
// of the same advance (not left for the next one).
//
// Mutation seams: drop resumeNotWaitingOnCloseSQL from
// ListExpiredPauseCollectionsForClock → the early resumer clears the pause;
// run the catchup's resume phase before Phase 1 → the store skip still holds
// the pause, so [P0, P1) stays a draft, but the pause is left on the row after
// the advance (not cleared in the same advance).
func TestLateClose_ScheduledResume_TestClock_AdvancePastResume(t *testing.T) {
	r := newCancelRig(t, "close lag scheduled resume on a test clock")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	resumesAt := p1.Add(30 * time.Minute)
	r.pauseUntil(t, &resumesAt)

	frozen := p1.Add(45 * time.Minute)
	clockID := postgres.NewID("vlx_tclk")
	execTx(t, r.f.db, r.base, r.f.tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, 'resume lag clock', $3, 'advancing')`,
		clockID, r.f.tenantID, frozen)
	r.exec(t, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, clockID, r.f.customerID)
	r.exec(t, `UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, clockID, r.f.subID)
	clocks := testclock.NewPostgresStore(r.f.db)
	r.f.engine.SetTestClockReader(clocks)
	r.f.subSvc.SetResolver(r.f.engine)

	// A failed Phase 1 leaves the resume phase to run with [P0, P1) still
	// unclosed: it must keep the pause.
	if _, errs := r.f.subSvc.ProcessExpiredPauseCollectionsForClock(r.base, r.f.tenantID, clockID, frozen); len(errs) != 0 {
		t.Fatalf("clock pause resumer: %v", errs)
	}
	if sub := r.sub(t); sub.PauseCollection == nil {
		t.Fatal("the clock resumer cleared the pause while [P0, P1), which ended before resumes_at, was still unclosed")
	}

	orchestrator := testclock.NewService(clocks)
	orchestrator.SetBillingRunner(r.f.engine)
	orchestrator.SetPauseResumer(r.f.subSvc)
	if err := orchestrator.RunCatchup(r.base, testclock.CatchupJob{TenantID: r.f.tenantID, ClockID: clockID, PrevFrozenTime: p0}); err != nil {
		t.Fatalf("catchup: %v", err)
	}
	invs, _ := r.liveInvoices(t)
	wantStatus(t, invoiceFor(t, invs, p0, p1), domain.InvoiceDraft, "collection was paused at P1 (resumes_at P1+30m)")
	if sub := r.sub(t); sub.PauseCollection != nil {
		t.Errorf("after the advance, pause = %+v, want cleared in the same advance", sub.PauseCollection)
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

// trialFlipRig turns the rig's plan in_advance and its subscription into a
// trial ending at trialEnd, paused a day into the trial until resumesAt. The
// post-trial period is [trialEnd, trialEnd+1 month).
func (r *cancelRig) trialFlipRig(t *testing.T, trialEnd, resumesAt time.Time) {
	t.Helper()
	r.exec(t, `UPDATE plans SET base_bill_timing = 'in_advance' WHERE id = $1`, r.f.planID)
	// A trialing item has no billable interval yet; the flip opens it.
	r.exec(t, `DELETE FROM billing_intervals WHERE subscription_item_id = $1`, r.f.itemID)
	r.exec(t, `UPDATE subscriptions SET status = 'trialing', trial_start_at = $1, trial_end_at = $2, activated_at = NULL,
		current_billing_period_start = $2, current_billing_period_end = $3, next_billing_at = $3 WHERE id = $4`,
		r.f.cycleStart, trialEnd, trialEnd.AddDate(0, 1, 0), r.f.subID)
	r.pauseUntil(t, &resumesAt)
}

// TestTrialFlipAfterResume_FirstInvoiceReadsPauseAtTrialEnd: an in_advance
// subscription's trial ends at T while collection is paused until T+30m, and
// the trial flip runs at T+45m. The day-1 invoice bills the period that began
// at T, while collection was paused: a draft.
//
// Mutation seam: pass the flip's clock now (or `now` in buildOnCreateInvoice)
// instead of trialEndAt to BillOnCreateTx → the invoice is finalized.
func TestTrialFlipAfterResume_FirstInvoiceReadsPauseAtTrialEnd(t *testing.T) {
	r := newCancelRig(t, "trial flip after resume")
	trialEnd := r.f.cycleStart.Add(48 * time.Hour)
	r.trialFlipRig(t, trialEnd, trialEnd.Add(30*time.Minute))

	if n, errs := r.f.subSvc.ProcessExpiredTrials(r.at(trialEnd.Add(45*time.Minute)), 100); n != 1 || len(errs) != 0 {
		t.Fatalf("trial expiry: flipped=%d errs=%v, want 1 and none", n, errs)
	}
	invs, _ := r.liveInvoices(t)
	wantStatus(t, invoiceFor(t, invs, trialEnd, trialEnd.AddDate(0, 1, 0)), domain.InvoiceDraft,
		"the period began at trial end, while collection was paused (resumes_at trial end+30m)")
}

// TestTrialFlipAfterResume_ResumerRunsFirst: the trial flip is late (its scan
// is batched and can fail), so the pause resumer reaches the subscription
// first, past resumes_at. It must hold the pause while the trial that ended
// before resumes_at is unflipped; otherwise the flip reads no pause and
// finalizes, and the sweep charges, a period that began while paused.
//
// Mutation seam: drop the trialing arm from resumeNotWaitingOnCloseSQL → the
// resumer clears the pause and the invoice is finalized.
func TestTrialFlipAfterResume_ResumerRunsFirst(t *testing.T) {
	r := newCancelRig(t, "trial flip after the resumer")
	trialEnd := r.f.cycleStart.Add(48 * time.Hour)
	r.trialFlipRig(t, trialEnd, trialEnd.Add(30*time.Minute))

	if _, errs := r.f.subSvc.ProcessExpiredPauseCollections(r.at(trialEnd.Add(45*time.Minute)), 100); len(errs) != 0 {
		t.Fatalf("pause resumer: %v", errs)
	}
	if sub := r.sub(t); sub.PauseCollection == nil {
		t.Fatalf("the resumer cleared the pause while the trial that ended before resumes_at was unflipped")
	}
	if n, errs := r.f.subSvc.ProcessExpiredTrials(r.at(trialEnd.Add(50*time.Minute)), 100); n != 1 || len(errs) != 0 {
		t.Fatalf("trial expiry: flipped=%d errs=%v, want 1 and none", n, errs)
	}
	invs, _ := r.liveInvoices(t)
	wantStatus(t, invoiceFor(t, invs, trialEnd, trialEnd.AddDate(0, 1, 0)), domain.InvoiceDraft,
		"the period began at trial end, while collection was paused (resumes_at trial end+30m)")

	// Flipped: the hold is gone and the next pass clears the pause.
	if _, errs := r.f.subSvc.ProcessExpiredPauseCollections(r.at(trialEnd.Add(55*time.Minute)), 100); len(errs) != 0 {
		t.Fatalf("pause resumer after the flip: %v", errs)
	}
	if sub := r.sub(t); sub.PauseCollection != nil {
		t.Fatalf("pause still held after the trial flipped, want it cleared")
	}
}

// TestTrialFlipAfterResume_TestClock: the same on a test clock, one advance
// from before the trial end to past resumes_at (Phase 0.5).
func TestTrialFlipAfterResume_TestClock(t *testing.T) {
	r := newCancelRig(t, "trial flip after resume on a test clock")
	trialEnd := r.f.cycleStart.Add(48 * time.Hour)
	r.trialFlipRig(t, trialEnd, trialEnd.Add(30*time.Minute))

	frozen := trialEnd.Add(45 * time.Minute)
	clockID := postgres.NewID("vlx_tclk")
	execTx(t, r.f.db, r.base, r.f.tenantID,
		`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, 'trial flip clock', $3, 'advancing')`,
		clockID, r.f.tenantID, frozen)
	r.exec(t, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, clockID, r.f.customerID)
	r.exec(t, `UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, clockID, r.f.subID)
	clocks := testclock.NewPostgresStore(r.f.db)
	r.f.engine.SetTestClockReader(clocks)
	r.f.subSvc.SetResolver(r.f.engine)

	orchestrator := testclock.NewService(clocks)
	orchestrator.SetBillingRunner(r.f.engine)
	orchestrator.SetTrialExpirer(r.f.subSvc)
	orchestrator.SetPauseResumer(r.f.subSvc)
	if err := orchestrator.RunCatchup(r.base, testclock.CatchupJob{TenantID: r.f.tenantID, ClockID: clockID, PrevFrozenTime: r.f.cycleStart}); err != nil {
		t.Fatalf("catchup: %v", err)
	}
	if sub := r.sub(t); sub.Status != domain.SubscriptionActive {
		t.Fatalf("status after the advance = %s, want active", sub.Status)
	}
	invs, _ := r.liveInvoices(t)
	wantStatus(t, invoiceFor(t, invs, trialEnd, trialEnd.AddDate(0, 1, 0)), domain.InvoiceDraft,
		"the period began at trial end, while collection was paused (resumes_at trial end+30m)")
}

// TestHeldTimedPause_DoesNotBlockOtherInvoicesPastResumesAt: a timed pause
// whose resumes_at has passed stays on the row while the subscription still
// owes a close before resumes_at (here: billing parked on a failure). Its
// other invoices resume collection at resumes_at all the same: the auto-charge
// sweep picks up an invoice finalized before the pause. A pause still in
// force, and one with no resumes_at, keep it out.
//
// Mutation seam: drop the resumes_at arm from notPausedForCollection → the
// sweep skips the invoice while the expired pause is held.
func TestHeldTimedPause_DoesNotBlockOtherInvoicesPastResumesAt(t *testing.T) {
	r := newCancelRig(t, "held timed pause collection")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	if gen, failures := r.f.engine.RunCycleForTenant(r.at(p1.Add(time.Hour)), r.f.tenantID, 50); gen != 1 || len(failures) != 0 {
		t.Fatalf("close [P0, P1): generated=%d failures=%v", gen, failures)
	}
	invs, _ := r.liveInvoices(t)
	inv := invoiceFor(t, invs, p0, p1)
	if inv.Status != domain.InvoiceFinalized || !inv.AutoChargePending {
		t.Fatalf("invoice status=%s auto_charge_pending=%v, want finalized and queued (card-less)", inv.Status, inv.AutoChargePending)
	}

	// The sweep reads the database clock, so the pause is stamped against
	// it. next_billing_at before resumes_at is the shape of a close still
	// owed (a parked billing failure): the resumer must keep the pause.
	r.exec(t, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft',
		pause_collection_resumes_at = now() - interval '1 hour', next_billing_at = now() - interval '2 hours' WHERE id = $1`, r.f.subID)
	r.resumeExpired(t, time.Now().UTC())
	if sub := r.sub(t); sub.PauseCollection == nil {
		t.Fatal("the resumer cleared a pause while a close before resumes_at is still owed")
	}

	queued := func() bool {
		t.Helper()
		list, err := r.f.invStore.ListAutoChargePending(r.base, domain.InvoiceKeyset{}, 1000)
		if err != nil {
			t.Fatalf("list auto-charge pending: %v", err)
		}
		for _, q := range list {
			if q.ID == inv.ID {
				return true
			}
		}
		return false
	}
	if !queued() {
		t.Error("the auto-charge sweep skips the invoice although the pause's resumes_at has passed")
	}

	r.exec(t, `UPDATE subscriptions SET pause_collection_resumes_at = now() + interval '1 hour' WHERE id = $1`, r.f.subID)
	if queued() {
		t.Error("the auto-charge sweep picks up the invoice while the pause is in force")
	}
	r.exec(t, `UPDATE subscriptions SET pause_collection_resumes_at = NULL WHERE id = $1`, r.f.subID)
	if queued() {
		t.Error("the auto-charge sweep picks up the invoice under a pause with no resumes_at")
	}
}
