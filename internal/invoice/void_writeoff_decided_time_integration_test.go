package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A void and a write-off stamp voided_at and uncollectible_at at the instant
// their caller bound, not at a later read of the server's clock: the API
// binds one instant for each request.
func TestVoidAndWriteOff_StampTheDecidedTime(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Void Write-off Decided Time")
	decidedAt := time.Date(2027, 2, 7, 9, 0, 0, 0, time.UTC)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "void-writeoff-decided", DisplayName: "Void Write-off Decided"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	invStore := invoice.NewPostgresStore(db)
	svc := invoice.NewService(invStore, clock.Real(), tenant.NewSettingsStore(db))
	decided := clock.WithEffectiveNow(ctx, decidedAt)
	newInvoice := func(t *testing.T, number string) domain.Invoice {
		t.Helper()
		inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: number,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: decidedAt.Add(-30 * 24 * time.Hour), BillingPeriodEnd: decidedAt,
		}, nil)
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		return inv
	}

	t.Run("a void is stamped at the decided time", func(t *testing.T) {
		inv := newInvoice(t, "INV-VOID-DECIDED")
		if _, err := svc.Void(decided, tenantID, inv.ID); err != nil {
			t.Fatalf("void: %v", err)
		}
		got, err := invStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != domain.InvoiceVoided || got.VoidedAt == nil || !got.VoidedAt.Equal(decidedAt) {
			t.Errorf("status %s, voided_at %v; want voided at %v, the bound instant", got.Status, got.VoidedAt, decidedAt)
		}
	})

	t.Run("a write-off is stamped at the decided time", func(t *testing.T) {
		inv := newInvoice(t, "INV-OFF-DECIDED")
		if _, err := svc.MarkUncollectible(decided, tenantID, inv.ID); err != nil {
			t.Fatalf("mark uncollectible: %v", err)
		}
		got, err := invStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != domain.InvoiceUncollectible || got.UncollectibleAt == nil || !got.UncollectibleAt.Equal(decidedAt) {
			t.Errorf("status %s, uncollectible_at %v; want uncollectible at %v, the bound instant", got.Status, got.UncollectibleAt, decidedAt)
		}
	})
}
