package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// INVOICE_GRACE_PERIOD over HTTP (ADR-120), on the close-lag rig: the real
// server with every collaborator wired as in production, the billing run
// being POST /v1/billing/run. The fixture is a 1c/call sum meter on plan A
// (monthly, base 3000c); April is P0 = [Apr 1, May 1) and B = May 1.

const e2eGrace = time.Hour

// ingestAt posts one usage event stamped ts that reaches Velox at arrival.
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
// an event stamped in April that reaches Velox 30 minutes after April ended
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
