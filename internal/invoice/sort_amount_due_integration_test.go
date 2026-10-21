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

// TestList_SortByAmountDueOrdersAVoidAsNothingDue covers the review of #1328:
// the Invoices list's "Amount due" column shows $0.00 for a void (a void owes
// nothing; amount_due survives it), but sorting by that column ordered the
// void by its surviving amount_due, so a $29 void sat between the $30 and $20
// open invoices it read below.
//
// Mutation check: invoiceSortColumn back to the raw amount_due_cents puts the
// void first on a descending sort.
func TestList_SortByAmountDueOrdersAVoidAsNothingDue(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sort Amount Due")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sort_due", DisplayName: "Sort Due"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	mk := func(number string, status domain.InvoiceStatus, due int64) {
		t.Helper()
		if _, err := store.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: number, Status: status, PaymentStatus: domain.PaymentPending,
			Currency: "USD", SubtotalCents: due, TotalAmountCents: due, AmountDueCents: due,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		}); err != nil {
			t.Fatalf("create %s: %v", number, err)
		}
	}
	mk("INV-SORT-30", domain.InvoiceFinalized, 30_00)
	mk("INV-SORT-VOID", domain.InvoiceVoided, 29_00)
	mk("INV-SORT-20", domain.InvoiceFinalized, 20_00)

	got, _, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID, Sort: "amount_due_cents", SortDir: "desc"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var order []string
	for _, inv := range got {
		order = append(order, inv.InvoiceNumber)
	}
	want := []string{"INV-SORT-30", "INV-SORT-20", "INV-SORT-VOID"}
	if len(order) != len(want) {
		t.Fatalf("order: got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order: got %v, want %v (a void sorts as $0.00 due, as the column shows it)", order, want)
		}
	}
}
