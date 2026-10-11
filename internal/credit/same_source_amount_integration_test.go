package credit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A second ledger entry for the same source (here, one credit note) is "already
// exists" only when its amount matches — callers treat that as done. A
// DIFFERENT amount means two computations disagree; it used to hit the same
// dedup index and be swallowed, hiding the bug. Now it is a distinct, loud
// error that no caller mistakes for a replay.
func TestAppendEntry_SameSourceDifferentAmountIsRefused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Credit Same Source")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_same_source", DisplayName: "Same Source"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := credit.NewPostgresStore(db)
	entry := func(amount int64) domain.CreditLedgerEntry {
		return domain.CreditLedgerEntry{
			CustomerID: cust.ID, EntryType: domain.CreditGrant, AmountCents: amount,
			Description: "credit note CN-1", SourceCreditNoteID: "vlx_cn_same_source",
		}
	}

	if _, err := store.AppendEntry(ctx, tenantID, entry(5000)); err != nil {
		t.Fatalf("first entry: %v", err)
	}
	_, err = store.AppendEntry(ctx, tenantID, entry(5000))
	if !errors.Is(err, errs.ErrAlreadyExists) || errs.Code(err) != "credit_note_source_taken" {
		t.Fatalf("same amount again: err = %v (code %q), want the usual already-exists replay", err, errs.Code(err))
	}
	_, err = store.AppendEntry(ctx, tenantID, entry(7000))
	if errs.Code(err) != "credit_source_amount_mismatch" {
		t.Fatalf("different amount: err = %v (code %q), want credit_source_amount_mismatch", err, errs.Code(err))
	}
	if errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatal("a mismatch must not look like an already-exists replay — GrantForCreditNote would swallow it")
	}
	bal, err := store.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.BalanceCents != 5000 {
		t.Errorf("balance = %d, want 5000 (only the first entry)", bal.BalanceCents)
	}
}
