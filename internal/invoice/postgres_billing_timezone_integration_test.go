package invoice_test

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

// TestPostgresStore_BillingTimezone_RoundTrips proves the ADR-074 invoice
// snapshot column persists through the INSERT and scans back into the struct.
// The in-memory store echoes any field set on the struct, so only a REAL
// round-trip catches a column dropped from the INSERT or a scanInvDest ordinal
// that drifted out of sync with invCols (the Finding-1 activated_at trap).
//
// Mutation-verify: drop `billing_timezone` from the INSERT column list (or its
// $N value) — Get returns an empty BillingTimezone and this fails.
func TestPostgresStore_BillingTimezone_RoundTrips(t *testing.T) {
	db := testutil.SetupTestDB(t) // skips on -short
	ctx := postgres.WithLivemode(context.Background(), false)

	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)

	tenantID := testutil.CreateTestTenant(t, db, "TZ Invoice Corp")
	cust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_tz", DisplayName: "TZ Cust"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Half-open [May 2 00:00 IST, Jun 1 00:00 IST), anchored in Asia/Kolkata.
	start := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 31, 18, 30, 0, 0, time.UTC)

	created, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "HC-TZ-1",
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		BillingPeriodStart: start,
		BillingPeriodEnd:   end,
		BillingTimezone:    "Asia/Kolkata",
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if created.BillingTimezone != "Asia/Kolkata" {
		t.Errorf("Create returned BillingTimezone %q, want Asia/Kolkata", created.BillingTimezone)
	}

	got, err := invStore.Get(ctx, tenantID, created.ID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if got.BillingTimezone != "Asia/Kolkata" {
		t.Errorf("round-trip BillingTimezone: got %q, want Asia/Kolkata (dropped INSERT column or scanInvDest/invCols misalignment)", got.BillingTimezone)
	}
	// Neighbouring columns must not have shifted from the appended column.
	if got.InvoiceNumber != "HC-TZ-1" || got.Currency != "USD" {
		t.Errorf("neighbouring fields shifted after adding billing_timezone: number=%q currency=%q", got.InvoiceNumber, got.Currency)
	}

	// An ad-hoc invoice with no snapshot persists an empty string (NULL → '' via
	// the COALESCE in invCols), so the read path falls back to the live tenant TZ.
	adhoc, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:    cust.ID,
		InvoiceNumber: "HC-TZ-ADHOC",
		Status:        domain.InvoiceDraft,
		PaymentStatus: domain.PaymentPending,
		Currency:      "USD",
	})
	if err != nil {
		t.Fatalf("create ad-hoc invoice: %v", err)
	}
	if adhoc.BillingTimezone != "" {
		t.Errorf("ad-hoc invoice BillingTimezone: got %q, want empty", adhoc.BillingTimezone)
	}
}
