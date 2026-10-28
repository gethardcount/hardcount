package payment_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// P13 token-burn semantics: the payment-update link must survive
// everything EXCEPT a successful session create. Validate (the GET
// page render) never consumes; Consume is the exactly-once CAS; and
// Restore revives a token whose Stripe create failed moments after
// consumption — a transient upstream error used to permanently kill
// the customer's emailed link ("invalid or expired token" forever).
//
// Mutation-verify: gut Restore (no-op UPDATE) — the revive assertions
// fail.
func TestPaymentUpdateToken_BurnAndRestoreSemantics(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Token Restore")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_tok", DisplayName: "Tok",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	issued := now
	due := now.Add(7 * 24 * time.Hour)
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		InvoiceNumber: "INV-TOK-1", CustomerID: cust.ID,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		Currency: "USD", TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now,
		IssuedAt: &issued, DueAt: &due,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	svc := payment.NewTokenService(db)
	raw, err := svc.Create(ctx, tenantID, cust.ID, inv.ID)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// GET/render path: Validate twice — never consumes.
	for i := 0; i < 2; i++ {
		if _, err := svc.Validate(ctx, raw); err != nil {
			t.Fatalf("validate #%d: %v (the page render must not burn the token)", i+1, err)
		}
	}

	// Consume is the exactly-once CAS.
	ok, err := svc.Consume(ctx, tenantID, raw)
	if err != nil || !ok {
		t.Fatalf("first consume: ok=%v err=%v", ok, err)
	}
	if ok, _ := svc.Consume(ctx, tenantID, raw); ok {
		t.Fatal("second consume succeeded; the CAS must be exactly-once")
	}

	// Restore (the failed-Stripe-create path) revives it…
	if err := svc.Restore(ctx, tenantID, raw); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := svc.Validate(ctx, raw); err != nil {
		t.Fatalf("validate after restore: %v (transient create failure must not kill the link)", err)
	}
	ok, err = svc.Consume(ctx, tenantID, raw)
	if err != nil || !ok {
		t.Fatalf("consume after restore: ok=%v err=%v", ok, err)
	}

	// …but only within the recency window: a token consumed a while ago
	// (i.e., by a SUCCESSFUL create) cannot be resurrected by a replay.
	btx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	if _, err := btx.Exec(`UPDATE payment_update_tokens SET used_at = NOW() - INTERVAL '10 minutes' WHERE tenant_id = $1`, tenantID); err != nil {
		t.Fatalf("age used_at: %v", err)
	}
	if err := btx.Commit(); err != nil {
		t.Fatalf("commit age: %v", err)
	}
	if err := svc.Restore(ctx, tenantID, raw); err != nil {
		t.Fatalf("restore (aged): %v", err)
	}
	if _, err := svc.Validate(ctx, raw); err == nil {
		t.Fatal("aged consumed token restored; the recency guard must bound revival to the consume→create window")
	}
}
