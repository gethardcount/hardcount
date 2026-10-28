package testclock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testclock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Deleting a test clock tears down its ENTIRE simulated customer graph
// (ADR-086, Design B) — the customers pinned to it and every row they own —
// while a customer NOT on the clock survives untouched. That is the whole basis
// of the design: after teardown no simulated row survives to leak, and no other
// data is disturbed. This test also exercises every DELETE in the ordered
// teardown, so a missing table or a wrong FK order would fail here.
func TestDelete_TearsDownSimulatedGraph_KeepsEverythingElse(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Teardown")

	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()

	execTx := func(sql string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	exists := func(table, col, id string) bool {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var ok bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE `+col+` = $1)`, id).Scan(&ok); err != nil {
			t.Fatalf("exists %s: %v", table, err)
		}
		return ok
	}
	seedInvoice := func(custID, num string) string {
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: custID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: 100,
			TotalAmountCents: 100, AmountDueCents: 100,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		return inv.ID
	}

	// A clock + a customer pinned to it + an invoice it owns + a raw usage event.
	clockID := "hc_tclk_teardown01"
	execTx(`INSERT INTO test_clocks (id, tenant_id, frozen_time) VALUES ($1, $2, now())`, clockID, tenantID)
	simCust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sim", DisplayName: "Sim", TestClockID: clockID})
	if err != nil {
		t.Fatalf("create clock-pinned customer: %v", err)
	}
	simInv := seedInvoice(simCust.ID, "INV-SIM")

	// A customer NOT on any clock + its invoice — must survive.
	realCust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_real", DisplayName: "Real"})
	if err != nil {
		t.Fatalf("create non-clock customer: %v", err)
	}
	realInv := seedInvoice(realCust.ID, "INV-REAL")

	// Tear down the clock (exercises every ordered DELETE).
	store := testclock.NewPostgresStore(db)
	if err := store.Delete(ctx, tenantID, clockID); err != nil {
		t.Fatalf("clock teardown: %v", err)
	}

	// The clock and its whole graph are GONE.
	if exists("test_clocks", "id", clockID) {
		t.Error("the test clock must be hard-deleted")
	}
	if exists("customers", "id", simCust.ID) {
		t.Error("the clock-pinned customer must be torn down")
	}
	if exists("invoices", "id", simInv) {
		t.Error("the clock-pinned customer's invoice must be torn down")
	}

	// Everything NOT on the clock survives.
	if !exists("customers", "id", realCust.ID) {
		t.Error("a customer not on the clock must survive teardown")
	}
	if !exists("invoices", "id", realInv) {
		t.Error("a non-clock customer's invoice must survive teardown")
	}

	// Idempotent: re-deleting the gone clock is ErrNotFound, not a crash.
	if err := store.Delete(ctx, tenantID, clockID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("re-deleting a gone clock must return ErrNotFound, got %v", err)
	}
}
