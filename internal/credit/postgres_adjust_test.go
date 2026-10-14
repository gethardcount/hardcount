package credit_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestAdjustAtomic_NoOversellUnderContention is the regression test for
// COR-4b: Adjust previously read the balance in one tx and appended the
// deduction in another, so two concurrent deductions could each observe
// the full balance, each pass the sufficiency check, and both commit —
// driving the ledger negative. The locked implementation serializes
// deductions on the same customer, so at most one succeeds per grant.
func TestAdjustAtomic_NoOversellUnderContention(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	tenantID := testutil.CreateTestTenant(t, db, "Credit Adjust Race")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_adjust_race", DisplayName: "Adjust Race",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Seed a balance of exactly 100 cents. With 10 goroutines each trying
	// to deduct 20 cents, the safe total is 100 (5 can succeed). The race
	// implementation would let all 10 commit, leaving balance = -100.
	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, AmountCents: 100, Description: "seed",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	const (
		goroutines  = 10
		deductCents = int64(-20)
	)

	var (
		wg        sync.WaitGroup
		successes int64
		mu        sync.Mutex
	)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Adjust(ctx, tenantID, credit.AdjustInput{
				CustomerID:  cust.ID,
				AmountCents: deductCents,
				Description: "contended deduction",
			})
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
				return
			}
			if !strings.Contains(err.Error(), "Insufficient balance") {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	bal, err := svc.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.BalanceCents < 0 {
		t.Fatalf("OVERSELL: balance = %d cents, expected >= 0 (successes=%d)",
			bal.BalanceCents, successes)
	}
	// Total consumed must equal exactly successes * 20. The starting balance
	// was 100, so successes <= 5.
	if successes > 5 {
		t.Fatalf("OVERSELL: %d successes but only 5 fit into balance of 100",
			successes)
	}
	expected := int64(100) + deductCents*successes
	if bal.BalanceCents != expected {
		t.Fatalf("balance = %d, want %d (100 - 20*%d)",
			bal.BalanceCents, expected, successes)
	}
}

// TestAdjustAtomic_DeductionExceedsBalance confirms that a single deduction
// larger than the balance is rejected with an insufficient-balance error,
// not silently applied.
func TestAdjustAtomic_DeductionExceedsBalance(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	creditStore := credit.NewPostgresStore(db)
	svc := credit.NewService(creditStore)
	tenantID := testutil.CreateTestTenant(t, db, "Credit Adjust Oversized")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_oversized", DisplayName: "Oversized Deduction",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	if _, err := svc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, AmountCents: 500, Description: "seed",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	_, err = svc.Adjust(ctx, tenantID, credit.AdjustInput{
		CustomerID:  cust.ID,
		AmountCents: -1000,
		Description: "over-deduct",
	})
	if err == nil {
		t.Fatal("expected insufficient-balance error, got nil")
	}
	if !strings.Contains(err.Error(), "Insufficient balance") {
		t.Fatalf("expected insufficient-balance error, got: %v", err)
	}

	bal, err := svc.GetBalance(ctx, tenantID, cust.ID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if bal.BalanceCents != 500 {
		t.Fatalf("balance must be untouched on rejection: got %d, want 500", bal.BalanceCents)
	}
}
