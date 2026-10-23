package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUpdateStatusWithReversal_ReversalFailure_RealTxRollsBackVoid is the
// real-Postgres proof of the void atomicity (#29): UpdateStatusWithReversal
// flips the invoice to voided AND runs the consumed-credit reversal in ONE tx,
// so a reversal failure must roll the VOID back. Otherwise the invoice lands
// voided while the customer's applied credits stay consumed — silently
// stripping them of the credits they paid the invoice with, with no reconciler
// to re-drive the reversal. Only a real tx proves the status flip is undone.
func TestUpdateStatusWithReversal_ReversalFailure_RealTxRollsBackVoid(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Void Reversal Rollback")
	invID := seedDraftInvoice(t, db, tenantID)

	reverseErr := errors.New("simulated in-tx credit reversal failure")
	called := false
	_, err := store.UpdateStatusWithReversal(ctx, tenantID, invID, domain.InvoiceVoided, func(tx *sql.Tx) error {
		called = true
		return reverseErr
	})
	if !errors.Is(err, reverseErr) {
		t.Fatalf("UpdateStatusWithReversal must surface the reverseFn error, got %v", err)
	}
	if !called {
		t.Fatal("reverseFn was never invoked — the test is vacuous")
	}

	// The real assertion: the void flip MUST have rolled back — a fresh read
	// still sees the invoice in its prior (draft) status, not voided.
	after, err := store.Get(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("get after rollback: %v", err)
	}
	if after.Status != domain.InvoiceDraft {
		t.Fatalf("void must roll back on a reversal failure; status=%q, want draft (voided-but-credits-still-consumed strands customer credits)", after.Status)
	}
	if after.VoidedAt != nil {
		t.Errorf("voided_at must be unset after rollback; got %v", after.VoidedAt)
	}
}

// TestUpdateStatusWithReversal_Success_CommitsVoidAndCreditGrant proves the
// cross-store coordinator tx actually COMMITS: a reversal grant appended on the
// invoice store's tx (the credit store's AppendEntryTx, the same call
// ReverseForInvoiceTx makes) lands in customer_credit_ledger together with the
// voided status — neither half is silently rolled back or RLS-blocked.
func TestUpdateStatusWithReversal_Success_CommitsVoidAndCreditGrant(t *testing.T) {
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

	voided, err := store.UpdateStatusWithReversal(ctx, tenantID, invID, domain.InvoiceVoided, func(tx *sql.Tx) error {
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
		t.Fatalf("UpdateStatusWithReversal: %v", err)
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
