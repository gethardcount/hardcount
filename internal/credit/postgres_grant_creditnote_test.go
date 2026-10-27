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

// TestGrantForCreditNoteTx_IdempotentNoDoubleCredit asserts the real
// migration-0093 partial unique index idx_credit_ledger_credit_note_dedup
// makes the credit-note grant exactly-once.
//
// creditnote.Issue grants through GrantForCreditNoteTx on its coordinator tx,
// behind the draft→issued CAS, so a second grant for the same credit note is
// not expected. If one ever comes, the index refuses the second INSERT and the
// store returns ErrAlreadyExists (code "credit_note_source_taken"). The Tx
// variant surfaces it rather than swallowing it, because the conflict aborts
// the shared tx; the customer is still credited once.
func TestGrantForCreditNoteTx_IdempotentNoDoubleCredit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	tenantID := testutil.CreateTestTenant(t, db, "Grant CN Idempotent")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_grant_cn_idem", DisplayName: "Grant CN Idem",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Same shape creditnote.Issue() uses for a credit-type CN.
	const creditNoteID = "vlx_cn_grant_idem"
	input := credit.GrantInput{
		Currency:    "USD",
		CustomerID:  cust.ID,
		AmountCents: 4000,
		Description: "Credit note CN-001 — duplicate_charge",
	}

	// First grant, on its own committed tx: restores $40 to the balance.
	tx1, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin first tx: %v", err)
	}
	first, err := svc.GrantForCreditNoteTx(ctx, tx1, tenantID, creditNoteID, input)
	if err != nil {
		_ = tx1.Rollback()
		t.Fatalf("first GrantForCreditNoteTx: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit first tx: %v", err)
	}
	if first.AmountCents != 4000 {
		t.Fatalf("first grant amount: got %d, want 4000", first.AmountCents)
	}

	balAfterFirst, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance after first grant: %v", err)
	}
	if balAfterFirst.BalanceCents != 4000 {
		t.Fatalf("balance after first grant: got %d, want 4000", balAfterFirst.BalanceCents)
	}

	// Second grant for the same credit note: the insert hits
	// idx_credit_ledger_credit_note_dedup and is refused as already-exists.
	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin second tx: %v", err)
	}
	_, err = svc.GrantForCreditNoteTx(ctx, tx2, tenantID, creditNoteID, input)
	_ = tx2.Rollback()
	if !errors.Is(err, errs.ErrAlreadyExists) || errs.Code(err) != "credit_note_source_taken" {
		t.Fatalf("second GrantForCreditNoteTx: err = %v (code %q), want already-exists credit_note_source_taken", err, errs.Code(err))
	}

	// Balance reflects a SINGLE grant, not doubled.
	balAfterSecond, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance after second grant: %v", err)
	}
	if balAfterSecond.BalanceCents != 4000 {
		t.Errorf("balance after second grant: got %d, want 4000 (no double-credit)", balAfterSecond.BalanceCents)
	}

	// Exactly one ledger grant row exists for this credit-note source — the
	// invariant the partial unique index enforces.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var grantRows int
	if err := tx.QueryRow(`
		SELECT count(*) FROM customer_credit_ledger
		WHERE source_credit_note_id = $1
	`, creditNoteID).Scan(&grantRows); err != nil {
		t.Fatalf("count credit-note grant rows: %v", err)
	}
	if grantRows != 1 {
		t.Errorf("grant rows for credit note: got %d, want 1", grantRows)
	}
}
