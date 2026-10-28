package creditnote_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A credit note created on a caller's transaction that rolls back (a cancel
// whose relief write failed) must give its number back: the next credit note
// takes the same number, so the tenant's sequence has no gap.
func TestCreditNoteNumber_RolledBackCreateLeavesNoGap(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Number Gap")
	_, inv := seedInvoiceForCN(t, db, tenantID, "number-gap", domain.InvoiceFinalized, domain.PaymentPending, "")

	store := creditnote.NewPostgresStore(db)
	svc := creditnote.NewService(store, invoice.NewPostgresStore(db), nil)
	svc.SetNumberGenerator(tenant.NewSettingsStore(db))

	tx, err := store.BeginTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	rolledBack, err := svc.CreateAdjustmentDraftTx(ctx, tx, tenantID, inv.ID, 100, "cancel_relief", "unused time")
	if err != nil {
		t.Fatalf("CreateAdjustmentDraftTx: %v", err)
	}
	postgres.Rollback(tx)

	kept, err := svc.Create(ctx, tenantID, creditnote.CreateInput{
		InvoiceID: inv.ID,
		Reason:    "order_change",
		Lines:     []creditnote.CreditLineInput{{Description: "over-billed seat", Quantity: 1, UnitAmountCents: 100}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if kept.CreditNoteNumber != rolledBack.CreditNoteNumber {
		t.Fatalf("number after a rolled-back create = %s, want %s reused (a gap was left)", kept.CreditNoteNumber, rolledBack.CreditNoteNumber)
	}
}
