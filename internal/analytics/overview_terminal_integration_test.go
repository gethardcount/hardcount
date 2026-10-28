package analytics

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestOverview_ClosedInvoicesLeaveFailedPaymentsAndRecovery covers sweep A7/E1
// and E7b. A void or write-off keeps the invoice's last payment_status, so
// Home's "N failed payments" alert counted invoices nobody is collecting any
// more; and the dunning recovery rate counted every resolved run as
// recovered, so a run closed by a void raised it. Review of #1328: a credit
// note that leaves nothing due closes the run as payment_recovered on an
// invoice paid with nothing paid for it, and raised it too.
//
// Seeded world, all in the 30-day window:
//
//	invoices with payment_status failed: open (counts), voided, written off
//	dunning runs opened 5 days ago:      resolved by a payment, resolved by
//	                                     a void, resolved by a waiver (paid,
//	                                     amount_paid 0), still active
//
// Mutation checks (each makes this test fail):
//   - failed_payments without status = 'finalized' → 3
//   - the recovery numerator back to state = 'resolved' alone → 3/4
//   - the numerator without amount_paid_cents > 0 → 2/4
func TestOverview_ClosedInvoicesLeaveFailedPaymentsAndRecovery(t *testing.T) {
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
	failed := func(cents int64, status domain.InvoiceStatus) string {
		t.Helper()
		inv := makePaidInvoice(t, ctx, invStore, tenantID, c.ID, "USD", cents)
		execBypass(t, db, `UPDATE invoices SET status = $2, payment_status = 'failed', paid_at = NULL,
			amount_paid_cents = 0, amount_due_cents = total_amount_cents, created_at = $3 WHERE id = $1`,
			inv.ID, string(status), now.Add(-10*24*time.Hour))
		return inv.ID
	}
	open := failed(10_01, domain.InvoiceFinalized)
	voided := failed(10_02, domain.InvoiceVoided)
	_ = failed(10_03, domain.InvoiceUncollectible)
	recovered := makePaidInvoice(t, ctx, invStore, tenantID, c.ID, "USD", 10_04).ID
	waived := makePaidInvoice(t, ctx, invStore, tenantID, c.ID, "USD", 10_05).ID
	execBypass(t, db, `UPDATE invoices SET amount_paid_cents = 0 WHERE id = $1`, waived)

	policyID := postgres.NewID("hc_dpol")
	// A tenant tx, so the set_livemode trigger stamps the test mode the
	// overview reads.
	execTenant(t, db, tenantID, `INSERT INTO dunning_policies (id, tenant_id, name, retry_days) VALUES ($1, $2, 'p', '[3, 3, 3]')`, policyID, tenantID)
	run := func(invoiceID, state, resolution string) {
		t.Helper()
		execTenant(t, db, tenantID, `INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, resolution, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)`,
			postgres.NewID("hc_drun"), tenantID, invoiceID, c.ID, policyID, state, resolution, now.Add(-5*24*time.Hour))
	}
	run(recovered, "resolved", "payment_recovered")
	run(voided, "resolved", "invoice_voided")
	run(waived, "resolved", "payment_recovered")
	run(open, "active", "")

	resp := getOverview(t, db, tenantID)

	if resp.FailedPayments != 1 {
		t.Errorf("failed_payments: got %d, want 1 (the open invoice; a void or write-off is no longer collected)", resp.FailedPayments)
	}
	if want := 1.0 / 4.0; math.Abs(resp.DunningRecoveryRate-want) > 1e-9 {
		t.Errorf("dunning_recovery_rate: got %v, want 1/4 (only the run a payment closed is a recovery; a void or a waiver is not)", resp.DunningRecoveryRate)
	}
}
