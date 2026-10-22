package invoice_test

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

// TestList_SortByAmountOrdersByTheFigureShown covers the sign-off of #1328:
// the Invoices list's "Amount" column shows a void at the amount it was voided
// at (amount_due survives a void) and a paid invoice at what paid it, payment
// plus customer credit (the settle zeroed amount_due). The sort must order
// rows by those figures, or a $25 paid invoice sorts as $0 below a $20 open
// one.
//
// Mutation check: invoiceSortColumn back to the raw amount_due_cents puts
// INV-SORT-PAID last on a descending sort.
func TestList_SortByAmountOrdersByTheFigureShown(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sort Amount")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sort_amount", DisplayName: "Sort Amount"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	mk := func(number string, status domain.InvoiceStatus, total, due, paid, credits int64) {
		t.Helper()
		if _, err := store.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: number, Status: status, PaymentStatus: domain.PaymentPending,
			Currency: "USD", SubtotalCents: total, TotalAmountCents: total, AmountDueCents: due,
			AmountPaidCents: paid, CreditsAppliedCents: credits,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		}); err != nil {
			t.Fatalf("create %s: %v", number, err)
		}
	}
	mk("INV-SORT-30", domain.InvoiceFinalized, 30_00, 30_00, 0, 0)
	mk("INV-SORT-VOID", domain.InvoiceVoided, 29_00, 29_00, 0, 0)
	// Paid $15 by card and $10 by customer credit; the settle zeroed amount_due.
	mk("INV-SORT-PAID", domain.InvoicePaid, 25_00, 0, 15_00, 10_00)
	mk("INV-SORT-20", domain.InvoiceFinalized, 20_00, 20_00, 0, 0)

	got, _, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID, Sort: "amount_due_cents", SortDir: "desc"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var order []string
	for _, inv := range got {
		order = append(order, inv.InvoiceNumber)
	}
	want := []string{"INV-SORT-30", "INV-SORT-VOID", "INV-SORT-PAID", "INV-SORT-20"}
	if len(order) != len(want) {
		t.Fatalf("order: got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order: got %v, want %v (rows sort by the amount the column shows)", order, want)
		}
	}
}
