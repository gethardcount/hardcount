package payment_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestTokenConsume_AtomicSingleUse covers the pass-3 low [security] audit
// finding: the payment-update token's validate→mark-used was a TOCTOU, so two
// concurrent requests for the same single-use token both passed Validate's
// `used_at IS NULL` read and both opened a setup-mode checkout session. Consume
// is now an atomic compare-and-swap (UPDATE ... WHERE used_at IS NULL); exactly
// one caller wins.
func TestTokenConsume_AtomicSingleUse(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Token TOCTOU")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_tok", DisplayName: "Tok",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-TOK-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 100, TotalAmountCents: 100, AmountDueCents: 100,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	tokens := payment.NewTokenService(db)
	rawToken, err := tokens.Create(ctx, tenantID, cust.ID, inv.ID)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	// Two concurrent consumers race for the same token.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := tokens.Consume(ctx, tenantID, rawToken)
			if err == nil && ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("concurrent Consume wins: got %d, want 1 (single-use must be atomic)", wins)
	}

	// A subsequent Consume must also lose.
	if ok, err := tokens.Consume(ctx, tenantID, rawToken); err != nil || ok {
		t.Errorf("post-consume Consume: got ok=%v err=%v, want ok=false", ok, err)
	}
}
