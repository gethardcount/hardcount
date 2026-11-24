package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestListPendingClawbackDrafts_ExcludesASimulatedDraft (ADR-029 / ADR-086
// sim-data gating): the wall-clock reconciler scan excludes a simulated
// draft, so it never fires a Stripe refund / tax reversal on fake data
// against real time.
func TestListPendingClawbackDrafts_ExcludesASimulatedDraft(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Clawback Sim Gate")

	custStore := customer.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	cnStore := creditnote.NewPostgresStore(db)
	now := time.Now().UTC()

	clockID := seedClockRow(t, db, tenantID)
	simCust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sim", DisplayName: "Sim", TestClockID: clockID})
	if err != nil {
		t.Fatalf("sim customer: %v", err)
	}
	realCust, err := custStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_real", DisplayName: "Real"})
	if err != nil {
		t.Fatalf("real customer: %v", err)
	}

	// A paid source + a pending (issue_pending) clawback draft for each customer.
	mkPaidClawback := func(cust domain.Customer, num string) domain.CreditNote {
		t.Helper()
		issued := now
		inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num + "-INV", Status: domain.InvoicePaid,
			PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
			SubtotalCents: 10000, TotalAmountCents: 10000, AmountDueCents: 0, AmountPaidCents: 10000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
		}, nil)
		if err != nil {
			t.Fatalf("invoice %s: %v", num, err)
		}
		cn, err := cnStore.Create(ctx, tenantID, domain.CreditNote{
			InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: num,
			Status: domain.CreditNoteDraft, Reason: "subscription_cancellation",
			SubtotalCents: 4000, TotalCents: 4000, CreditAmountCents: 4000, // credit-type → in-tx grant path
			Currency: "USD", RefundStatus: domain.RefundNone, IssuePending: true,
		})
		if err != nil {
			t.Fatalf("clawback %s: %v", num, err)
		}
		return cn
	}
	simCN := mkPaidClawback(simCust, "CN-SIM")
	realCN := mkPaidClawback(realCust, "CN-REAL")
	// Stamp the simulated marker (the engine stamps it from the customer pin at
	// build; set it directly here since we seed the draft through the store).
	execBypassCN(t, db, `UPDATE credit_notes SET is_simulated = true WHERE id = $1`, simCN.ID)

	wall, err := cnStore.ListPendingClawbackDrafts(ctx, 100, false)
	if err != nil {
		t.Fatalf("wall scan: %v", err)
	}
	if got := cnIDs(wall); len(got) != 1 || got[0] != realCN.ID {
		t.Errorf("wall-clock scan must return ONLY the real draft %s, got %v", realCN.ID, got)
	}
}

func cnIDs(cns []domain.CreditNote) []string {
	out := make([]string, 0, len(cns))
	for _, cn := range cns {
		out = append(out, cn.ID)
	}
	return out
}

func seedClockRow(t *testing.T, db *postgres.DB, tenantID string) string {
	t.Helper()
	id := postgres.NewID("hc_tclk")
	execBypassCN(t, db, `INSERT INTO test_clocks (id, tenant_id, frozen_time) VALUES ($1, $2, now())`, id, tenantID)
	return id
}

func execBypassCN(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
