package invoice

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestRetryPendingTaxReversal drives the recovery sweep for a void whose
// upstream tax reversal failed: the symmetric sibling of RetryPendingTaxCommit.
// A pending row (voided + stripe_tax + tax_transaction_id set + tax_reversed_at
// NULL) is re-driven through reverseInvoiceTax; on success it stamps
// tax_reversed_at and falls out of the next scan; on failure it stays pending
// for the next tick and the error is aggregated.
func TestRetryPendingTaxReversal(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), true)

	seed := func() (*Service, *memStore, *fakeTaxReverser) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		rev := &fakeTaxReverser{}
		svc.SetTaxReverser(rev)
		// A voided stripe_tax invoice with a committed transaction and no
		// confirmed reversal — exactly what the sweep must pick up.
		store.invoices["inv_rev"] = domain.Invoice{
			ID: "inv_rev", TenantID: "t1", CustomerID: "c",
			Status:           domain.InvoiceVoided,
			TaxFacts:         domain.TaxFacts{TaxProvider: "stripe_tax", TaxStatus: domain.InvoiceTaxOK},
			TaxTransactionID: "tx_1",
			TotalAmountCents: 1000, AmountDueCents: 1000,
		}
		return svc, store, rev
	}

	t.Run("success re-reverses and stamps the marker (falls out of scan)", func(t *testing.T) {
		svc, store, rev := seed()

		recovered, errs := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered != 1 || len(errs) != 0 {
			t.Fatalf("recovered=%d errs=%d, want 1/0", recovered, len(errs))
		}
		if len(rev.calls) != 1 {
			t.Errorf("ReverseTax calls=%d, want 1", len(rev.calls))
		}
		if store.invoices["inv_rev"].TaxReversedAt == nil {
			t.Error("tax_reversed_at must be stamped after a successful re-reversal")
		}
		// Second sweep: nothing pending (marker stamped).
		recovered2, _ := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered2 != 0 {
			t.Errorf("second sweep recovered=%d, want 0 (row should have fallen out)", recovered2)
		}
	})

	t.Run("failure leaves the row pending and aggregates the error", func(t *testing.T) {
		svc, store, rev := seed()
		rev.failErr = errors.New("stripe 503")

		recovered, errs := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered != 0 || len(errs) != 1 {
			t.Fatalf("recovered=%d errs=%d, want 0/1", recovered, len(errs))
		}
		if store.invoices["inv_rev"].TaxReversedAt != nil {
			t.Error("tax_reversed_at must stay NULL on a failed reversal (re-scanned next tick)")
		}
		// Recovers once Stripe is healthy again.
		rev.failErr = nil
		recovered2, errs2 := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered2 != 1 || len(errs2) != 0 {
			t.Fatalf("retry after recovery: recovered=%d errs=%d, want 1/0", recovered2, len(errs2))
		}
	})

	t.Run("no reverser wired → inert", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		store.invoices["inv_rev"] = domain.Invoice{
			ID: "inv_rev", TenantID: "t1", Status: domain.InvoiceVoided,
			TaxFacts: domain.TaxFacts{TaxProvider: "stripe_tax"}, TaxTransactionID: "tx_1", TotalAmountCents: 1000, AmountDueCents: 1000,
		}
		recovered, errs := svc.RetryPendingTaxReversal(ctx, 50)
		if recovered != 0 || len(errs) != 0 {
			t.Fatalf("unwired reverser must no-op; recovered=%d errs=%d", recovered, len(errs))
		}
	})
}
