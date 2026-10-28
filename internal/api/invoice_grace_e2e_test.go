package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// INVOICE_GRACE_PERIOD over HTTP (ADR-120), on the close-lag rig: the real
// server with every collaborator wired as in production, the billing run
// being POST /v1/billing/run. The fixture is a 1c/call sum meter on plan A
// (monthly, base 3000c); April is P0 = [Apr 1, May 1) and B = May 1.

const e2eGrace = time.Hour

// ingestAt posts one usage event stamped ts that reaches Hardcount at arrival.
func (r *lagRig) ingestAt(ts, arrival time.Time, qty int64) {
	r.t.Helper()
	r.eventSeq++
	r.must(http.MethodPost, "/v1/usage-events", arrival, map[string]any{
		"external_customer_id": r.custExt, "event_name": "calls", "quantity": qty,
		"timestamp": ts.Format(time.RFC3339), "idempotency_key": fmt.Sprintf("grace-%d", r.eventSeq),
	}, http.StatusCreated)
}

// usageBilled sums the usage lines of every live invoice of the subscription.
func usageBilled(invs []lagInvoice) int64 {
	var n int64
	for _, i := range invs {
		n += i.sum("usage")
	}
	return n
}

// TestInvoiceGrace_LateUsageInsideTheGraceIsBilled is the policy's headline:
// an event stamped in April that reaches Hardcount 30 minutes after April ended
// is on April's invoice, because the invoice is built an hour after the
// period end. POST /v1/billing/run waits for the grace like the scheduler: a
// run at B+30m closes nothing and saves no failure. Once the invoice is
// built it does not change: an April event arriving after it is stored and
// billed nowhere.
//
// Mutation check: scan with the real now in RunCycleForTenant (no grace) →
// the scan admits April at B+30m, the engine declines it inside its grace,
// and the run answers 206 with the subscription saved as failing.
func TestInvoiceGrace_LateUsageInsideTheGraceIsBilled(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	r.ingestAt(lagB.Add(-10*time.Minute), lagB.Add(30*time.Minute), 40)

	if n := r.run(lagB.Add(30 * time.Minute)); n != 0 {
		t.Fatalf("POST /v1/billing/run at B+30m generated %d invoices, want 0: April is inside its grace", n)
	}
	if s := r.sub(); s.BillingFailure != nil || !s.CurrentBillingPeriodStart.Equal(lagP0) {
		t.Fatalf("after the run inside the grace: failure=%+v period start=%v, want no failure and April still open", s.BillingFailure, s.CurrentBillingPeriodStart)
	}

	if n := r.run(lagB.Add(e2eGrace + 5*time.Minute)); n != 1 {
		t.Fatalf("POST /v1/billing/run at B+1h05m generated %d invoices, want April's", n)
	}
	apr := r.cycleInvoice(r.invoices(), lagP0)
	if apr.sum("usage") != 140 {
		r.logInvoices(r.invoices())
		t.Fatalf("April usage billed = %dc, want 140c: the 40 calls that arrived inside the grace belong on it", apr.sum("usage"))
	}

	// After the invoice is built: accepted, stored, billed nowhere.
	r.ingestAt(lagB.Add(-5*time.Minute), lagB.Add(2*time.Hour), 25)
	r.run(lagB.Add(3 * time.Hour))
	invs := r.invoices()
	if got := r.cycleInvoice(invs, lagP0).sum("usage"); got != 140 {
		t.Errorf("April usage after a later event = %dc, want 140c: a built invoice does not change", got)
	}
	if got := usageBilled(invs); got != 140 {
		t.Errorf("usage billed on all invoices = %dc, want 140c: late usage is not moved onto another invoice", got)
	}
}

// TestInvoiceGrace_ZeroKeepsTheOldTiming is the control: with
// INVOICE_GRACE_PERIOD=0 the run a minute after B closes April, and the event
// arriving at B+30m misses it.
func TestInvoiceGrace_ZeroKeepsTheOldTiming(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(0))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	if n := r.run(lagB.Add(time.Minute)); n != 1 {
		t.Fatalf("run at B+1m with no grace generated %d invoices, want April's", n)
	}
	r.ingestAt(lagB.Add(-10*time.Minute), lagB.Add(30*time.Minute), 40)
	r.run(lagB.Add(2 * time.Hour))
	if got := r.cycleInvoice(r.invoices(), lagP0).sum("usage"); got != 100 {
		t.Errorf("April usage = %dc, want 100c: with no grace the 40 late calls missed the invoice", got)
	}
}

// TestInvoiceGrace_MidPeriodCancelAtWaitsTheGrace: a cancel scheduled for
// May 16 (inside May) is a due instant like a period end, so its final
// invoice also waits the grace and carries usage that arrives inside it. The
// subscription is still canceled at the contracted instant. While it waits,
// no run saves a billing failure: the cancel arm of the due scan and the
// engine's own check agree.
//
// Mutation check: drop the grace from the cancel arm of
// GetDueBillingForTenant only → the run at C+30m admits the subscription, the
// engine declines it, and the drain saves errStillDue as a failure.
func TestInvoiceGrace_MidPeriodCancelAtWaitsTheGrace(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	// A cancel_at must lie past the period in progress (April), so May's
	// middle is the earliest mid-period instant.
	cancelAt := lagB.AddDate(0, 0, 15)
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/schedule-cancel", lagP0.AddDate(0, 0, 5),
		map[string]any{"cancel_at": cancelAt.Format(time.RFC3339)}, http.StatusOK)
	if n := r.run(lagB.Add(2 * time.Hour)); n != 1 {
		t.Fatalf("run at B+2h generated %d invoices, want April's", n)
	}
	r.ingest(lagB.AddDate(0, 0, 9), 100)
	r.ingestAt(cancelAt.Add(-5*time.Minute), cancelAt.Add(20*time.Minute), 7)

	if n := r.run(cancelAt.Add(30 * time.Minute)); n != 0 {
		t.Fatalf("run at cancel_at+30m generated %d invoices, want 0: the cancel is inside its grace", n)
	}
	if s := r.sub(); s.Status != domain.SubscriptionActive || s.BillingFailure != nil {
		t.Fatalf("inside the grace: status=%s failure=%+v, want active with no failure", s.Status, s.BillingFailure)
	}

	r.run(cancelAt.Add(e2eGrace + 5*time.Minute))
	s := r.sub()
	if s.Status != domain.SubscriptionCanceled || s.CanceledAt == nil || !s.CanceledAt.Equal(cancelAt) {
		t.Fatalf("after the grace: status=%s canceled_at=%v, want canceled at %v", s.Status, s.CanceledAt, cancelAt)
	}
	if s.BillingFailure != nil {
		t.Errorf("billing failure = %+v, want none", s.BillingFailure)
	}
	invs := r.invoices()
	if got := usageBilled(invs); got != 107 {
		r.logInvoices(invs)
		t.Errorf("usage billed = %dc, want 107c: the 7 calls that arrived inside the grace belong on the final invoice", got)
	}
}

// TestInvoiceGrace_ImmediateCancelClosesTheEndedPeriodNow: an immediate
// cancel inside the grace does not wait and is not refused. It closes April
// first, as the operator acted on the subscription now.
//
// Mutation check: make Cancel's pre-close honour the grace → the cancel is
// refused with 409 unbilled_period.
func TestInvoiceGrace_ImmediateCancelClosesTheEndedPeriodNow(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)

	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/cancel", lagB.Add(30*time.Minute), nil, http.StatusOK)
	if s := r.sub(); s.Status != domain.SubscriptionCanceled {
		t.Fatalf("status = %s, want canceled", s.Status)
	}
	if got := r.cycleInvoice(r.invoices(), lagP0).sum("usage"); got != 100 {
		t.Errorf("April usage = %dc, want 100c", got)
	}
}

// TestInvoiceGrace_SubscriptionWriteClosesTheEndedPeriodNow: any other
// subscription write inside the grace closes the ended period first (#1038)
// and does not wait for the grace.
func TestInvoiceGrace_SubscriptionWriteClosesTheEndedPeriodNow(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)

	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagB.Add(10*time.Minute),
		map[string]any{"quantity": 3}, http.StatusOK)
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagB) {
		t.Fatalf("period start = %v, want May: the write closed April first", s.CurrentBillingPeriodStart)
	}
	if got := r.cycleInvoice(r.invoices(), lagP0).sum("usage"); got != 100 {
		t.Errorf("April usage = %dc, want 100c", got)
	}
}

// TestInvoiceGrace_RetryBillingDoesNotWait: April's close fails inside the
// grace (an item write tried it first and the tenant's net terms are
// invalid), and the failure is saved. Once the cause is fixed,
// retry-billing closes April at once, still inside the grace.
func TestInvoiceGrace_RetryBillingDoesNotWait(t *testing.T) {
	skipShort(t)
	// in_advance: the close that fails on negative net terms is the one the
	// close-lag suite already uses (TestCloseLag_ItemWritesWhileBillingFails_Refused).
	r := newLagRigWith(t, "in_advance", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	r.exec(`UPDATE tenant_settings SET net_payment_terms = -1 WHERE tenant_id = (SELECT tenant_id FROM subscriptions WHERE id = $1)`, r.subID)

	code, raw := r.do(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagB.Add(10*time.Minute), map[string]any{"quantity": 3})
	wantAPIError(t, code, raw, http.StatusConflict, "unbilled_period", "not invoiced yet")
	if s := r.sub(); s.BillingFailure == nil {
		t.Fatalf("no billing failure saved: the write's pre-close did not attempt April inside the grace")
	}

	r.exec(`UPDATE tenant_settings SET net_payment_terms = 30 WHERE tenant_id = (SELECT tenant_id FROM subscriptions WHERE id = $1)`, r.subID)
	out := r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/retry-billing", lagB.Add(20*time.Minute), nil, http.StatusOK)
	if n, _ := out["invoices_generated"].(float64); n != 1 {
		t.Fatalf("retry-billing at B+20m generated %v invoices, want 1: %v", out["invoices_generated"], out)
	}
	if s := r.sub(); s.BillingFailure != nil || !s.CurrentBillingPeriodStart.Equal(lagB) {
		t.Errorf("after the retry: failure=%+v period start=%v, want no failure and May running", s.BillingFailure, s.CurrentBillingPeriodStart)
	}
}

// TestInvoiceGrace_CatchUpOverHTTP: after downtime, POST /v1/billing/run
// inside May's grace bills April at once and leaves May for a run after its
// grace.
func TestInvoiceGrace_CatchUpOverHTTP(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	if n := r.run(lagP2.Add(30 * time.Minute)); n != 1 {
		t.Fatalf("run at Jun 1 + 30m generated %d invoices, want April's only", n)
	}
	if s := r.sub(); s.BillingFailure != nil || !s.CurrentBillingPeriodStart.Equal(lagB) {
		t.Fatalf("failure=%+v period start=%v, want no failure and May still open", s.BillingFailure, s.CurrentBillingPeriodStart)
	}
	if n := r.run(lagP2.Add(e2eGrace)); n != 1 {
		t.Fatalf("run at Jun 1 + 1h generated %d invoices, want May's", n)
	}
}

// Dunning's final actions and the grace (ADR-120 amendment 2026-10-22).
// Dunning is not the operator acting now: its final pause or cancel, landing
// inside an ended period's grace, waits for the grace like the billing run,
// so usage that arrives inside the grace is on the period's invoice. The run
// is due again when the grace ends, and only then pauses or cancels and
// escalates. The unpaid invoice is a manual one on the subscription (a
// threshold or one-off invoice can exhaust its dunning at any time of day).

// graceDunning is a dunning policy on the lag rig and the run under it.
type graceDunning struct {
	r     *lagRig
	ctx   context.Context
	runs  *dunning.PostgresStore
	pol   domain.DunningPolicy
	invID string
	runID string
}

// exhaustedDunning leaves a manual invoice of the subscription unpaid, with
// an active dunning run whose one retry is used and whose exhaustion is due
// at due, under a policy with the given final actions.
func (r *lagRig) exhaustedDunning(subAction domain.DunningSubscriptionAction, invAction domain.DunningInvoiceAction, due time.Time) *graceDunning {
	r.t.Helper()
	ctx := leadertest.Token(r.t, testutil.AdminPool(r.t), postgres.WithLivemode(context.Background(), true), leader.RoleDunning)
	d := &graceDunning{r: r, ctx: ctx, runs: dunning.NewPostgresStore(r.db)}
	pol, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
		Name: "grace wait", Enabled: true, RetryDays: []int{3},
		FinalSubscriptionAction: subAction, FinalInvoiceAction: invAction,
	})
	if err != nil {
		r.t.Fatalf("upsert policy: %v", err)
	}
	d.pol = pol
	issued := lagP0.AddDate(0, 0, 11)
	inv, err := invoice.NewPostgresStore(r.db).Create(ctx, r.tenantID, domain.Invoice{
		CustomerID: r.custID, SubscriptionID: r.subID, InvoiceNumber: "GRACE-1", BillingReason: domain.BillingReasonManual,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 500, TotalAmountCents: 500, AmountDueCents: 500,
		BillingPeriodStart: lagP0.AddDate(0, 0, 10), BillingPeriodEnd: issued, IssuedAt: &issued,
	})
	if err != nil {
		r.t.Fatalf("create invoice: %v", err)
	}
	d.invID = inv.ID
	run, err := d.runs.CreateRun(ctx, r.tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: r.custID, PolicyID: pol.ID,
		State: domain.DunningActive, Reason: "payment_failed", AttemptCount: pol.Retries(), NextActionAt: &due,
	})
	if err != nil {
		r.t.Fatalf("create run: %v", err)
	}
	if ok, err := d.runs.UpdateRunIfActive(ctx, r.tenantID, run, run.AttemptCount, nil); err != nil || !ok {
		r.t.Fatalf("record the attempts: applied=%v err=%v", ok, err)
	}
	d.runID = run.ID
	return d
}

// tick is the dunning scheduler's tick at the instant at.
func (d *graceDunning) tick(at time.Time) {
	d.r.t.Helper()
	d.r.clk.Set(at)
	if _, errs := d.r.srv.DunningSvc.ProcessDueRuns(clock.WithEffectiveNow(d.ctx, at), d.r.tenantID, 50); len(errs) > 0 {
		d.r.t.Fatalf("dunning tick at %v: %v", at, errs)
	}
}

func (d *graceDunning) run() domain.InvoiceDunningRun {
	d.r.t.Helper()
	run, err := d.runs.GetRun(d.ctx, d.r.tenantID, d.runID)
	if err != nil {
		d.r.t.Fatalf("get run: %v", err)
	}
	return run
}

// escalations counts the run's escalated timeline rows.
func (d *graceDunning) escalations() int {
	d.r.t.Helper()
	evs, err := d.runs.ListEvents(d.ctx, d.r.tenantID, d.runID)
	if err != nil {
		d.r.t.Fatalf("list run events: %v", err)
	}
	n := 0
	for _, e := range evs {
		if e.EventType == domain.DunningEventEscalated {
			n++
		}
	}
	return n
}

// waiting asserts the run waits for the grace: active, not action_failed,
// not escalated, due again at until.
func (d *graceDunning) waiting(when string, until time.Time) {
	d.r.t.Helper()
	run := d.run()
	if run.State != domain.DunningActive || run.Resolution == domain.ResolutionActionFailed ||
		run.NextActionAt == nil || !run.NextActionAt.Equal(until) {
		d.r.t.Fatalf("%s: run state=%s resolution=%q next_action_at=%v, want active, not action_failed, due again at %v",
			when, run.State, run.Resolution, run.NextActionAt, until)
	}
	// The dashboard labels next_action_at by whether a retry is still to
	// come: the list read carries the policy's limit, which the run has
	// reached, so the wait reads as the final step, not a retry.
	out := d.r.must(http.MethodGet, "/v1/dunning/runs?invoice_id="+d.invID, d.r.clk.Now(context.Background()), nil, http.StatusOK)
	rows, _ := out["data"].([]any)
	if len(rows) != 1 {
		d.r.t.Fatalf("%s: GET /v1/dunning/runs returned %d rows, want 1", when, len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if row["max_retry_attempts"] != float64(d.pol.Retries()) || row["attempt_count"] != float64(d.pol.Retries()) {
		d.r.t.Fatalf("%s: listed run attempt_count=%v max_retry_attempts=%v, want both %d: every retry is used",
			when, row["attempt_count"], row["max_retry_attempts"], d.pol.Retries())
	}
	if n := d.escalations(); n != 0 {
		d.r.t.Fatalf("%s: %d escalated rows, want none while the action waits", when, n)
	}
}

// escalated asserts the run escalated once.
func (d *graceDunning) escalated() {
	d.r.t.Helper()
	if run := d.run(); run.State != domain.DunningEscalated || run.Resolution != domain.ResolutionRetriesExhausted {
		d.r.t.Fatalf("after the grace: run state=%s resolution=%q, want escalated as retries_exhausted", run.State, run.Resolution)
	}
	if n := d.escalations(); n != 1 {
		d.r.t.Fatalf("%d escalated rows, want 1", n)
	}
}

// TestInvoiceGrace_DunningPauseWaitsForTheGrace: a dunning run on the
// subscription exhausts at B+5m under a pause policy that also writes the
// invoice off. April ended 5 minutes ago and is inside its grace, so the
// pause waits: April stays open, nothing is paused or written off, the run is
// not escalated and is due again at B+1h. An April event arriving at B+20m is
// then on April's invoice, which dunning's own close builds at B+1h05m before
// it pauses.
//
// Mutation checks: remove the grace guard from closeEndedPeriods → the pause
// is refused unbilled_period and the run is action_failed, due a day later;
// pass 0 to BillDue from closeEndedPeriods → the pause closes April at B+5m
// (April usage 100); send the wait down the action_failed path → the run is
// due a day later.
func TestInvoiceGrace_DunningPauseWaitsForTheGrace(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	d := r.exhaustedDunning(domain.SubActionPause, domain.InvActionMarkUncollectible, lagB)

	d.tick(lagB.Add(5 * time.Minute))
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagP0) || s.PauseCollection != nil || s.BillingFailure != nil {
		t.Fatalf("at B+5m: period start=%v pause=%+v failure=%+v, want April open, no pause, no failure", s.CurrentBillingPeriodStart, s.PauseCollection, s.BillingFailure)
	}
	d.waiting("at B+5m", lagB.Add(e2eGrace))
	if inv := r.invoiceStatus(d.invID); inv != string(domain.InvoiceFinalized) {
		t.Fatalf("at B+5m the invoice is %s, want finalized: the write-off waits with the pause", inv)
	}

	r.ingestAt(lagB.Add(-10*time.Minute), lagB.Add(20*time.Minute), 40)
	d.tick(lagB.Add(30 * time.Minute))
	d.waiting("at B+30m", lagB.Add(e2eGrace))

	d.tick(lagB.Add(e2eGrace + 5*time.Minute))
	s := r.sub()
	if s.PauseCollection == nil || s.PauseCollection.SetBy != domain.PauseSetByDunning {
		t.Fatalf("after the grace: pause=%+v, want dunning's pause", s.PauseCollection)
	}
	if !s.CurrentBillingPeriodStart.Equal(lagB) {
		t.Fatalf("after the grace: period start=%v, want May", s.CurrentBillingPeriodStart)
	}
	d.escalated()
	if inv := r.invoiceStatus(d.invID); inv != string(domain.InvoiceUncollectible) {
		t.Errorf("after the grace the invoice is %s, want uncollectible", inv)
	}
	if got := r.cycleInvoice(r.invoices(), lagP0).sum("usage"); got != 140 {
		r.logInvoices(r.invoices())
		t.Errorf("April usage = %dc, want 140c: the 40 calls that arrived inside the grace belong on April's invoice", got)
	}
}

// TestInvoiceGrace_DunningCancelWaitsForTheGrace is the cancel policy's
// version: the subscription stays active inside the grace and is canceled
// after it, with April's invoice carrying the usage that arrived inside the
// grace.
func TestInvoiceGrace_DunningCancelWaitsForTheGrace(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	d := r.exhaustedDunning(domain.SubActionCancel, domain.InvActionNone, lagB)

	d.tick(lagB.Add(5 * time.Minute))
	if s := r.sub(); s.Status != domain.SubscriptionActive || !s.CurrentBillingPeriodStart.Equal(lagP0) || s.BillingFailure != nil {
		t.Fatalf("at B+5m: status=%s period start=%v failure=%+v, want active, April open, no failure", s.Status, s.CurrentBillingPeriodStart, s.BillingFailure)
	}
	d.waiting("at B+5m", lagB.Add(e2eGrace))

	r.ingestAt(lagB.Add(-10*time.Minute), lagB.Add(20*time.Minute), 40)
	d.tick(lagB.Add(e2eGrace + 5*time.Minute))
	if s := r.sub(); s.Status != domain.SubscriptionCanceled {
		t.Fatalf("after the grace: status=%s, want canceled", s.Status)
	}
	d.escalated()
	if got := r.cycleInvoice(r.invoices(), lagP0).sum("usage"); got != 140 {
		r.logInvoices(r.invoices())
		t.Errorf("April usage = %dc, want 140c: the 40 calls that arrived inside the grace belong on April's invoice", got)
	}
}

// TestInvoiceGrace_DunningCatchUpClosesTheOlderPeriodAndWaits: the billing
// run was down over two period ends. Dunning's pause at Jun 1 00:05 closes
// April, whose grace passed long ago, and waits for May's: May stays open
// until Jun 1 01:00, and a May event arriving at 00:20 is on May's invoice.
//
// Mutation checks: bill with grace 0 in Engine.BillDue, or have
// closeEndedPeriods pass 0 to it → May is closed at 00:05 too.
func TestInvoiceGrace_DunningCatchUpClosesTheOlderPeriodAndWaits(t *testing.T) {
	skipShort(t)
	r := newLagRigWith(t, "in_arrears", false, WithInvoiceGrace(e2eGrace))
	r.ingest(lagP0.AddDate(0, 0, 19), 100)
	r.ingest(lagB.AddDate(0, 0, 9), 30)
	d := r.exhaustedDunning(domain.SubActionPause, domain.InvActionNone, lagP2)

	d.tick(lagP2.Add(5 * time.Minute))
	if s := r.sub(); !s.CurrentBillingPeriodStart.Equal(lagB) || s.PauseCollection != nil || s.BillingFailure != nil {
		t.Fatalf("at Jun 1 00:05: period start=%v pause=%+v failure=%+v, want April closed, May open, no pause, no failure",
			s.CurrentBillingPeriodStart, s.PauseCollection, s.BillingFailure)
	}
	if got := r.cycleInvoice(r.invoices(), lagP0).sum("usage"); got != 100 {
		t.Errorf("April usage = %dc, want 100c", got)
	}
	d.waiting("at Jun 1 00:05", lagP2.Add(e2eGrace))

	r.ingestAt(lagP2.Add(-10*time.Minute), lagP2.Add(20*time.Minute), 40)
	d.tick(lagP2.Add(e2eGrace + 5*time.Minute))
	if s := r.sub(); s.PauseCollection == nil || !s.CurrentBillingPeriodStart.Equal(lagP2) {
		t.Fatalf("after May's grace: pause=%+v period start=%v, want paused with June running", s.PauseCollection, s.CurrentBillingPeriodStart)
	}
	d.escalated()
	if got := r.cycleInvoice(r.invoices(), lagB).sum("usage"); got != 70 {
		r.logInvoices(r.invoices())
		t.Errorf("May usage = %dc, want 70c: the 40 calls that arrived inside May's grace belong on May's invoice", got)
	}
}

// invoiceStatus is the invoice's status as GET /v1/invoices/{id} returns it.
func (r *lagRig) invoiceStatus(id string) string {
	r.t.Helper()
	out := r.must(http.MethodGet, "/v1/invoices/"+id, r.clk.Now(context.Background()), nil, http.StatusOK)
	inv, _ := out["invoice"].(map[string]any)
	s, _ := inv["status"].(string)
	return s
}
