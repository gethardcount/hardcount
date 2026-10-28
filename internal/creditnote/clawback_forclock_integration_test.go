package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestClawbackForClock_GatesWallClock_IssuesInFrozenTime is the real-Postgres
// proof of the clawback disjoint flow (ADR-029 / ADR-086 sim-data gating):
//
//   - the wall-clock reconciler scan (ListPendingClawbackDrafts) EXCLUDES a
//     simulated draft, so it never fires a Stripe refund / tax reversal on fake
//     data against real time;
//   - the catchup scan (ListPendingClawbackDraftsForClock) returns ONLY that
//     clock's simulated draft — not the wall-clock draft;
//   - RetryPendingClawbackIssueForClock issues the simulated draft and stamps
//     issued_at in SIMULATED (frozen) time, not wall-clock, and leaves the
//     wall-clock draft untouched.
func TestClawbackForClock_GatesWallClock_IssuesInFrozenTime(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Clawback ForClock")

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
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num + "-INV", Status: domain.InvoicePaid,
			PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
			SubtotalCents: 10000, TotalAmountCents: 10000, AmountDueCents: 0, AmountPaidCents: 10000,
			BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
		})
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

	// 1. Wall-clock reconciler scan excludes the simulated draft.
	wall, err := cnStore.ListPendingClawbackDrafts(ctx, 100, false)
	if err != nil {
		t.Fatalf("wall scan: %v", err)
	}
	if got := cnIDs(wall); len(got) != 1 || got[0] != realCN.ID {
		t.Errorf("wall-clock scan must return ONLY the real draft %s, got %v", realCN.ID, got)
	}

	// 2. Catchup scan returns ONLY this clock's simulated draft.
	forClock, err := cnStore.ListPendingClawbackDraftsForClock(ctx, tenantID, clockID, 100)
	if err != nil {
		t.Fatalf("forClock scan: %v", err)
	}
	if got := cnIDs(forClock); len(got) != 1 || got[0] != simCN.ID {
		t.Errorf("forClock scan must return ONLY the sim draft %s, got %v", simCN.ID, got)
	}

	// 3. Issuing via the catchup path stamps issued_at at frozen_time, and leaves
	//    the wall-clock draft untouched.
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	svc := creditnote.NewService(cnStore, invStore, nil, okGranterTx{})
	n, issueErrs := svc.RetryPendingClawbackIssueForClock(ctx, tenantID, clockID, frozen, 100)
	if len(issueErrs) > 0 {
		t.Fatalf("RetryPendingClawbackIssueForClock: %v", issueErrs)
	}
	if n != 1 {
		t.Fatalf("issued count: got %d, want 1", n)
	}
	got, err := cnStore.Get(ctx, tenantID, simCN.ID)
	if err != nil {
		t.Fatalf("get sim CN: %v", err)
	}
	if got.Status != domain.CreditNoteIssued {
		t.Errorf("sim CN status: got %q, want issued", got.Status)
	}
	if got.IssuedAt == nil || !got.IssuedAt.Equal(frozen) {
		t.Errorf("issued_at: got %v, want frozen %v — must stamp simulated time, not wall-clock", got.IssuedAt, frozen)
	}
	realGot, err := cnStore.Get(ctx, tenantID, realCN.ID)
	if err != nil {
		t.Fatalf("get real CN: %v", err)
	}
	if realGot.Status != domain.CreditNoteDraft {
		t.Errorf("the wall-clock draft must be untouched by the ForClock issuer, got status %q", realGot.Status)
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
