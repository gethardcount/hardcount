package billing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestFinalizeAudit_SharedFate is the money-path proof for the ADR-090 finalize
// migration, against REAL Postgres and the engine's REAL emit closure.
//
// Before this change, the engine finalized an invoice in one transaction and
// wrote its audit row in a SECOND, post-commit transaction whose error was
// discarded (`_ =`). So a finalized invoice — one the operator can see and the
// customer can be charged for — could exist with no record of what created it,
// permanently, if that second write failed. That was the `row_lost` outcome, and
// nothing retried it.
//
// Now the finalize row rides the invoice-create transaction. This test drives the
// engine's actual finalizeAuditEmit closure (not a hand-built one) through the
// real store's Audited create and pins both directions of shared fate:
//
//   - a failed audit emission ROLLS THE INVOICE BACK (no orphan invoice, no lost
//     evidence — the mutation is refused, which is the opposite, recoverable
//     incident);
//   - a successful create commits the invoice AND exactly one finalize row
//     atomically.
//
// The store-level rollback mechanics are also covered in
// invoice/intx_audit_integration_test.go; this test additionally proves the
// ENGINE is wired to that mechanism — that finalizeAuditEmit is passed and that
// its failure is not swallowed the way the old post-commit Log was.
func TestFinalizeAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Finalize Shared Fate")

	store := invoice.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_sf", DisplayName: "Shared Fate",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	finalized := func(number string) domain.Invoice {
		now := time.Now().UTC()
		return domain.Invoice{
			CustomerID:         cust.ID,
			InvoiceNumber:      number,
			Status:             domain.InvoiceFinalized,
			PaymentStatus:      domain.PaymentPending,
			Currency:           "USD",
			SubtotalCents:      10000,
			TotalAmountCents:   10000,
			AmountDueCents:     10000,
			BillingReason:      domain.BillingReasonSubscriptionCycle,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
			BillingPeriodEnd:   now,
			IssuedAt:           &now,
		}
	}

	invoiceExists := func(t *testing.T, number string) bool {
		t.Helper()
		var n int
		if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM invoices WHERE invoice_number = $1`, number).Scan(&n)
		}); err != nil {
			t.Fatalf("count invoices: %v", err)
		}
		return n > 0
	}
	finalizeRowsFor := func(t *testing.T, invoiceID string) int {
		t.Helper()
		var n int
		if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT count(*) FROM audit_log WHERE resource_id = $1 AND action = 'finalize'`, invoiceID).Scan(&n)
		}); err != nil {
			t.Fatalf("count finalize rows: %v", err)
		}
		return n
	}

	t.Run("audit emission fails → the invoice is ROLLED BACK, not orphaned", func(t *testing.T) {
		e := &Engine{}
		e.SetAuditLogger(erroringEngineAudit{}) // LogInTx returns an error

		_, err := store.CreateWithLineItemsAudited(ctx, tenantID, finalized("INV-SF-FAIL"), nil, e.finalizeAuditEmit(ctx))
		if err == nil {
			t.Fatal("CreateWithLineItemsAudited returned nil — a failed finalize audit must abort the invoice (shared fate)")
		}
		if invoiceExists(t, "INV-SF-FAIL") {
			t.Error("the invoice was persisted despite the audit emission failing — this is exactly the orphan-invoice / lost-evidence state ADR-090 removes")
		}
	})

	t.Run("audit emission succeeds → invoice + exactly one finalize row commit together", func(t *testing.T) {
		e := &Engine{}
		e.SetAuditLogger(logger) // real writer, real LogInTx

		out, err := store.CreateWithLineItemsAudited(ctx, tenantID, finalized("INV-SF-OK"), nil, e.finalizeAuditEmit(ctx))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if !invoiceExists(t, "INV-SF-OK") {
			t.Fatal("invoice missing after a successful create")
		}
		if got := finalizeRowsFor(t, out.ID); got != 1 {
			t.Errorf("finalize audit rows for %s = %d, want exactly 1 (the row that rode the create tx)", out.ID, got)
		}
	})

	t.Run("draft → invoice persists, NO finalize row (the row is service.Finalize's job later)", func(t *testing.T) {
		e := &Engine{}
		e.SetAuditLogger(logger)

		draft := finalized("INV-SF-DRAFT")
		draft.Status = domain.InvoiceDraft
		out, err := store.CreateWithLineItemsAudited(ctx, tenantID, draft, nil, e.finalizeAuditEmit(ctx))
		if err != nil {
			t.Fatalf("create draft: %v", err)
		}
		if !invoiceExists(t, "INV-SF-DRAFT") {
			t.Fatal("draft invoice was not persisted — the emit no-op must not block the write")
		}
		if got := finalizeRowsFor(t, out.ID); got != 0 {
			t.Errorf("draft wrote %d finalize rows, want 0 — a draft finalizes later via service.Finalize, which audits it there", got)
		}
	})
}
