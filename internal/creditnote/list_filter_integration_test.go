package creditnote_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestList_CustomerIDFilter locks the #559 rider: ListFilter.CustomerID
// existed on the struct but the store never built its predicate, so a
// customer-scoped query silently returned the whole tenant's credit notes.
func TestList_CustomerIDFilter(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "CN customer filter")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	cnStore := creditnote.NewPostgresStore(db)

	now := time.Now().UTC()
	mkCN := func(ext, invNum, cnNum string) (customerID string) {
		cust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: ext, DisplayName: ext})
		if err != nil {
			t.Fatalf("create customer %s: %v", ext, err)
		}
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID:         cust.ID,
			InvoiceNumber:      invNum,
			Status:             domain.InvoiceFinalized,
			PaymentStatus:      domain.PaymentPending,
			Currency:           "USD",
			SubtotalCents:      5000,
			TotalAmountCents:   5000,
			AmountDueCents:     5000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   now,
			IssuedAt:           &now,
		})
		if err != nil {
			t.Fatalf("create invoice %s: %v", invNum, err)
		}
		_, err = cnStore.CreateUnderInvoiceLock(ctx, tenantID, inv.ID, nil,
			func(*sql.Tx, []domain.CreditNote) (domain.CreditNote, error) {
				return domain.CreditNote{
					InvoiceID:         inv.ID,
					CustomerID:        cust.ID,
					CreditNoteNumber:  cnNum,
					Status:            domain.CreditNoteIssued,
					Reason:            "filter fixture",
					SubtotalCents:     1000,
					TotalCents:        1000,
					CreditAmountCents: 1000,
					Currency:          "USD",
					RefundStatus:      domain.RefundNone,
				}, nil
			})
		if err != nil {
			t.Fatalf("create CN %s: %v", cnNum, err)
		}
		return cust.ID
	}

	cusA := mkCN("cus-filter-a", "INV-CNF-A", "CN-FLT-A")
	cusB := mkCN("cus-filter-b", "INV-CNF-B", "CN-FLT-B")

	scoped, err := cnStore.List(ctx, creditnote.ListFilter{TenantID: tenantID, CustomerID: cusA})
	if err != nil {
		t.Fatalf("scoped list: %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("customer-scoped list returned %d rows, want exactly 1 — the filter was silently ignored", len(scoped))
	}
	if scoped[0].CustomerID != cusA {
		t.Errorf("scoped row belongs to %s, want %s", scoped[0].CustomerID, cusA)
	}

	all, err := cnStore.List(ctx, creditnote.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("unscoped list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unscoped list returned %d rows, want 2", len(all))
	}
	_ = cusB
}
