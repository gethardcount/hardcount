package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A credit note's "reduce the amount due" leg chooses its branch from a read
// taken before its tx. If a payment lands in between, the reduction must
// refuse (so the note is re-applied against the paid invoice and credits the
// balance) instead of zeroing the amount due of an invoice already paid.
func TestApplyCreditNoteTx_RefusesAPaidInvoice(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Apply CN Guard")
	store := invoice.NewPostgresStore(db)

	apply := func(id string) error {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := store.ApplyCreditNoteTx(ctx, tx, tenantID, id, 1000, 0); err != nil {
			return err
		}
		return tx.Commit()
	}

	open := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CN-OPEN")
	if err := apply(open.ID); err != nil {
		t.Fatalf("open receivable: %v", err)
	}
	if got, _ := store.Get(ctx, tenantID, open.ID); got.AmountDueCents != 4000 {
		t.Errorf("open receivable amount due = %d, want 4000", got.AmountDueCents)
	}

	paid := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CN-PAID")
	if _, err := store.MarkPaid(ctx, tenantID, paid.ID, "pi_paid", time.Now().UTC()); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if err := apply(paid.ID); errs.Code(err) != "invoice_not_open_receivable" {
		t.Fatalf("paid invoice: err = %v, want invoice_not_open_receivable", err)
	}
}
