package credit_test

import (
	"context"
	"sync"
	"testing"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestAppendEntry_SerializesOnEmptyLedger covers the pass-3 low audit finding:
// the serialization lock was a `SELECT ... FOR UPDATE` over
// customer_credit_ledger, which matches zero rows for a customer with an empty
// ledger — so the first concurrent grants acquired no lock and computed
// balance_after off the same stale (zero) snapshot. A per-customer advisory
// lock serializes regardless of ledger state.
//
// Two concurrent first-grants ($50 and $30) must serialize: the running
// balance_after of the later entry must reflect BOTH (== $80), never just its
// own amount.
func TestAppendEntry_SerializesOnEmptyLedger(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Credit Empty Ledger Lock")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_emptylock", DisplayName: "Empty Lock",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	svc := credit.NewService(credit.NewPostgresStore(db))

	var wg sync.WaitGroup
	for _, amt := range []int64{5000, 3000} {
		wg.Add(1)
		go func(amt int64) {
			defer wg.Done()
			if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
				Currency:   "USD",
				CustomerID: cust.ID, AmountCents: amt, Description: "concurrent first grant",
			}); err != nil {
				t.Errorf("grant %d: %v", amt, err)
			}
		}(amt)
	}
	wg.Wait()

	// Final authoritative balance is order-independent.
	bal, err := testutil.BalanceUSD(svc.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.BalanceCents != 8000 {
		t.Errorf("balance: got %d, want 8000", bal.BalanceCents)
	}

	// The serialization invariant: exactly one entry carries the cumulative
	// running balance ($80). Without the lock, both first-grants computed
	// balance_after off zero and the max would be only $50 (or $30).
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var maxBalanceAfter int64
	if err := tx.QueryRow(`
		SELECT COALESCE(MAX(balance_after), 0) FROM customer_credit_ledger
		WHERE customer_id = $1 AND entry_type = 'grant'
	`, cust.ID).Scan(&maxBalanceAfter); err != nil {
		t.Fatalf("read max balance_after: %v", err)
	}
	if maxBalanceAfter != 8000 {
		t.Errorf("max running balance_after: got %d, want 8000 (the later grant must see the earlier — proves serialization)", maxBalanceAfter)
	}
}
