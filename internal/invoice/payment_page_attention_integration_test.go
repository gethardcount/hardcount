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
	svc := invoice.NewService(store, nil, nil)
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
	sessID := postgres.NewID("vlx_cks")
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

// A clock-pinned invoice's failure banner reads its latest failed charge's
// simulated instant off invoice_charge_attempts, not its issue instant.
func TestGet_SimulatedFailureBannerDatesFromTheCharge_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Sim Failure Since")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sim_since", DisplayName: "Sim Since"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	issued := time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC)
	declined := time.Date(2028, 2, 10, 9, 0, 0, 0, time.UTC)
	retried := time.Date(2028, 2, 13, 9, 0, 0, 0, time.UTC)
	store := invoice.NewPostgresStore(db)
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-SIM-SINCE",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		IssuedAt: &issued, IsSimulated: true,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	for _, at := range []time.Time{declined, retried} {
		at := at
		if err := store.RecordChargeAttempt(ctx, tenantID, domain.InvoiceChargeAttempt{
			InvoiceID: inv.ID, StripePaymentIntentID: "pi_" + at.Format("0102"),
			Trigger: domain.ChargeTriggerAutoCharge, Outcome: domain.ChargeAttemptFailed,
			ProviderReason: "Your card was declined.", AmountCents: 5000,
			OccurredAt: time.Now().UTC(), SimEffectiveAt: &at,
		}); err != nil {
			t.Fatalf("record attempt: %v", err)
		}
	}

	got, err := invoice.NewService(store, nil, nil).Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Attention == nil || got.Attention.Since == nil || !got.Attention.Since.Equal(retried) {
		t.Fatalf("since = %+v, want the latest failed charge %v (not the issue %v)", got.Attention, retried, issued)
	}
}
