package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A credit note carries two stamps: created_at, the instant its work bound,
// and recorded_at, the database's time of the INSERT, which the API returns.
// A bound instant set apart from real time tells the two apart.
func TestCreditNote_RecordedAt_DualStamp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "CN recorded_at")
	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	cnStore := creditnote.NewPostgresStore(db)

	cust, err := custStore.Create(base, tenantID, domain.Customer{ExternalID: "rec-cus", DisplayName: "Rec Cus"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := invStore.CreateWithLineItems(base, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "HC-REC-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &now,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	at := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)

	cn, err := cnStore.Create(clock.WithEffectiveNow(base, at), tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-REC-1",
		Status: domain.CreditNoteDraft, Reason: "billing error",
		SubtotalCents: 100, TotalCents: 100, CreditAmountCents: 100,
		Currency: "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create cn: %v", err)
	}

	if !cn.CreatedAt.Equal(at) {
		t.Errorf("created_at = %s, want the bound instant %s", cn.CreatedAt, at)
	}
	if cn.RecordedAt == nil {
		t.Fatal("recorded_at is nil; want the INSERT's time")
	}
	if cn.RecordedAt.Equal(at) {
		t.Errorf("recorded_at = the bound instant; want the INSERT's time")
	}
	if d := time.Since(*cn.RecordedAt); d < 0 || d > 2*time.Minute {
		t.Errorf("recorded_at = %s, not within the test's real-time window (drift %s)", cn.RecordedAt, d)
	}

	// Read path returns it too (List projection).
	list, err := cnStore.List(base, creditnote.ListFilter{TenantID: tenantID, InvoiceID: inv.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].RecordedAt == nil {
		t.Fatalf("List dropped recorded_at (rows=%d)", len(list))
	}
}
