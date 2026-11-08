package invoice_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// stampingTaxReverser stands in for Stripe Tax: every reversal succeeds.
type stampingTaxReverser struct{ calls int }

func (r *stampingTaxReverser) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	r.calls++
	return &tax.ReversalResult{TransactionID: "taxrev_" + req.Reference}, nil
}

// TestVoid_UnderStripeTax_EventIsTheVoidedRow: invoice.voided carries the
// invoice as the void's transaction committed it (ADR-131). Under a tax
// provider the void then reverses the tax, a call to Stripe, and stamps
// tax_reversed_at after the commit, so the event never carries that stamp
// and GET /v1/invoices/{id} shows it.
func TestVoid_UnderStripeTax_EventIsTheVoidedRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Void Webhook Tax")
	store, svc := eventsWired(db)
	rev := &stampingTaxReverser{}
	svc.SetTaxReverser(rev)
	inv := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "HC-VOID-TAX-1", Status: domain.InvoiceFinalized,
		SubtotalCents: 10800, TotalAmountCents: 10800, AmountDueCents: 10800,
	})
	if err := store.SetTaxTransaction(ctx, tenantID, inv.ID, "tx_void_tax"); err != nil {
		t.Fatalf("set tax transaction: %v", err)
	}

	voided, err := svc.Void(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	final, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.calls != 1 || final.TaxReversedAt == nil {
		t.Fatalf("tax reversal: %d calls, tax_reversed_at %v; want 1 call and a stamp", rev.calls, final.TaxReversedAt)
	}
	got := queuedEvents(t, db, tenantID, domain.EventInvoiceVoided, inv.ID)
	if len(got) != 1 {
		t.Fatalf("%d invoice.voided rows, want 1", len(got))
	}
	// The voided row is the row now, without the stamp the reversal wrote.
	committed := final
	committed.TaxReversedAt = nil
	if want := asData(t, domain.InvoiceWebhookData(committed, nil)); !reflect.DeepEqual(got[0], want) {
		t.Errorf("invoice.voided differs from the voided row:\n got %v\nwant %v", got[0], want)
	}
	if voided.TaxReversedAt != nil || !voided.UpdatedAt.Equal(final.UpdatedAt) {
		t.Errorf("Void returned tax_reversed_at %v updated_at %v; want the voided row: no stamp, updated_at %v",
			voided.TaxReversedAt, voided.UpdatedAt, final.UpdatedAt)
	}
}
