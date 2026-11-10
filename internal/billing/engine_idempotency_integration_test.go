package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/tenant"

	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// cycleRig is one tenant with a 4900c monthly in_arrears plan, a
// subscription whose period starts at periodStart, and a real engine on a
// fake clock.
type cycleRig struct {
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	custID   string
	subID    string
	invoices *invoice.PostgresStore
	clk      *clock.Fake
	engine   *billing.Engine
}

func newCycleRig(t *testing.T, name string, periodStart, now time.Time) *cycleRig {
	t.Helper()
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling, leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_cycle", DisplayName: "Cycle Customer"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "flat", Name: "Flat Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 4900,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	periodEnd := periodStart.AddDate(0, 1, 0)
	subStore := subscription.NewPostgresStore(db)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-cycle", DisplayName: "Cycle Sub", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	clk := clock.NewFake(now)
	return &cycleRig{db: db, ctx: ctx, tenantID: tenantID, custID: cust.ID, subID: sub.ID,
		invoices: invoice.NewPostgresStore(db), clk: clk, engine: newRealEngine(db, clk)}
}

// period returns the subscription's period start and its billing failure count.
func (r *cycleRig) period(t *testing.T) (time.Time, int) {
	t.Helper()
	var start time.Time
	var failures int
	if err := testutil.AdminPool(t).QueryRowContext(r.ctx,
		`SELECT current_billing_period_start, billing_failure_count FROM subscriptions WHERE id = $1`, r.subID).Scan(&start, &failures); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	return start, failures
}

func (r *cycleRig) cycleInvoices(t *testing.T) int {
	t.Helper()
	invs, _, err := r.invoices.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID, SubscriptionID: r.subID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	n := 0
	for _, inv := range invs {
		if inv.BillingReason == domain.BillingReasonSubscriptionCycle {
			n++
		}
	}
	return n
}

// takeNextInvoiceNumber gives the tenant's next invoice number to an unrelated
// invoice and sets the counter back, so the next invoice the engine numbers
// collides with it: the counter set back out of band. The returned func
// repairs the counter.
func takeNextInvoiceNumber(t *testing.T, ctx context.Context, db *postgres.DB, invoices *invoice.PostgresStore, tenantID, customerID string) (repair func()) {
	t.Helper()
	num, err := tenant.NewSettingsStore(db).NextInvoiceNumber(ctx, tenantID)
	if err != nil {
		t.Fatalf("take a number: %v", err)
	}
	now := time.Now().UTC()
	if _, err := invoices.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: customerID, InvoiceNumber: num, Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		BillingReason: domain.BillingReasonManual, Currency: "USD", BillingPeriodStart: now, BillingPeriodEnd: now,
	}, nil); err != nil {
		t.Fatalf("create the unrelated invoice: %v", err)
	}
	admin := testutil.AdminPool(t)
	move := func(delta int) {
		if _, err := admin.ExecContext(ctx, `UPDATE tenant_settings SET invoice_next_seq_test = invoice_next_seq_test + $2 WHERE tenant_id = $1`, tenantID, delta); err != nil {
			t.Fatalf("move the invoice counter: %v", err)
		}
	}
	move(-1)
	return func() { move(1) }
}

// TestCycleClose_InvoiceNumberCollision_FailsBillKeepsPeriod: three periods
// are overdue and the invoice counter was set back, so the first close's
// number is taken. The close fails with invoice_number_taken, the period
// stays, and the run saves a counted failure. Before, it read the violation as
// "already invoiced" and closed all three periods with no invoice. Once the
// counter is repaired, the retry after the first wait bills all three.
//
// Mutation: restore the catch for any ErrAlreadyExists, and the run returns no
// error, the period moves three ahead, and no invoice is written.
func TestCycleClose_InvoiceNumberCollision_FailsBillKeepsPeriod(t *testing.T) {
	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := newCycleRig(t, "Number Collision Corp", periodStart, time.Date(2026, 4, 1, 0, 0, 0, 1, time.UTC))
	repair := takeNextInvoiceNumber(t, r.ctx, r.db, r.invoices, r.tenantID, r.custID)

	count, runErrs := r.engine.RunCycle(r.ctx, 50, nil)
	if count != 0 || len(runErrs) != 1 || errs.Code(runErrs[0]) != "invoice_number_taken" {
		t.Fatalf("run = %d invoices, errors %v; want 0 and one with code invoice_number_taken", count, runErrs)
	}
	if start, failures := r.period(t); !start.Equal(periodStart) || failures != 1 {
		t.Errorf("period start = %v, billing failures = %d; want %v (unchanged) and 1", start, failures, periodStart)
	}
	if n := r.cycleInvoices(t); n != 0 {
		t.Errorf("cycle invoices = %d, want 0", n)
	}

	repair()
	r.clk.Advance(16 * time.Minute) // past the first retry wait (ADR-119)
	count, runErrs = r.engine.RunCycle(r.ctx, 50, nil)
	if len(runErrs) != 0 || count != 3 {
		t.Fatalf("retry = %d invoices, errors %v; want 3 and none", count, runErrs)
	}
	if n := r.cycleInvoices(t); n != 3 {
		t.Errorf("cycle invoices = %d, want 3", n)
	}
}

// TestBilling_PeriodSlotTaken_FailsBillKeepsPeriod: the period is billed, then
// set back in the database so the run bills it again. Its invoice's key is
// taken by the first invoice: the close fails with
// invoice_billing_period_taken, the period stays, and the run saves a counted
// failure. Before, it read the violation as "already invoiced" and moved the
// period on with no invoice. (A racing closer never reaches the index: it
// misses its period CAS first; see TestConcurrentBilling_ExactlyOneInvoice.)
//
// Mutation: restore the catch for the period code, and run 2 returns no error
// and advances.
func TestBilling_PeriodSlotTaken_FailsBillKeepsPeriod(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	r := newCycleRig(t, "Period Slot Corp", periodStart, periodEnd.Add(time.Nanosecond))

	if count, runErrs := r.engine.RunCycle(r.ctx, 50, nil); count != 1 || len(runErrs) != 0 {
		t.Fatalf("run 1 = %d invoices, errors %v; want 1, none", count, runErrs)
	}
	subscriptiontest.SetBillingCycle(t, r.ctx, r.db, r.tenantID, r.subID, periodStart, periodEnd, periodEnd, 0)

	count, runErrs := r.engine.RunCycle(r.ctx, 50, nil)
	if count != 0 || len(runErrs) != 1 || errs.Code(runErrs[0]) != errs.CodeInvoiceBillingPeriodTaken {
		t.Fatalf("run 2 = %d invoices, errors %v; want 0 and one with code %s", count, runErrs, errs.CodeInvoiceBillingPeriodTaken)
	}
	if start, failures := r.period(t); !start.Equal(periodStart) || failures != 1 {
		t.Errorf("period start = %v, billing failures = %d; want %v (unchanged) and 1", start, failures, periodStart)
	}
	if n := r.cycleInvoices(t); n != 1 {
		t.Errorf("cycle invoices = %d, want 1", n)
	}
}
