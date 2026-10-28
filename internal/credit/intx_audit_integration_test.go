package credit_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// failingEmitter satisfies credit.AuditEmitter and always errors — the fault
// injection for the shared-fate direction the playbook's mutation-verify
// gate demands: if the audit row cannot be written, the money mutation must
// not commit.
type failingEmitter struct{}

func (failingEmitter) LogInTx(_ context.Context, _ *sql.Tx, _ audit.Entry) error {
	return errors.New("injected audit failure")
}

// Shared-fate integration coverage for the first ADR-090 domain batch
// (credit grant/adjust). Both directions:
//   - business write fails → no audit row (phantom class),
//   - audit emission fails → no business write (silent-loss class).
func TestCreditAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Credit InTx Audit")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	customerID := "hc_cus_sharedfate"
	seedTx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := seedTx.ExecContext(ctx,
		`INSERT INTO customers (id, tenant_id, external_id, display_name, email, status)
		 VALUES ($1, $2, 'cus_shared_fate', 'Shared Fate Cust', 'sf@example.com', 'active')`,
		customerID, tenantID,
	); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if err := seedTx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	store := credit.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	t.Run("grant commits ledger row and audit row together", func(t *testing.T) {
		svc := credit.NewService(store)
		svc.SetAuditLogger(logger)

		entry, err := svc.Grant(ctx, tenantID, credit.GrantInput{
			Currency:   "USD",
			CustomerID: customerID, AmountCents: 5000, Description: "shared-fate grant",
		})
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "credit", ResourceID: entry.ID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		if len(rows) != 1 || rows[0].Action != "grant" {
			t.Fatalf("want exactly one 'grant' audit row for %s; got %+v", entry.ID, rows)
		}
		if rows[0].Metadata["customer_id"] != customerID {
			t.Errorf("audit metadata customer_id: got %v", rows[0].Metadata["customer_id"])
		}
	})

	t.Run("audit failure rolls the grant back", func(t *testing.T) {
		svc := credit.NewService(store)
		svc.SetAuditLogger(failingEmitter{})

		_, err := svc.Grant(ctx, tenantID, credit.GrantInput{
			Currency:   "USD",
			CustomerID: customerID, AmountCents: 7700, Description: "must-roll-back grant",
		})
		if err == nil {
			t.Fatal("grant must fail when its audit emission fails (shared fate)")
		}
		bal, err := testutil.BalanceUSD(store.GetBalance(ctx, tenantID, customerID))
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
		if bal.BalanceCents != 5000 {
			t.Errorf("ledger balance: got %d, want 5000 — the failed-audit grant leaked into the ledger", bal.BalanceCents)
		}
	})

	t.Run("business failure writes no audit row", func(t *testing.T) {
		svc := credit.NewService(store)
		svc.SetAuditLogger(logger)

		// Deduct more than the balance — AdjustAtomic fails inside the tx
		// AFTER the emission hook site would run on success.
		_, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
			Currency:   "USD",
			CustomerID: customerID, AmountCents: -999999, Description: "overdraft attempt",
		})
		if err == nil {
			t.Fatal("overdraft adjust must fail")
		}
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{Action: "credit.deduction"})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("phantom audit row for a failed deduction: %+v", rows)
		}
	})

	t.Run("audit failure rolls the adjust back", func(t *testing.T) {
		svc := credit.NewService(store)
		svc.SetAuditLogger(failingEmitter{})

		before, err := testutil.BalanceUSD(store.GetBalance(ctx, tenantID, customerID))
		if err != nil {
			t.Fatalf("balance before: %v", err)
		}
		_, err = svc.Adjust(ctx, tenantID, credit.AdjustInput{
			Currency:   "USD",
			CustomerID: customerID, AmountCents: -500, Description: "must-roll-back deduction",
		})
		if err == nil {
			t.Fatal("adjust must fail when its audit emission fails (shared fate)")
		}
		after, err := testutil.BalanceUSD(store.GetBalance(ctx, tenantID, customerID))
		if err != nil {
			t.Fatalf("balance after: %v", err)
		}
		if after.BalanceCents != before.BalanceCents {
			t.Errorf("ledger balance moved %d→%d despite failed audit emission", before.BalanceCents, after.BalanceCents)
		}
	})

	t.Run("successful adjust audits with the sign-split action", func(t *testing.T) {
		svc := credit.NewService(store)
		svc.SetAuditLogger(logger)

		entry, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
			Currency:   "USD",
			CustomerID: customerID, AmountCents: -1000, Description: "small deduction",
		})
		if err != nil {
			t.Fatalf("adjust: %v", err)
		}
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
			ResourceType: "credit", ResourceID: entry.ID,
		})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		if len(rows) != 1 || rows[0].Action != "credit.deduction" {
			t.Fatalf("want one 'credit.deduction' row; got %+v", rows)
		}
	})
}
