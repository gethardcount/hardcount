package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// An issued credit note whose refund leg ran but whose outcome was never
// recorded (refund_status 'none' with a refund amount) must reach the
// needs-attention list once it is over an hour old, so the operator retries
// the refund (RetryRefund adopts the refund Stripe minted). Fresh rows and
// credit-only rows stay out.
func TestNeedsAttention_UnrecordedRefundOutcome(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN needs attention")
	cnStore := creditnote.NewPostgresStore(db)
	now := time.Now().UTC()

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_na", DisplayName: "NA"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	mk := func(num string, refund int64, age time.Duration) string {
		inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: "INV-" + num,
			Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountPaidCents: 5000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		cn, err := cnStore.CreateUnderInvoiceLock(ctx, tenantID, inv.ID, nil, func([]domain.CreditNote) (domain.CreditNote, error) {
			return domain.CreditNote{
				InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-" + num,
				Status: domain.CreditNoteIssued, Reason: "fixture",
				SubtotalCents: 1000, TotalCents: 1000,
				RefundAmountCents: refund, CreditAmountCents: 1000 - refund,
				Currency: "USD", RefundStatus: domain.RefundNone,
			}, nil
		})
		if err != nil {
			t.Fatalf("create credit note: %v", err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE credit_notes SET updated_at = $1 WHERE id = $2`, now.Add(-age), cn.ID)
		if err != nil {
			t.Fatalf("age credit note: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("aged %d rows, want 1", n)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return cn.ID
	}
	stuck := mk("STUCK", 1000, 2*time.Hour)
	mk("FRESH", 1000, time.Minute)    // the refund leg may still be running
	mk("CREDITONLY", 0, 48*time.Hour) // no refund leg exists

	got, err := cnStore.List(ctx, creditnote.ListFilter{TenantID: tenantID, RefundStatus: "needs_attention"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != stuck {
		ids := make([]string, len(got))
		for i, c := range got {
			ids[i] = c.CreditNoteNumber
		}
		t.Fatalf("needs attention = %v, want only CN-STUCK", ids)
	}
}
