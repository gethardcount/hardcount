package billing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/usage"
)

// A period ends at a boundary B, and the billing run closes it on a later
// tick: up to an hour later today, longer after downtime or a failed attempt.
// These tests pin what the close bills when it runs late.
//
// They use cancelRig (cancel_after_period_end_integration_test.go): a monthly
// plan with a 5000c in_arrears base and a 1c/call sum meter, a one-month
// period [P0, P1) and 100 events × qty 10 in its first hours. Time is bound per
// call with clock.WithEffectiveNow, as the billing run binds each row's
// instant.

// makeLastMeter makes the rig's meter a last meter. Its plan prices it with
// one catch-all price: the billed quantity is the latest event's in the
// period. Written past the API, which refuses an aggregation change once the
// meter has events (ADR-130 §2): it stands for configuration made before the
// period.
func (r *cancelRig) makeLastMeter(t *testing.T) {
	t.Helper()
	tx, err := r.f.db.BeginTx(r.base, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE meters SET aggregation = 'last' WHERE id = $1`, r.f.meterID); err != nil {
		t.Fatalf("make the meter a last meter: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
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

// TestLateClose_Last_BillsValueAsOfPeriodEnd (D2): a last meter bills the
// latest value in the period. The value changes to 100 five hours after the
// boundary, and the billing run closes the period an hour after that. The
// ended period's invoice must bill 10 (the value at the boundary), not 100.
//
// Mutation seam: drop the `timestamp < $4` bound from the last-value read in
// usage/postgres.go (AggregateForBillingPeriodByAgg) → the invoice bills 100.
func TestLateClose_Last_BillsValueAsOfPeriodEnd(t *testing.T) {
	r := newCancelRig(t, "close lag last")
	r.makeLastMeter(t)
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	r.ingestOne(t, p1.Add(5*time.Hour), 100)

	if gen, failures := r.f.engine.RunCycleForTenant(r.at(p1.Add(6*time.Hour)), r.f.tenantID, 50); gen != 1 || len(failures) != 0 {
		t.Fatalf("billing run: generated=%d failures=%v, want 1 and none", gen, failures)
	}
	invs, lines := r.liveInvoices(t)
	inv := invoiceFor(t, invs, p0, p1)
	usageCents, _ := sumLines(lines[inv.ID])
	if usageCents != 10 {
		t.Errorf("ended period [P0, P1) bills last usage %dc, want 10c: the value at the period end, not the value set 5h after it", usageCents)
	}
}

// TestLateClose_Last_CatchUp_EachPeriodBillsItsOwnValue (D2, catch-up): the
// billing run is two periods behind. Each period's invoice bills the latest
// value in its own period: 10 for [P0, P1) and 40 for [P1, P2), even though
// the run reads both after the value became 70.
func TestLateClose_Last_CatchUp_EachPeriodBillsItsOwnValue(t *testing.T) {
	r := newCancelRig(t, "close lag last catch-up")
	r.makeLastMeter(t)
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
		t.Errorf("[P0, P1) bills last usage %dc, want 10c", u)
	}
	if u, _ := sumLines(lines[second.ID]); u != 40 {
		t.Errorf("[P1, P2) bills last usage %dc, want 40c", u)
	}
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

// resumeExpired runs the scheduler's pause resumer at the given instant.
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
// still-due check → the pause is written onto the ended period and the next
// close drafts its invoice.
func TestCloseLag_WriteWhileBillingFails_Refused(t *testing.T) {
	r := newCancelRig(t, "close lag write refused")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	r.f.invAdapter.createTxErr = errors.New("invoice insert refused (test)")
	now := p1.Add(time.Hour)
	pause := subscription.PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft}

	_, err := r.f.subSvc.PauseCollection(r.at(now), r.f.tenantID, r.f.subID, pause)
	wantWriteRefusedUnbilled(t, err)
	_, _, err = r.f.subSvc.ResumeCollection(r.at(now), r.f.tenantID, r.f.subID)
	wantWriteRefusedUnbilled(t, err)
	sub := r.sub(t)
	if sub.PauseCollection != nil || !sub.CurrentBillingPeriodStart.Equal(p0) {
		t.Fatalf("after the refusals: pause=%v period start=%v, want nothing written and the period still [P0, P1)",
			sub.PauseCollection, sub.CurrentBillingPeriodStart)
	}
	if sub.BillingFailure == nil || sub.BillingFailure.Attempts != 1 {
		t.Fatalf("billing failure = %+v, want the first refused write's attempt saved (attempt 1); the second write ran inside the wait and attempted nothing", sub.BillingFailure)
	}

	// Billing works again; retry-billing closes P0, and the pause lands in P1.
	r.f.invAdapter.createTxErr = nil
	now = now.Add(time.Hour)
	if res, err := r.f.subSvc.RetryBilling(r.at(now), r.f.tenantID, r.f.subID); err != nil || res.InvoicesGenerated != 1 {
		t.Fatalf("retry billing: %v generated=%d", err, res.InvoicesGenerated)
	}
	paused, err := r.f.subSvc.PauseCollection(r.at(now), r.f.tenantID, r.f.subID, pause)
	if err != nil || paused.PauseCollection == nil || !paused.CurrentBillingPeriodStart.Equal(p1) {
		t.Fatalf("pause after the retry: %v pause=%v period start=%v, want paused in [P1, P2)", err, paused.PauseCollection, paused.CurrentBillingPeriodStart)
	}
	r.runWritesNothing(t, now.Add(time.Hour))
	invs, _ := r.liveInvoices(t)
	wantStatus(t, invoiceFor(t, invs, p0, p1), domain.InvoiceFinalized, "collection was paused in [P1, P2), after [P0, P1) was closed")
}

// TestCloseLag_CrossIntervalSwap_OnEndedPeriod_RefusedInTx (D3 backstop): the
// atomic swap is handed a subscription whose period has ended unbilled (a
// boundary that passed after the handler's pre-close). It must refuse with
// unbilled_period and write nothing, not re-anchor past the ended period.
func TestCloseLag_CrossIntervalSwap_OnEndedPeriod_RefusedInTx(t *testing.T) {
	r := newCancelRig(t, "close lag swap backstop")
	p0, p1 := r.f.cycleStart, r.f.cycleEnd
	yearly, _, err := r.f.pricingSvc.CreatePlan(r.base, r.f.tenantID, pricing.CreatePlanInput{
		Code: "pln_yearly", Name: "Yearly", Currency: "USD", BillingInterval: domain.BillingYearly,
		BaseAmountCents: 50000, BaseBillTiming: domain.BillInArrears, Prices: []pricing.PriceInput{{MeterID: r.f.meterID, PriceShape: perUnitShape("1")}},
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

// TestHeldTimedPause_DoesNotBlockOtherInvoicesPastResumesAt: a timed pause
// whose resumes_at has passed stays on the row while the subscription still
// owes a close before resumes_at (here: billing parked on a failure). Its
// other invoices resume collection at resumes_at all the same: the auto-charge
// sweep picks up an invoice finalized before the pause. A pause still in
// force, and one with no resumes_at, keep it out.
//
// Mutation seam: drop the resumes_at arm from domain.NotPausedForCollectionSQL → the
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
		list, err := r.f.invStore.ListAutoChargePending(r.base, walk.Key{}, 1000)
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
