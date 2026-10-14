package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// errInjectedAuditEmit is the fault injected into the in-tx emission the void
// closure carries (ADR-090 shared fate): an audit row that cannot be written
// must take the void down with it.
var errInjectedAuditEmit = errors.New("injected audit emission failure")

// seedVoidableInvoice creates a customer + FINALIZED invoice — the prior
// status the void's audit row must record. Column set mirrors
// postgres_void_reversal_rollback_integration_test.go's seed path (no
// subscription needed: the store-level flip doesn't read one).
func seedVoidableInvoice(t *testing.T, db *postgres.DB, tenantID, suffix string) domain.Invoice {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_prior_" + suffix, DisplayName: "Prior Status " + suffix,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-PRIOR-" + suffix,
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
	return inv
}

// TestUpdateStatusWithReversalPrior_TruePriorAndSharedFate pins the PR4 review
// fix: the closure's `prior` is the invoice's TRUE status, read FOR UPDATE on
// the flip's OWN transaction — not the service's earlier, racy snapshot, which
// could permanently stamp the wrong status_before into an append-only audit
// row. The second leg proves shared fate: a closure error (the emission
// failing) rolls the void back.
func TestUpdateStatusWithReversalPrior_TruePriorAndSharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Invoice Void Prior Audit")

	store := invoice.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	t.Run("closure receives the true prior status read under the row lock", func(t *testing.T) {
		inv := seedVoidableInvoice(t, db, tenantID, "ok")

		var gotPrior domain.InvoiceStatus
		calls := 0
		voided, err := store.UpdateStatusWithReversalPrior(ctx, tenantID, inv.ID, domain.InvoiceVoided,
			func(tx *sql.Tx, locked domain.Invoice) error {
				calls++
				gotPrior = locked.Status
				// The real void emission (invoice.Service.Void) stamps
				// status_before from exactly this value.
				return logger.LogInTx(ctx, tx, audit.Entry{
					Action:        domain.AuditActionVoid,
					ResourceType:  "invoice",
					ResourceID:    inv.ID,
					ResourceLabel: inv.InvoiceNumber,
					Metadata: map[string]any{
						"invoice_number":     inv.InvoiceNumber,
						"customer_id":        inv.CustomerID,
						"total_amount_cents": inv.TotalAmountCents,
						"currency":           inv.Currency,
						"status_before":      string(locked.Status),
					},
				})
			})
		if err != nil {
			t.Fatalf("UpdateStatusWithReversalPrior: %v", err)
		}
		if calls != 1 {
			t.Fatalf("closure ran %d times, want exactly 1", calls)
		}
		if gotPrior != domain.InvoiceFinalized {
			t.Fatalf("prior handed to the closure: got %q, want finalized — status_before must come from the in-tx locked read, not a pre-tx snapshot", gotPrior)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Fatalf("returned status: got %q, want voided", voided.Status)
		}

		after, err := store.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get after commit: %v", err)
		}
		if after.Status != domain.InvoiceVoided {
			t.Fatalf("committed status: got %q, want voided", after.Status)
		}

		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "invoice", ResourceID: inv.ID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		if len(rows) != 1 || rows[0].Action != domain.AuditActionVoid {
			t.Fatalf("want exactly one 'void' audit row on the flip's tx; got %+v", rows)
		}
		if rows[0].Metadata["status_before"] != string(domain.InvoiceFinalized) {
			t.Errorf("audit metadata status_before: got %v, want finalized", rows[0].Metadata["status_before"])
		}
	})

	t.Run("closure failure rolls the void back and writes no audit row", func(t *testing.T) {
		inv := seedVoidableInvoice(t, db, tenantID, "fail")

		calls := 0
		_, err := store.UpdateStatusWithReversalPrior(ctx, tenantID, inv.ID, domain.InvoiceVoided,
			func(tx *sql.Tx, locked domain.Invoice) error {
				calls++
				if locked.Status != domain.InvoiceFinalized {
					t.Errorf("prior handed to the closure: got %q, want finalized", locked.Status)
				}
				return errInjectedAuditEmit
			})
		if !errors.Is(err, errInjectedAuditEmit) {
			t.Fatalf("want the injected emission error surfaced (shared fate); got %v", err)
		}
		if calls != 1 {
			t.Fatalf("closure ran %d times, want exactly 1 (the test is vacuous otherwise)", calls)
		}

		after, err := store.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get after rollback: %v", err)
		}
		if after.Status != domain.InvoiceFinalized {
			t.Fatalf("status: got %q, want finalized — the void must roll back with its failed audit emission", after.Status)
		}
		if after.VoidedAt != nil {
			t.Errorf("voided_at must be unset after rollback; got %v", after.VoidedAt)
		}

		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "invoice", ResourceID: inv.ID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("audit row leaked from a rolled-back void: %+v", rows)
		}
	})
}
