package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestOverview_HomeCountsAndBaselines locks the numbers the Home page shows
// next to a trend, and the baselines those trends compare against.
//
// Seeded world (period=30d, t = window start, every customer is status
// active):
//
//	A  one active sub, started 60d ago            live now, live at t
//	B  one trialing sub, started 5d ago           live now (trialing), not at t
//	C  one sub started 60d ago, canceled 10d ago  not live now, live at t
//	D  no subscription                            never live
//	E  two active subs, started 60d ago           counted once, now and at t
//	F  one draft sub                              never live
//	G  one sub started 90d ago, canceled 40d ago  not live now, not at t
//	S  test-clock customer with a trialing sub    excluded everywhere
//
// Failed payments: one invoice created 10d ago (window), two created 40d
// ago (previous window), one simulated in the window.
//
// Mutation checks (each makes this test fail):
//   - trialing predicate back to status='active' AND trial_end_at > now() → 0
//   - live_customers without DISTINCT → 4
//   - live_customers_prev without the canceled_at check → G counts → 4
//   - live_customers_prev without the started_at bound → B counts → 4
//   - failed_payments_prev reading the current window → 1
func TestOverview_HomeCountsAndBaselines(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Analytics Home")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	daysAgo := func(d int) time.Time { return now.Add(-time.Duration(d) * 24 * time.Hour) }

	mkCust := func(ext, clockID string) string {
		t.Helper()
		c, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: ext, DisplayName: ext, TestClockID: clockID})
		if err != nil {
			t.Fatalf("customer %s: %v", ext, err)
		}
		execBypass(t, db, `UPDATE customers SET status = 'active' WHERE id = $1`, c.ID)
		return c.ID
	}
	mkSub := func(custID, code, status string, startedAt, activatedAt, canceledAt *time.Time, clockID string) {
		t.Helper()
		execTenant(t, db, tenantID, `
			INSERT INTO subscriptions (id, tenant_id, livemode, code, display_name, customer_id, status, billing_time,
				started_at, activated_at, canceled_at, test_clock_id,
				current_billing_period_start, current_billing_period_end, next_billing_at, created_at, updated_at)
			VALUES ($1, $2, false, $3, $3, $4, $5, 'anniversary', $6, $7, $8, NULLIF($9, ''), $10, $11, $11, $10, $10)`,
			postgres.NewID("vlx_sub"), tenantID, code, custID, status,
			startedAt, activatedAt, canceledAt, clockID, daysAgo(60), now.Add(24*time.Hour))
	}
	p := func(t time.Time) *time.Time { return &t }

	a := mkCust("cus_a", "")
	mkSub(a, "sub-a", "active", p(daysAgo(60)), p(daysAgo(60)), nil, "")
	b := mkCust("cus_b", "")
	mkSub(b, "sub-b", "trialing", p(daysAgo(5)), nil, nil, "")
	c := mkCust("cus_c", "")
	mkSub(c, "sub-c", "canceled", p(daysAgo(60)), p(daysAgo(60)), p(daysAgo(10)), "")
	_ = mkCust("cus_d", "")
	e := mkCust("cus_e", "")
	mkSub(e, "sub-e1", "active", p(daysAgo(60)), p(daysAgo(60)), nil, "")
	mkSub(e, "sub-e2", "active", p(daysAgo(60)), p(daysAgo(60)), nil, "")
	f := mkCust("cus_f", "")
	mkSub(f, "sub-f", "draft", nil, nil, nil, "")
	g := mkCust("cus_g", "")
	mkSub(g, "sub-g", "canceled", p(daysAgo(90)), p(daysAgo(90)), p(daysAgo(40)), "")
	clockID := seedClock(t, db, tenantID)
	s := mkCust("cus_s", clockID)
	mkSub(s, "sub-s", "trialing", p(daysAgo(5)), nil, nil, clockID)

	failed := func(custID string, cents int64, createdAt time.Time, simulated bool) {
		t.Helper()
		inv := makePaidInvoice(t, ctx, invStore, tenantID, custID, "USD", cents)
		execBypass(t, db, `UPDATE invoices SET status = 'finalized', payment_status = 'failed', paid_at = NULL,
			amount_paid_cents = 0, amount_due_cents = total_amount_cents, created_at = $2, is_simulated = $3 WHERE id = $1`,
			inv.ID, createdAt, simulated)
	}
	failed(a, 10_01, daysAgo(10), false)
	failed(a, 10_02, daysAgo(40), false)
	failed(e, 10_03, daysAgo(41), false)
	failed(s, 10_04, daysAgo(3), true)

	resp := getOverview(t, db, tenantID)

	if resp.TrialingSubs != 1 {
		t.Errorf("trialing_subscriptions: got %d, want 1 (status 'trialing'; the simulated trial is excluded)", resp.TrialingSubs)
	}
	if resp.ActiveSubs != 3 {
		t.Errorf("active_subscriptions: got %d, want 3", resp.ActiveSubs)
	}
	if resp.ActiveCustomers != 7 {
		t.Errorf("active_customers: got %d, want 7 (customer records with status active, unchanged meaning)", resp.ActiveCustomers)
	}
	if resp.LiveCustomers != 3 {
		t.Errorf("live_customers: got %d, want 3 (A, B, E: an active or trialing sub, E counted once)", resp.LiveCustomers)
	}
	if resp.LiveCustomersPrev != 3 {
		t.Errorf("live_customers_prev: got %d, want 3 (A, C, E were live 30 days ago; B started after)", resp.LiveCustomersPrev)
	}
	if resp.FailedPayments != 1 {
		t.Errorf("failed_payments: got %d, want 1", resp.FailedPayments)
	}
	if resp.FailedPaymentsPrev != 2 {
		t.Errorf("failed_payments_prev: got %d, want 2 (previous 30-day window)", resp.FailedPaymentsPrev)
	}
}

// execTenant runs one statement in a test-mode tenant tx, so livemode and
// the RLS tenant are set the way the app sets them.
func execTenant(t *testing.T, db *postgres.DB, tenantID, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
