package creditnote_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCreateUnderInvoiceLockTx_RollsBackWithCallerTx is the real-Postgres proof
// of the create-atomicity half of ADR-057: a clawback credit note created via
// CreateUnderInvoiceLockTx rides the CALLER's transaction, so when the caller
// rolls back (e.g. a later step in the item-change tx fails) the credit note is
// gone too — the item change and the clawback obligation are all-or-nothing.
// Pre-fix the clawback was created+issued post-commit, fire-and-forget, so a
// removed item could be left with no credit note at all. The in-memory double
// can't model rollback; this proves it against real tx semantics.
func TestCreateUnderInvoiceLockTx_RollsBackWithCallerTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Tx Atomicity")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cntx", DisplayName: "CN Tx",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-CNTX-1",
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		AmountDueCents:     10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	store := creditnote.NewPostgresStore(db)
	build := func(_ *sql.Tx, existing []domain.CreditNote) (domain.CreditNote, error) {
		return domain.CreditNote{
			InvoiceID:        inv.ID,
			CustomerID:       cust.ID,
			CreditNoteNumber: "CN-CNTX-1",
			Status:           domain.CreditNoteDraft,
			Reason:           "subscription_downgrade",
			SubtotalCents:    5000,
			TotalCents:       5000,
			Currency:         "USD",
			RefundStatus:     domain.RefundNone,
			IssuePending:     true,
		}, nil
	}

	// --- Rollback path: the draft must NOT survive the caller's rollback. ---
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	cn, err := store.CreateUnderInvoiceLockTx(ctx, tx, tenantID, inv.ID, nil, build)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("create under invoice lock tx: %v", err)
	}
	// Uncommitted: a read on the pool (a separate connection) must not see it.
	if pre, err := store.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: inv.ID}); err != nil {
		t.Fatalf("list pre-commit: %v", err)
	} else if len(pre) != 0 {
		t.Errorf("pre-commit pool read sees %d notes, want 0 (draft must be invisible until commit)", len(pre))
	}
	// Mirror the handler's deferred rollback when a later in-tx step fails.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	after, err := store.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: inv.ID})
	if err != nil {
		t.Fatalf("list after rollback: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("after rollback: %d notes survive, want 0 — the draft create (id %s) must roll back with the caller's tx", len(after), cn.ID)
	}

	// --- Commit path (positive control): the same call DOES persist on commit,
	// with issue_pending round-tripping through migration 0121. ---
	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	committed, err := store.CreateUnderInvoiceLockTx(ctx, tx2, tenantID, inv.ID, nil, build)
	if err != nil {
		_ = tx2.Rollback()
		t.Fatalf("create under invoice lock tx2: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}
	got, err := store.Get(ctx, tenantID, committed.ID)
	if err != nil {
		t.Fatalf("get committed: %v", err)
	}
	if got.Status != domain.CreditNoteDraft {
		t.Errorf("committed status: got %q, want draft", got.Status)
	}
	if !got.IssuePending {
		t.Error("issue_pending must round-trip true through migration 0121")
	}
}
