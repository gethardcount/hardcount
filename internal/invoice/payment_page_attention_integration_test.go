package invoice_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The invoice page's banner reads the live payment page through the same
// HostedCheckoutLiveSQL rule the charge claims refuse on: open page → the
// page-open banner (no Retry); page lapsed → the normal banner again. Real
// store, real service read path.
func TestGet_PaymentPageOpenBanner_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Payment Page Banner")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-PAGE-OPEN")
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, tenant.NewSettingsStore(db))
	svc.SetPaymentPageReader(store)

	got, err := svc.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Attention != nil && got.Attention.Code == "payment.page_open" {
		t.Fatalf("no page yet: got the page-open banner")
	}

	// An unfilled claim minted just now: the page link may be reaching the
	// customer, so it holds collection.
	sessID := postgres.NewID("hc_cks")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status)
		VALUES ($1, $2, $3, false, 5000, 'USD', 'open')`, sessID, tenantID, inv.ID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err = svc.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Attention == nil || got.Attention.Code != "payment.page_open" || len(got.Attention.Actions) != 0 {
		t.Fatalf("page open: attention = %+v, want payment.page_open with no actions", got.Attention)
	}

	// The page lapses (the claim is past its window): the banner returns to
	// the invoice's own state.
	tx, err = db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE checkout_sessions SET created_at = now() - interval '2 hours' WHERE id = $1`, sessID); err != nil {
		t.Fatalf("age session: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err = svc.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Attention != nil && got.Attention.Code == "payment.page_open" {
		t.Fatalf("page lapsed: still showing the page-open banner")
	}
}
