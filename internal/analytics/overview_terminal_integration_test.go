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

// TestOverview_ClosedInvoicesLeaveFailedPayments: a void or write-off keeps
// the invoice's last payment_status, and is no longer collected, so it is
// not a failed payment.
//
// Seeded world, all in the 30-day window: invoices with payment_status
// failed that are open (counts), voided and written off.
//
// Mutation check: failed_payments without status = 'finalized' → 3.
func TestOverview_ClosedInvoicesLeaveFailedPayments(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Analytics Terminal")
	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()

	c, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_term", DisplayName: "Term"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}

	// An invoice in the given state whose last payment failed 10 days ago.
	failed := func(cents int64, status domain.InvoiceStatus) {
		t.Helper()
		inv := makePaidInvoice(t, ctx, invStore, tenantID, c.ID, "USD", cents)
		execBypass(t, db, `UPDATE invoices SET status = $2, payment_status = 'failed', paid_at = NULL,
			amount_paid_cents = 0, amount_due_cents = total_amount_cents, created_at = $3 WHERE id = $1`,
			inv.ID, string(status), now.Add(-10*24*time.Hour))
	}
	failed(10_01, domain.InvoiceFinalized)
	failed(10_02, domain.InvoiceVoided)
	failed(10_03, domain.InvoiceUncollectible)

	resp := getOverview(t, db, tenantID)

	if resp.FailedPayments != 1 {
		t.Errorf("failed_payments: got %d, want 1 (the open invoice; a void or write-off is no longer collected)", resp.FailedPayments)
	}
}
