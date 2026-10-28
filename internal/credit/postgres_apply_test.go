package credit_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestApplyToInvoiceAtomic_CapsAtCurrentBalance reproduces the
// over-draw bug caught 2026-05-24: a customer with $30 actual balance
// but a grant carrying $80 remaining-to-drain (because a prior $50
// clawback adjustment reduced balance without bumping any grant's
// consumed_cents) got an $80 invoice draw applied at face value,
// driving the ledger to -$50.
//
// The fix caps the FIFO drain at min(invoice_amount, current_balance)
// — currentBalance is the authoritative source of truth, not the sum
// of grants' remaining-to-drain.
func TestApplyToInvoiceAtomic_CapsAtCurrentBalance(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	invoiceStore := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Credit Apply Cap")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_apply_cap", DisplayName: "Apply Cap",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Grant $100. Grant.consumed_cents=0, grant.remaining=$100.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: cust.ID, AmountCents: 10000, Description: "seed",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Clawback $70 via adjustment. Balance drops to $30; grant's
	// consumed_cents unchanged. This is the inconsistency the cap
	// protects against — adjustments reduce balance without
	// attribution to any specific grant.
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		Currency:   "USD",
		CustomerID: cust.ID, AmountCents: -7000, Description: "clawback",
	}); err != nil {
		t.Fatalf("adjust: %v", err)
	}

	bal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.BalanceCents != 3000 {
		t.Fatalf("balance before apply: got %d, want 3000 ($100 grant - $70 clawback)", bal.BalanceCents)
	}

	// Stand up an invoice for $80. Pre-fix: ApplyToInvoiceAtomic would
	// drain $80 from the grant (per-grant remaining = $100), driving
	// balance to -$50. Post-fix: capped at $30, leaves $50 due.
	now := time.Now().UTC()
	dueAt := now.Add(7 * 24 * time.Hour)
	issuedAt := now
	inv, err := invoiceStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      8000,
		TotalAmountCents:   8000,
		AmountDueCents:     8000,
		BillingPeriodStart: now,
		BillingPeriodEnd:   now.Add(30 * 24 * time.Hour),
		IssuedAt:           &issuedAt,
		DueAt:              &dueAt,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	applied, err := svc.ApplyToInvoice(ctx, tenantID, cust.ID, inv.ID, 8000)
	if err != nil {
		t.Fatalf("ApplyToInvoice: %v", err)
	}
	if applied != 3000 {
		t.Errorf("applied: got %d, want 3000 (capped at current balance, not $80 invoice or $100 grant remaining)", applied)
	}

	postBal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance after apply: %v", err)
	}
	if postBal.BalanceCents != 0 {
		t.Errorf("balance after apply: got %d, want 0 (full $30 drained, never negative)", postBal.BalanceCents)
	}

	postInv, err := invoiceStore.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get invoice after apply: %v", err)
	}
	if postInv.AmountDueCents != 5000 {
		t.Errorf("invoice amount_due after apply: got %d, want 5000 ($80 - $30 credit; PaymentIntent picks up the rest)", postInv.AmountDueCents)
	}
	if postInv.CreditsAppliedCents != 3000 {
		t.Errorf("invoice credits_applied: got %d, want 3000", postInv.CreditsAppliedCents)
	}

	// After the apply, the grant should have consumed_cents = grant
	// amount (100) because the clawback drained $70 + the invoice
	// drained $30 = full $100. No remaining-to-drain, no expiry liability.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var consumed int64
	if err := tx.QueryRow(`
		SELECT consumed_cents FROM customer_credit_ledger
		WHERE customer_id = $1 AND entry_type = 'grant'
	`, cust.ID).Scan(&consumed); err != nil {
		t.Fatalf("read grant consumed_cents: %v", err)
	}
	if consumed != 10000 {
		t.Errorf("grant consumed_cents after clawback+apply: got %d, want 10000 (per-block attribution restored)", consumed)
	}
}

// TestApplyToInvoiceAtomic_DriftIsCappedNotNegative reproduces the live-DB
// drift the 2026-06 audit found: a legacy negative ledger entry (from before
// clawback attribution shipped, or migration 0092's unbackfilled
// consumed_cents) that lowered the SUM(amount_cents) balance WITHOUT bumping
// any block's consumed_cents — so the positive blocks hold MORE remaining than
// the balance. Pre-cap, a large invoice drained the full block remaining and
// wrote a negative balance_after. Post-cap, the drain is capped at the
// authoritative balance: never negative.
func TestApplyToInvoiceAtomic_DriftIsCappedNotNegative(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	invoiceStore := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Credit Drift Cap")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_drift_cap", DisplayName: "Drift Cap",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Grant $100 — block: amount 10000, consumed 0, remaining 10000.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: cust.ID, AmountCents: 10000, Description: "seed",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Inject UNATTRIBUTED drift: a raw -$70 ledger row with NO block
	// attribution (consumed_cents untouched) — the legacy/0092 state the cap
	// defends against. The public AdjustAtomic path can no longer create this
	// (it FIFO-drains blocks); we go around it with a direct INSERT. Balance
	// SUM drops to 3000 while the block still shows 10000 remaining. Inserted
	// via TxTenant so the set_livemode trigger derives livemode from the
	// session (false), keeping the row visible to the test's reads.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO customer_credit_ledger
			(tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description, created_at)
		VALUES ($1, $2, 'adjustment', 'USD', -7000, 3000, 'legacy unattributed clawback', now())
	`, tenantID, cust.ID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert drift entry: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit drift entry: %v", err)
	}

	bal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.BalanceCents != 3000 {
		t.Fatalf("balance before apply: got %d, want 3000 ($100 grant - $70 unattributed)", bal.BalanceCents)
	}

	// Apply an $80 invoice. Pre-cap: drains the full $80 from the
	// $100-remaining block → balance_after -$50. Post-cap: drains only $30
	// (the balance) → balance 0, never negative; $50 left for the PaymentIntent.
	now := time.Now().UTC()
	dueAt := now.Add(7 * 24 * time.Hour)
	issuedAt := now
	inv, err := invoiceStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      8000,
		TotalAmountCents:   8000,
		AmountDueCents:     8000,
		BillingPeriodStart: now,
		BillingPeriodEnd:   now.Add(30 * 24 * time.Hour),
		IssuedAt:           &issuedAt,
		DueAt:              &dueAt,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	applied, err := svc.ApplyToInvoice(ctx, tenantID, cust.ID, inv.ID, 8000)
	if err != nil {
		t.Fatalf("ApplyToInvoice: %v", err)
	}
	if applied != 3000 {
		t.Errorf("applied: got %d, want 3000 (capped at balance, NOT the $100 block remaining)", applied)
	}

	postBal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance after apply: %v", err)
	}
	if postBal.BalanceCents < 0 {
		t.Errorf("balance went NEGATIVE (%d) — the over-drain bug the cap prevents", postBal.BalanceCents)
	}
	if postBal.BalanceCents != 0 {
		t.Errorf("balance after apply: got %d, want 0 (full $30 drained, capped)", postBal.BalanceCents)
	}

	postInv, err := invoiceStore.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get invoice after apply: %v", err)
	}
	if postInv.AmountDueCents != 5000 {
		t.Errorf("invoice amount_due after apply: got %d, want 5000 ($80 - $30 capped credit)", postInv.AmountDueCents)
	}
}

// TestAdjustAtomic_ClawbackAttributesToGrants is the per-block
// attribution regression test. Pre-fix: a negative Adjust dropped
// balance but didn't bump any grant's consumed_cents, so subsequent
// expiry or apply over-drained. Post-fix: clawback FIFO-drains across
// positive blocks just like usage.
func TestAdjustAtomic_ClawbackAttributesToGrants(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	tenantID := testutil.CreateTestTenant(t, db, "Clawback Attribution")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_clawback_attr", DisplayName: "Clawback",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Two grants seeded earliest-first; FIFO drains oldest first.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: cust.ID, AmountCents: 5000, Description: "grant A",
	}); err != nil {
		t.Fatalf("grant A: %v", err)
	}
	// Sleep so created_at ordering is deterministic (postgres TIMESTAMPTZ
	// has microsecond precision; back-to-back inserts can collide).
	time.Sleep(2 * time.Millisecond)
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		Currency:   "USD",
		CustomerID: cust.ID, AmountCents: 3000, Description: "grant B",
	}); err != nil {
		t.Fatalf("grant B: %v", err)
	}

	// Clawback $60. Should consume grant A fully ($50) + $10 of grant B.
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		Currency:   "USD",
		CustomerID: cust.ID, AmountCents: -6000, Description: "clawback",
	}); err != nil {
		t.Fatalf("clawback: %v", err)
	}

	// Verify per-block consumed_cents.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`
		SELECT description, amount_cents, consumed_cents
		FROM customer_credit_ledger
		WHERE customer_id = $1 AND entry_type = 'grant'
		ORDER BY created_at, id
	`, cust.ID)
	if err != nil {
		t.Fatalf("query grants: %v", err)
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		desc     string
		amount   int64
		consumed int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.desc, &r.amount, &r.consumed); err != nil {
			t.Fatalf("scan grant: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(got))
	}
	if got[0].consumed != 5000 {
		t.Errorf("grant A consumed: got %d, want 5000 (fully drained by clawback)", got[0].consumed)
	}
	if got[1].consumed != 1000 {
		t.Errorf("grant B consumed: got %d, want 1000 ($60 clawback = $50 A + $10 B)", got[1].consumed)
	}

	// Balance reconciles.
	bal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.BalanceCents != 2000 {
		t.Errorf("balance after clawback: got %d, want 2000 ($50 + $30 - $60 = $20)", bal.BalanceCents)
	}
}

// TestApplyToInvoiceAtomic_DrainsPositiveAdjustments verifies that a
// positive adjustment (goodwill credit) is now drainable by
// subsequent invoice applications. Pre-fix the apply drain query was
// scoped to entry_type='grant' only, so positive adjustments
// inflated balance but never funded invoices.
func TestApplyToInvoiceAtomic_DrainsPositiveAdjustments(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	invoiceStore := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Adj Drain")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_adj_drain", DisplayName: "Adj Drain",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Goodwill credit via positive adjustment — no Grant.
	if _, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
		Currency:   "USD",
		CustomerID: cust.ID, AmountCents: 5000, Description: "goodwill",
	}); err != nil {
		t.Fatalf("positive adjust: %v", err)
	}

	now := time.Now().UTC()
	dueAt := now.Add(7 * 24 * time.Hour)
	issuedAt := now
	inv, err := invoiceStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		Status:             domain.InvoiceDraft,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      3000,
		TotalAmountCents:   3000,
		AmountDueCents:     3000,
		BillingPeriodStart: now,
		BillingPeriodEnd:   now.Add(30 * 24 * time.Hour),
		IssuedAt:           &issuedAt,
		DueAt:              &dueAt,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	applied, err := svc.ApplyToInvoice(ctx, tenantID, cust.ID, inv.ID, 3000)
	if err != nil {
		t.Fatalf("ApplyToInvoice: %v", err)
	}
	if applied != 3000 {
		t.Errorf("applied: got %d, want 3000 (positive adjustment is drainable)", applied)
	}
}
