package billing_test

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// INVOICE_GRACE_PERIOD (ADR-120) on the engine's own runs, on real Postgres.
// The two wall-clock billing runs close a period only once the grace has
// passed after its end; the operator's one-subscription close (BillDueNow,
// behind retry-billing and every subscription write's pre-close) and the
// test-clock run do not wait. The HTTP-level cases (late usage billed, a
// mid-period cancel_at, POST /v1/billing/run, immediate cancel) are in
// internal/api/invoice_grace_e2e_test.go.

const testGrace = time.Hour

// TestInvoiceGrace_SchedulerRunWaitsForTheGrace: a period that ended at E is
// not closed by the scheduler's run until E + grace, and waiting saves no
// billing failure. When it is closed, the invoice is dated at E, not at the
// run.
//
// Mutation checks: drop the grace from the next_billing_at arm of
// GetDueBilling → the run at E+30m admits the sub, billSubscription declines
// it, and the drain saves errStillDue (the billing columns are not blank).
// Drop it from billSubscription instead → the run at E+30m bills.
func TestInvoiceGrace_SchedulerRunWaitsForTheGrace(t *testing.T) {
	env := newBillFailureEnv(t, "Invoice Grace Scheduler")
	env.engine.SetInvoiceGrace(testGrace)
	a := env.dueSub(t, "sub-grace", env.planOK, env.periodEnd)

	for _, at := range []time.Duration{30 * time.Minute, testGrace - time.Second} {
		env.clock.Set(env.periodEnd.Add(at))
		gen, errs := env.engine.RunCycle(env.ctx, 50, nil)
		if gen != 0 || len(errs) != 0 {
			t.Fatalf("run at end+%v: generated=%d errors=%v, want nothing: the period is inside its grace", at, gen, errs)
		}
		if n := env.invoiceCount(t, a.ID); n != 0 {
			t.Fatalf("run at end+%v: %d invoices, want 0", at, n)
		}
		if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
			t.Fatalf("run at end+%v saved a billing failure on a healthy subscription inside its grace: %s", at, cols)
		}
	}

	env.clock.Set(env.periodEnd.Add(testGrace))
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 1 || len(errs) != 0 {
		t.Fatalf("run at end+grace: generated=%d errors=%v, want the period closed", gen, errs)
	}
	invs, _, err := env.invoices.List(env.ctx, invoice.ListFilter{TenantID: env.tenantID, SubscriptionID: a.ID})
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices: %v %v", invs, err)
	}
	inv := invs[0]
	// The grace moves when the invoice is built, not what it says.
	if !inv.BillingPeriodEnd.Equal(env.periodEnd) || inv.IssuedAt == nil || !inv.IssuedAt.Equal(env.periodEnd) {
		t.Errorf("invoice period end=%v issued_at=%v, want both at the period end %v", inv.BillingPeriodEnd, inv.IssuedAt, env.periodEnd)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Errorf("billing columns after the close = %s, want blank", cols)
	}
}

// TestInvoiceGrace_ZeroClosesAsSoonAsDue is the control: with no grace the
// same run two seconds after the period end closes it, as before ADR-120.
func TestInvoiceGrace_ZeroClosesAsSoonAsDue(t *testing.T) {
	env := newBillFailureEnv(t, "Invoice Grace Zero")
	env.engine.SetInvoiceGrace(0)
	a := env.dueSub(t, "sub-grace-zero", env.planOK, env.periodEnd)
	// env's clock starts at periodEnd + 2s.
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 1 || len(errs) != 0 {
		t.Fatalf("run at end+2s with no grace: generated=%d errors=%v, want the period closed", gen, errs)
	}
	if n := env.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices, want 1", n)
	}
}

// TestInvoiceGrace_CatchUpBillsOlderPeriodsAtOnceAndTheNewestAfterItsGrace:
// after downtime a subscription owes two periods. A run inside the newest
// period's grace bills the older one only, saves no failure, and leaves the
// newest for a run after its grace.
//
// Mutation check: drop the grace from billSubscription's caught-up check →
// the first run bills both periods.
func TestInvoiceGrace_CatchUpBillsOlderPeriodsAtOnceAndTheNewestAfterItsGrace(t *testing.T) {
	env := newBillFailureEnv(t, "Invoice Grace Catch-up")
	env.engine.SetInvoiceGrace(testGrace)
	feb := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mar, apr := feb.AddDate(0, 1, 0), feb.AddDate(0, 2, 0)
	a := env.subWith(t, domain.Subscription{Code: "sub-grace-catchup", Status: domain.SubscriptionActive}, env.planOK, feb, mar, mar)

	env.clock.Set(apr.Add(30 * time.Minute))
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 1 || len(errs) != 0 {
		t.Fatalf("run at Apr 1 + 30m: generated=%d errors=%v, want February's period only", gen, errs)
	}
	sub := env.get(t, a.ID)
	if sub.NextBillingAt == nil || !sub.NextBillingAt.Equal(apr) {
		t.Fatalf("next_billing_at = %v, want %v: March is inside its grace", sub.NextBillingAt, apr)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("the catch-up saved a billing failure: %s", cols)
	}

	env.clock.Set(apr.Add(testGrace))
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 1 || len(errs) != 0 {
		t.Fatalf("run at Apr 1 + grace: generated=%d errors=%v, want March's period", gen, errs)
	}
	if n := env.invoiceCount(t, a.ID); n != 2 {
		t.Fatalf("%d invoices, want 2 (one per period)", n)
	}
}

// TestInvoiceGrace_OperatorCloseDoesNotWait: BillDueNow (retry-billing, and
// the pre-close every subscription write runs) closes the ended period inside
// its grace. The operator is acting on this subscription now.
//
// Mutation check: pass the engine's grace from BillDueNow → no invoice, and
// BillDueNow reports errStillDue.
func TestInvoiceGrace_OperatorCloseDoesNotWait(t *testing.T) {
	env := newBillFailureEnv(t, "Invoice Grace Operator Close")
	env.engine.SetInvoiceGrace(testGrace)
	a := env.dueSub(t, "sub-grace-operator", env.planOK, env.periodEnd)
	env.clock.Set(env.periodEnd.Add(10 * time.Minute))

	n, err := env.engine.BillDueNow(env.ctx, env.tenantID, a.ID)
	if err != nil || n != 1 {
		t.Fatalf("BillDueNow inside the grace: generated=%d err=%v, want the period closed now", n, err)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("billing columns = %s, want blank", cols)
	}
}

// TestInvoiceGrace_TestClockDoesNotWait: advancing a test clock to a period
// end produces the invoice, whatever the grace. In a simulation the operator
// decides when usage arrives relative to an advance.
//
// Mutation check: pass the engine's grace from RunCycleForClock → no invoice.
func TestInvoiceGrace_TestClockDoesNotWait(t *testing.T) {
	env := newBillFailureEnv(t, "Invoice Grace Test Clock")
	env.pricing.failing.Store(false)
	env.engine.SetInvoiceGrace(testGrace)
	// clockSub pins a sub due at the period end to a clock frozen at the
	// period end + 2s.
	a, clockID := env.clockSub(t, "sub-grace-clock", env.planOK)

	if gen, errs := env.engine.RunCycleForClock(env.ctx, env.tenantID, clockID, 50); gen != 1 || len(errs) != 0 {
		t.Fatalf("clock advanced 2s past the period end: generated=%d errors=%v, want the period closed", gen, errs)
	}
	if n := env.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices, want 1", n)
	}
}

// TestInvoiceGrace_SchedulerMidPeriodCancelWaitsForTheGrace: the scheduler's
// cancel arm. A cancel_at C inside the period is not fired by the scheduler's
// run until C + grace, and waiting saves no billing failure. When it fires,
// canceled_at is C, not the run.
//
// Mutation check: drop the grace from the cancel_at arm of GetDueBilling →
// the run at C+30m admits the sub, billSubscription declines it, and the
// drain saves errStillDue (the billing columns are not blank).
func TestInvoiceGrace_SchedulerMidPeriodCancelWaitsForTheGrace(t *testing.T) {
	env := newBillFailureEnv(t, "Invoice Grace Scheduler Cancel")
	env.engine.SetInvoiceGrace(testGrace)
	env.engine.SetScheduledCancelExecutor(subscription.NewService(env.subs, env.clock))
	apr, may := env.periodEnd, env.periodEnd.AddDate(0, 1, 0)
	a := env.subWith(t, domain.Subscription{Code: "sub-grace-cancel", Status: domain.SubscriptionActive}, env.planOK, apr, may, may)
	cancelAt := apr.AddDate(0, 0, 5)
	if _, err := env.subs.ScheduleCancellation(env.ctx, env.tenantID, a.ID, &cancelAt, false, domain.SubscriptionActive); err != nil {
		t.Fatalf("schedule cancel: %v", err)
	}

	env.clock.Set(cancelAt.Add(30 * time.Minute))
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 0 || len(errs) != 0 {
		t.Fatalf("run at cancel_at+30m: generated=%d errors=%v, want nothing: the cancel is inside its grace", gen, errs)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("run at cancel_at+30m saved a billing failure on a healthy subscription inside its grace: %s", cols)
	}
	if s := env.get(t, a.ID); s.Status != domain.SubscriptionActive {
		t.Fatalf("status = %s inside the grace, want active", s.Status)
	}

	env.clock.Set(cancelAt.Add(testGrace))
	if _, errs := env.engine.RunCycle(env.ctx, 50, nil); len(errs) != 0 {
		t.Fatalf("run at cancel_at+grace: errors=%v", errs)
	}
	s := env.get(t, a.ID)
	if s.Status != domain.SubscriptionCanceled || s.CanceledAt == nil || !s.CanceledAt.Equal(cancelAt) {
		t.Fatalf("after the grace: status=%s canceled_at=%v, want canceled at %v", s.Status, s.CanceledAt, cancelAt)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Errorf("billing columns after the cancel = %s, want blank", cols)
	}
}
