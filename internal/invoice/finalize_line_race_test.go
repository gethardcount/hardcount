package invoice

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// memAddingTaxRetrier adds a line while the tax is computed, as an operator's
// add landing during the provider call would (#1298).
type memAddingTaxRetrier struct {
	store *memStore
	add   bool
}

func (f *memAddingTaxRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	return f.ComputeTaxForInvoice(ctx, tenantID, invoiceID)
}

func (f *memAddingTaxRetrier) ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	if f.add {
		f.add = false
		if _, _, err := f.store.AddLineItemAtomic(ctx, tenantID, invoiceID, domain.InvoiceLineItem{
			LineType: domain.LineTypeAddOn, Description: "Added during finalize", Quantity: 1,
			UnitAmountCents: 2500, AmountCents: 2500, TotalAmountCents: 2500,
		}); err != nil {
			return domain.Invoice{}, err
		}
	}
	return f.store.Get(ctx, tenantID, invoiceID)
}

// Finalize reads the lines before it computes tax, so a line added during the
// computation makes it refuse: the tax covers only the lines it read.
func TestFinalize_ALineAddedDuringTaxIsRefused(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, clock.Real(), newMemNumberer())
	ctx := context.Background()
	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems:  []AddLineItemInput{{Description: "Setup", Quantity: 1, UnitAmountCents: 5000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	svc.SetTaxRetrier(&memAddingTaxRetrier{store: store, add: true})

	_, err = svc.Finalize(ctx, "t1", inv.ID)
	if errs.Code(err) != "invoice_lines_changed" {
		t.Fatalf("finalize while a line was added: want invoice_lines_changed, got %v", err)
	}
	if got, _ := store.Get(ctx, "t1", inv.ID); got.Status != domain.InvoiceDraft {
		t.Fatalf("status %s, want draft", got.Status)
	}

	final, err := svc.Finalize(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize again: %v", err)
	}
	if final.Status != domain.InvoiceFinalized || final.TotalAmountCents != 7500 {
		t.Fatalf("finalized %s with total %d, want finalized with 7500", final.Status, final.TotalAmountCents)
	}
}
