package payment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// seedClaimInvoice inserts a minimal customer + invoice row in the given
// state so the claim protocol can be exercised without the full invoice
// service stack.
func seedClaimInvoice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, status, payStatus string, due int64) string {
	t.Helper()
	custID := postgres.NewID("hc_cus")
	invID := postgres.NewID("hc_inv")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email, created_at, updated_at)
		VALUES ($1, $2, $3, 'Claim Test', '', $4, $4)
	`, custID, tenantID, "cus-"+invID, now); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status,
			currency, subtotal_cents, total_amount_cents, amount_due_cents, tax_status,
			billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'USD', $7, $7, $7, 'ok', $8, $8, $8, $8)
	`, invID, tenantID, custID, "INV-"+invID[len(invID)-6:], status, payStatus, due, now); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return invID
}

// TestClaimOpen_ConcurrentDoublePOST_OneWinner is the panel's core dedup
// invariant on real Postgres: two racing claims for one invoice yield
// exactly one winner; the loser receives the WINNER's claim (whose id
// derives the Stripe idempotency key, so both converge on one session).
func TestClaimOpen_ConcurrentDoublePOST_OneWinner(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Race")
	invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 10000)
	store := NewCheckoutSessionStore(db)

	type res struct {
		claim  CheckoutClaim
		winner bool
		err    error
	}
	results := make([]res, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			<-start
			c, w, err := store.ClaimOpen(ctx, tenantID, invID, 10000, "USD", false)
			results[slot] = res{c, w, err}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("racer %d: %v", i, r.err)
		}
	}
	winners := 0
	for _, r := range results {
		if r.winner {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (partial unique index is the guard)", winners)
	}
	if results[0].claim.ID != results[1].claim.ID {
		t.Fatalf("racers hold different claims (%s vs %s) — they would mint two Stripe sessions",
			results[0].claim.ID, results[1].claim.ID)
	}
}

// TestClaimOpen_PayableRecheck: the claim tx re-verifies the invoice under
// FOR SHARE — paid/voided → ErrInvoiceNotPayable; a processing charge →
// ErrChargeInFlight (dunning-retry racing the Pay click); a stale caller
// amount is re-anchored to the locked row's amount_due.
func TestClaimOpen_PayableRecheck(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Recheck")
	store := NewCheckoutSessionStore(db)

	paid := seedClaimInvoice(t, db, ctx, tenantID, "paid", "succeeded", 0)
	if _, _, err := store.ClaimOpen(ctx, tenantID, paid, 1000, "USD", false); !errors.Is(err, ErrInvoiceNotPayable) {
		t.Fatalf("paid invoice: err = %v, want ErrInvoiceNotPayable", err)
	}

	processing := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "processing", 5000)
	if _, _, err := store.ClaimOpen(ctx, tenantID, processing, 5000, "USD", false); !errors.Is(err, ErrChargeInFlight) {
		t.Fatalf("processing invoice: err = %v, want ErrChargeInFlight", err)
	}

	fresh := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 7500)
	claim, winner, err := store.ClaimOpen(ctx, tenantID, fresh, 9999, "USD", false) // stale caller amount
	if err != nil || !winner {
		t.Fatalf("fresh claim: winner=%v err=%v", winner, err)
	}
	if claim.AmountCents != 7500 {
		t.Fatalf("claim amount = %d, want 7500 (re-anchored on the locked row, not the caller's stale read)", claim.AmountCents)
	}
}

// TestSupersede_CASExactlyOnce: two racers superseding one open claim —
// exactly one wins the remint right.
func TestSupersede_CASExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Supersede CAS")
	invID := seedClaimInvoice(t, db, ctx, tenantID, "finalized", "pending", 4000)
	store := NewCheckoutSessionStore(db)
	claim, _, err := store.ClaimOpen(ctx, tenantID, invID, 4000, "USD", false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	wins := make([]bool, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			<-start
			w, err := store.Supersede(ctx, tenantID, claim.ID)
			if err != nil {
				t.Errorf("supersede %d: %v", slot, err)
			}
			wins[slot] = w
		}(i)
	}
	close(start)
	wg.Wait()
	if wins[0] == wins[1] {
		t.Fatalf("supersede CAS: wins = %v, want exactly one true", wins)
	}
}
