package invoice_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestUpdateTaxAtomic_RejectsNonDraft is the symmetric counterpart to
// TestAddLineItemAtomic_RejectsNonDraft: the draft-only invariant is
// re-asserted inside the locking tx (the SELECT ... FOR UPDATE in
// UpdateTaxAtomic), so a caller cannot stamp tax onto a finalized invoice —
// even if it raced a concurrent Finalize. Tax is mutable while a draft and
// frozen once finalized; this pins that the store guards it, not just the
// engine layer.
func TestUpdateTaxAtomic_RejectsNonDraft(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "UpdateTax NonDraft")
	invID := seedDraftInvoice(t, db, tenantID)

	if _, err := store.UpdateStatus(ctx, tenantID, invID, domain.InvoiceFinalized); err != nil {
		t.Fatalf("finalize invoice: %v", err)
	}

	_, err := store.UpdateTaxAtomic(ctx, tenantID, invID, domain.InvoiceTaxRetryUpdate{
		TaxFacts:         domain.TaxFacts{TaxAmountCents: 100, TaxStatus: domain.InvoiceTaxOK},
		TotalAmountCents: 600,
		SubtotalCents:    500,
	}, nil)
	if err == nil {
		t.Fatal("expected error updating tax on finalized invoice, got nil")
	}
	// The store raises the draft-only invariant as a typed InvalidState error,
	// not a generic SQL failure.
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState (draft-only rejection), got %v", err)
	}
}

// TestUpdateTaxAtomic_ConcurrentSerializes is the lost-update regression for
// UpdateTaxAtomic, mirroring TestAddLineItemAtomic_ConcurrentAdds. Every call
// bumps tax_retry_count by 1 under the SELECT ... FOR UPDATE row lock, so N
// concurrent calls on one draft invoice must serialize: each increment is
// applied on top of the prior committed value, leaving tax_retry_count == N.
// Without the lock the read-modify-write on the counter would lose increments
// and the final header would be an incoherent partial result.
func TestUpdateTaxAtomic_ConcurrentSerializes(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	store := invoice.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "UpdateTax Concurrency")
	invID := seedDraftInvoice(t, db, tenantID)

	const (
		goroutines = 8
		taxCents   = int64(100)
		// The seeded draft's subtotal: every writer's basis must match the
		// header it locks (#1298), so the subtotal stays put and only the tax
		// and the retry counter move.
		subtotal = int64(0)
		total    = subtotal + taxCents
	)

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for range goroutines {
		wg.Go(func() {
			_, err := store.UpdateTaxAtomic(ctx, tenantID, invID, domain.InvoiceTaxRetryUpdate{
				TaxFacts: domain.TaxFacts{
					TaxAmountCents: taxCents, TaxRate: 20,
					TaxStatus: domain.InvoiceTaxOK,
				},
				SubtotalCents:      subtotal,
				DiscountCents:      0,
				TotalAmountCents:   total,
				BasisSubtotalCents: subtotal,
			}, nil)
			if err != nil {
				errCh <- err
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent UpdateTaxAtomic: %v", err)
	}

	inv, err := store.Get(ctx, tenantID, invID)
	if err != nil {
		t.Fatalf("get final invoice: %v", err)
	}

	// Every call increments tax_retry_count = tax_retry_count + 1 inside its
	// locked tx; if any read the counter before another committed, the final
	// value would be < goroutines (a lost update). Exactly N proves the row
	// lock serialized the read-modify-write.
	if inv.TaxRetryCount != goroutines {
		t.Fatalf("lost-update race: tax_retry_count = %d, want %d (concurrent updates were not serialized on the row lock)",
			inv.TaxRetryCount, goroutines)
	}
	// The serialized winner leaves a coherent header: the last committed
	// snapshot, not a torn mix of two writers.
	if inv.TaxAmountCents != taxCents {
		t.Fatalf("tax_amount_cents = %d, want %d", inv.TaxAmountCents, taxCents)
	}
	if inv.TotalAmountCents != total {
		t.Fatalf("total_amount_cents = %d, want %d", inv.TotalAmountCents, total)
	}
	// amount_due = GREATEST(total - amount_paid - credits_applied, 0); a fresh
	// draft has neither, so it must equal total.
	if inv.AmountDueCents != total {
		t.Fatalf("amount_due_cents = %d, want %d", inv.AmountDueCents, total)
	}
	if inv.TaxStatus != domain.InvoiceTaxOK {
		t.Fatalf("tax_status = %q, want %q", inv.TaxStatus, domain.InvoiceTaxOK)
	}
}
