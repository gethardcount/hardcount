package invoice_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestUpdateStatusWithReversalPrior_Success_CommitsVoidAndCreditGrant proves
// the cross-store coordinator tx actually COMMITS: a reversal grant appended on
// the invoice store's tx (the credit store's AppendEntryTx, the same call
// ReverseForInvoiceTx makes) lands in customer_credit_ledger together with the
// voided status — neither half is silently rolled back or RLS-blocked. The
// rollback half (a closure error undoes the void) is pinned by the intx_audit
// subtest "closure failure rolls the void back and writes no audit row".
func TestUpdateStatusWithReversalPrior_Success_CommitsVoidAndCreditGrant(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	creditStore := credit.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Void Reversal Commit")
	invID := seedDraftInvoice(t, db, tenantID)

	seeded, err := store.Get(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("get seeded invoice: %v", err)
	}
	custID := seeded.CustomerID

	voided, err := store.UpdateStatusWithReversalPrior(ctx, tenantID, invID, domain.InvoiceVoided, func(tx *sql.Tx, _ domain.Invoice) error {
		_, e := creditStore.AppendEntryTx(ctx, tx, tenantID, domain.CreditLedgerEntry{
			Currency:                "USD",
			CustomerID:              custID,
			EntryType:               domain.CreditGrant,
			AmountCents:             500,
			Description:             "Reversed — invoice voided (test)",
			InvoiceID:               invID,
			SourceInvoiceReversalID: invID,
		})
		return e
	})
	if err != nil {
		t.Fatalf("UpdateStatusWithReversalPrior: %v", err)
	}
	if voided.Status != domain.InvoiceVoided {
		t.Fatalf("returned status=%q, want voided", voided.Status)
	}

	// Both halves committed: the invoice is voided AND the reversal grant exists.
	after, err := store.Get(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("get after commit: %v", err)
	}
	if after.Status != domain.InvoiceVoided {
		t.Fatalf("invoice must be voided after commit; status=%q", after.Status)
	}

	bal, err := testutil.BalanceUSD(creditStore.GetBalance(ctx, tenantID, custID))
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.BalanceCents != 500 {
		t.Fatalf("reversal grant must have committed on the shared tx; balance=%d, want 500", bal.BalanceCents)
	}
}
