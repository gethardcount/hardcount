package creditnote_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCreateUnderInvoiceLock_SerializesCap covers the pass-3 medium audit
// finding: the credit-note total cap was a TOCTOU — two concurrent Create
// calls both read the same pre-state (no existing CNs), both passed the
// "existing + new ≤ invoice total" check, and both inserted, crediting beyond
// the invoice total. CreateUnderInvoiceLock takes a per-invoice advisory lock
// so the second call sees the first's row and is capped.
//
// Invoice total = 10000. Two goroutines each try to create a 6000 CN. With the
// lock exactly one wins (6000 ≤ 10000); the other sees 6000 already and its cap
// rejects 6000+6000 > 10000. Persisted total must be 6000, never 12000.
func TestCreateUnderInvoiceLock_SerializesCap(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "CN Cap TOCTOU")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cap", DisplayName: "Cap",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-CAP-1",
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

	store := creditnote.NewPostgresStore(db)

	const amount = 6000
	build := func(i int) func(_ *sql.Tx, existing []domain.CreditNote) (domain.CreditNote, error) {
		return func(_ *sql.Tx, existing []domain.CreditNote) (domain.CreditNote, error) {
			var existingTotal int64
			for _, cn := range existing {
				if cn.Status != domain.CreditNoteVoided {
					existingTotal += cn.TotalCents
				}
			}
			if existingTotal+amount > inv.TotalAmountCents {
				return domain.CreditNote{}, fmt.Errorf("over cap")
			}
			return domain.CreditNote{
				InvoiceID:         inv.ID,
				CustomerID:        cust.ID,
				CreditNoteNumber:  fmt.Sprintf("CN-CAP-%d", i),
				Status:            domain.CreditNoteDraft,
				Reason:            "concurrent",
				SubtotalCents:     amount,
				TotalCents:        amount,
				CreditAmountCents: amount,
				Currency:          "USD",
				RefundStatus:      domain.RefundNone,
			}, nil
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.CreateUnderInvoiceLock(ctx, tenantID, inv.ID, nil, build(i)); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("successes: got %d, want 1 (lock must serialize the cap)", successes)
	}

	// Persisted credit-note total must not exceed the invoice total.
	all, err := store.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: inv.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var persisted int64
	for _, cn := range all {
		persisted += cn.TotalCents
	}
	if persisted != amount {
		t.Errorf("persisted CN total: got %d, want %d (never the un-capped 12000)", persisted, amount)
	}
}
