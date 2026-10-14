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

// TestGetOutstandingBalance_ByCurrency: the customer page showed USD invoices'
// outstanding total in the workspace default (CA$739.00) because total_cents
// adds every currency together. by_currency gives one amount per currency,
// largest first, over the same invoices the count covers.
func TestGetOutstandingBalance_ByCurrency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Outstanding By Currency")
	store := invoice.NewPostgresStore(db)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_obc", DisplayName: "OBC"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	mk := func(num, currency string, due int64, status domain.InvoiceStatus, pay domain.InvoicePaymentStatus) {
		t.Helper()
		now := time.Now().UTC()
		if _, err := store.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: status, PaymentStatus: pay,
			Currency: currency, SubtotalCents: due, TotalAmountCents: due, AmountDueCents: due,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		}); err != nil {
			t.Fatalf("create %s: %v", num, err)
		}
	}
	mk("INV-OBC-1", "USD", 120_00, domain.InvoiceFinalized, domain.PaymentPending)
	mk("INV-OBC-2", "USD", 619_00, domain.InvoiceFinalized, domain.PaymentFailed)
	mk("INV-OBC-3", "EUR", 49_00, domain.InvoiceFinalized, domain.PaymentPending)
	mk("INV-OBC-4", "GBP", 999_00, domain.InvoiceUncollectible, domain.PaymentFailed) // excluded
	mk("INV-OBC-5", "CAD", 300_00, domain.InvoiceFinalized, domain.PaymentSucceeded)  // excluded

	got, err := store.GetOutstandingBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("outstanding: %v", err)
	}
	if got.UnpaidCount != 3 {
		t.Errorf("unpaid_count: got %d, want 3", got.UnpaidCount)
	}
	if got.TotalCents != 788_00 {
		t.Errorf("total_cents: got %d, want 78800 (kept as the all-currency sum for API callers)", got.TotalCents)
	}
	want := []invoice.CurrencyAmount{
		{Currency: "USD", AmountCents: 739_00},
		{Currency: "EUR", AmountCents: 49_00},
	}
	if len(got.ByCurrency) != len(want) {
		t.Fatalf("by_currency: got %+v, want %+v", got.ByCurrency, want)
	}
	for i := range want {
		if got.ByCurrency[i] != want[i] {
			t.Errorf("by_currency[%d]: got %+v, want %+v", i, got.ByCurrency[i], want[i])
		}
	}
}
