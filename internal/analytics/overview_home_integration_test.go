package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestOverview_HomeCountsAndBaselines locks the failed-payment count and the
// baseline its trend compares against, and the active-customer count.
//
// Seeded world (period=30d): three customers with status active. Failed
// payments: one invoice created 10d ago (window), two created 40d ago
// (previous window).
//
// Mutation check: failed_payments_prev reading the current window → 1.
func TestOverview_HomeCountsAndBaselines(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Analytics Home")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	daysAgo := func(d int) time.Time { return now.Add(-time.Duration(d) * 24 * time.Hour) }

	mkCust := func(ext string) string {
		t.Helper()
		c, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: ext, DisplayName: ext})
		if err != nil {
			t.Fatalf("customer %s: %v", ext, err)
		}
		execBypass(t, db, `UPDATE customers SET status = 'active' WHERE id = $1`, c.ID)
		return c.ID
	}
	a := mkCust("cus_a")
	e := mkCust("cus_e")
	_ = mkCust("cus_d")

	failed := func(custID string, cents int64, createdAt time.Time) {
		t.Helper()
		inv := makePaidInvoice(t, ctx, invStore, tenantID, custID, "USD", cents)
		execBypass(t, db, `UPDATE invoices SET status = 'finalized', payment_status = 'failed', paid_at = NULL,
			amount_paid_cents = 0, amount_due_cents = total_amount_cents, created_at = $2 WHERE id = $1`,
			inv.ID, createdAt)
	}
	failed(a, 10_01, daysAgo(10))
	failed(a, 10_02, daysAgo(40))
	failed(e, 10_03, daysAgo(41))

	resp := getOverview(t, db, tenantID)

	if resp.ActiveCustomers != 3 {
		t.Errorf("active_customers: got %d, want 3 (customer records with status active)", resp.ActiveCustomers)
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
