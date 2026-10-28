package invoice_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestListPendingTaxCommit_IncludesNonFinalizedOrphans locks the fix: the
// commit reconciler must also recover orphans that already flipped to
// paid/voided/uncollectible — which happens on the synchronous
// finalize+auto-charge path, where the invoice reaches 'paid' in the same
// flow before any scheduler tick. Pre-fix the finalized-only filter missed
// these, so a later credit-note/void couldn't reverse the committed tax and
// the tenant over-remitted. Bounded by the existing 24h Stripe-calc window.
func TestListPendingTaxCommit_IncludesNonFinalizedOrphans(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Tax Commit NonFinal")
	invID := seedDraftInvoice(t, db, tenantID)

	exec := func(q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// A stripe_tax orphan (calc id set, txn id NULL) is now recovered in any
	// of the post-finalize terminal statuses, not just 'finalized'.
	for _, status := range []string{"paid", "voided", "uncollectible"} {
		exec(`UPDATE invoices SET status=$2, tax_status='ok', tax_provider='stripe_tax',
		         tax_calculation_id='taxcalc_orphan', tax_transaction_id=NULL, updated_at=now() WHERE id=$1`, invID, status)
		got, err := store.ListPendingTaxCommit(ctx, 50, false)
		if err != nil {
			t.Fatalf("list (%s): %v", status, err)
		}
		if len(got) != 1 || got[0].ID != invID {
			t.Fatalf("%s orphan not returned: got %d rows", status, len(got))
		}
	}

	// A paid orphan aged past the 24h Stripe-calc window is excluded (the
	// transaction can no longer be re-fetched, so there's nothing to recover).
	exec(`UPDATE invoices SET status='paid', updated_at=now() - interval '25 hours' WHERE id=$1`, invID)
	got, err := store.ListPendingTaxCommit(ctx, 50, false)
	if err != nil {
		t.Fatalf("list aged: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("aged-out paid orphan still returned: %d rows", len(got))
	}
}
