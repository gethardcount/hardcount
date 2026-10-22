package credit_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestReverseForInvoiceTx_IdempotentNoDoubleCredit reproduces the
// double-credit bug caught in the 2026-06 backend audit: voiding an
// invoice and then manual-resolving the same invoice's dunning run (or
// any retry of the void) re-summed the untouched usage rows and
// re-granted the full applied amount, crediting the customer twice.
//
// The fix stamps each reversal grant with source_invoice_reversal_id
// and enforces one-reversal-per-invoice via the partial unique index
// idx_credit_ledger_reversal_dedup (migration 0106). A second
// ReverseForInvoiceTx hits the constraint and returns 0: no second credit.
// The conflict aborts that second tx, so the test rolls it back.
func TestReverseForInvoiceTx_IdempotentNoDoubleCredit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	invoiceStore := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Reverse Idempotent")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_reverse_idem", DisplayName: "Reverse Idem",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Grant $100, then apply $40 to an invoice — leaves $60 balance and a
	// usage entry of -$40 linked to the invoice.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, AmountCents: 10000, Description: "seed",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	now := time.Now().UTC()
	dueAt := now.Add(7 * 24 * time.Hour)
	issuedAt := now
	inv, err := invoiceStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      4000,
		TotalAmountCents:   4000,
		AmountDueCents:     4000,
		BillingPeriodStart: now,
		BillingPeriodEnd:   now.Add(30 * 24 * time.Hour),
		IssuedAt:           &issuedAt,
		DueAt:              &dueAt,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	applied, err := svc.ApplyToInvoice(ctx, tenantID, cust.ID, inv.ID, 4000)
	if err != nil {
		t.Fatalf("ApplyToInvoice: %v", err)
	}
	if applied != 4000 {
		t.Fatalf("applied: got %d, want 4000", applied)
	}

	balAfterApply, err := svc.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get balance after apply: %v", err)
	}
	if balAfterApply.BalanceCents != 6000 {
		t.Fatalf("balance after apply: got %d, want 6000 ($100 - $40)", balAfterApply.BalanceCents)
	}

	// First reversal: invoice voided. Credits the $40 back → balance $100.
	tx1, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin first tx: %v", err)
	}
	reversed, err := svc.ReverseForInvoiceTx(ctx, tx1, tenantID, cust.ID, inv.ID, "INV-001")
	if err != nil {
		_ = tx1.Rollback()
		t.Fatalf("first ReverseForInvoiceTx: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit first tx: %v", err)
	}
	if reversed != 4000 {
		t.Errorf("first reversal: got %d, want 4000", reversed)
	}

	balAfterFirst, err := svc.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get balance after first reversal: %v", err)
	}
	if balAfterFirst.BalanceCents != 10000 {
		t.Fatalf("balance after first reversal: got %d, want 10000 ($60 + $40 reversed)", balAfterFirst.BalanceCents)
	}

	// Second reversal: dunning manual-resolve / retry of the same void.
	// Pre-fix this re-granted another $40 → balance $140. Post-fix it's an
	// idempotent no-op returning 0 with balance unchanged.
	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin second tx: %v", err)
	}
	reversedAgain, err := svc.ReverseForInvoiceTx(ctx, tx2, tenantID, cust.ID, inv.ID, "INV-001")
	_ = tx2.Rollback()
	if err != nil {
		t.Fatalf("second ReverseForInvoiceTx: %v", err)
	}
	if reversedAgain != 0 {
		t.Errorf("second reversal: got %d, want 0 (idempotent no-op)", reversedAgain)
	}

	balAfterSecond, err := svc.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get balance after second reversal: %v", err)
	}
	if balAfterSecond.BalanceCents != 10000 {
		t.Errorf("balance after second reversal: got %d, want 10000 (no double-credit)", balAfterSecond.BalanceCents)
	}

	// Exactly one reversal grant exists for this invoice.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var reversalGrants int
	if err := tx.QueryRow(`
		SELECT count(*) FROM customer_credit_ledger
		WHERE source_invoice_reversal_id = $1
	`, inv.ID).Scan(&reversalGrants); err != nil {
		t.Fatalf("count reversal grants: %v", err)
	}
	if reversalGrants != 1 {
		t.Errorf("reversal grants for invoice: got %d, want 1", reversalGrants)
	}
}
