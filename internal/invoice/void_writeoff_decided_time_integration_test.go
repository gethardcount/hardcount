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

// A void and a write-off on a clock invoice stamp voided_at and
// uncollectible_at at the time their caller decided. The API's door decides
// both routes from the invoice: here its clock at an instant before the
// clock's frozen time, as a request decided before an advance would carry.
// At real time (an invoice on no clock) they stamp the server's clock, read
// once, not the machine's.
func TestVoidAndWriteOff_StampTheDecidedTime(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Void Write-off Decided Time")
	frozen := time.Date(2027, 2, 10, 9, 0, 0, 0, time.UTC)
	decidedAt := frozen.Add(-72 * time.Hour)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "void-writeoff-decided", DisplayName: "Void Write-off Decided"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	clockID := postgres.NewID("hc_tclk")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, 'decided clock', $3, 'ready')`, []any{clockID, tenantID, frozen}},
		{`UPDATE customers SET test_clock_id = $1 WHERE id = $2`, []any{clockID, cust.ID}},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			postgres.Rollback(tx)
			t.Fatalf("pin to the clock: %s: %v", q.sql, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	svc := invoice.NewService(invStore, clock.Real(), tenant.NewSettingsStore(db))
	decided := clock.WithSim(ctx, clock.Sim{At: decidedAt, TestClockID: clockID})
	newInvoice := func(t *testing.T, number string) domain.Invoice {
		t.Helper()
		inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: number, IsSimulated: true,
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
			t.Errorf("status %s, voided_at %v; want voided at %v (not the clock's frozen %v)", got.Status, got.VoidedAt, decidedAt, frozen)
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
			t.Errorf("status %s, uncollectible_at %v; want uncollectible at %v (not the clock's frozen %v)", got.Status, got.UncollectibleAt, decidedAt, frozen)
		}
	})

	// At real time: the server's clock at a fixed past instant.
	serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	realSvc := invoice.NewService(invStore, clock.NewFake(serverAt), tenant.NewSettingsStore(db))
	realCust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "void-writeoff-real-time", DisplayName: "Void Write-off Real Time"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	realInvoice := func(t *testing.T, number string) domain.Invoice {
		t.Helper()
		inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: realCust.ID, InvoiceNumber: number,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: serverAt.Add(-30 * 24 * time.Hour), BillingPeriodEnd: serverAt,
		}, nil)
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		return inv
	}
	realTime := clock.WithRealTime(ctx)

	t.Run("a void at real time is stamped at the server's time", func(t *testing.T) {
		inv := realInvoice(t, "INV-VOID-REAL")
		if _, err := realSvc.Void(realTime, tenantID, inv.ID); err != nil {
			t.Fatalf("void: %v", err)
		}
		got, err := invStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.VoidedAt == nil || !got.VoidedAt.Equal(serverAt) {
			t.Errorf("voided_at %v; want the server's %v", got.VoidedAt, serverAt)
		}
	})

	t.Run("a write-off at real time is stamped at the server's time", func(t *testing.T) {
		inv := realInvoice(t, "INV-OFF-REAL")
		if _, err := realSvc.MarkUncollectible(realTime, tenantID, inv.ID); err != nil {
			t.Fatalf("mark uncollectible: %v", err)
		}
		got, err := invStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.UncollectibleAt == nil || !got.UncollectibleAt.Equal(serverAt) {
			t.Errorf("uncollectible_at %v; want the server's %v", got.UncollectibleAt, serverAt)
		}
	})
}
