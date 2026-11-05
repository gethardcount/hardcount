package invoice_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// committingTaxCommitter stands in for billing.Engine.CommitTax under Stripe
// Tax: the commit records the tax transaction on the invoice row.
type committingTaxCommitter struct {
	store interface {
		SetTaxTransaction(ctx context.Context, tenantID, id, taxTransactionID string) error
	}
	calls int
}

func (c *committingTaxCommitter) CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	c.calls++
	return c.store.SetTaxTransaction(ctx, tenantID, invoiceID, "tx_"+calculationID)
}

func (c *committingTaxCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return nil
}

// TestFinalize_UnderStripeTax_EventIsTheFinalizedRow: invoice.finalized
// carries the invoice as the finalize transaction committed it, with its
// hosted-page token (ADR-131). Under Stripe Tax the tax transaction is
// recorded after that commit, since it is a call to Stripe, so the event
// carries no tax_transaction_id and GET /v1/invoices/{id} shows it. Finalize
// returns the row it committed.
func TestFinalize_UnderStripeTax_EventIsTheFinalizedRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Finalize Webhook Tax")
	store, svc := eventsWired(db)
	committer := &committingTaxCommitter{store: store}
	svc.SetTaxCommitter(committer)
	draft := seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: "HC-FIN-TAX-1", Status: domain.InvoiceDraft,
		SubtotalCents: 10000, TotalAmountCents: 10800, AmountDueCents: 10800,
		TaxFacts: domain.TaxFacts{TaxAmountCents: 800, TaxProvider: "stripe_tax", TaxCalculationID: "taxcalc_fin_1"},
	})

	returned, err := svc.Finalize(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	final, err := store.Get(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committer.calls != 1 || final.TaxTransactionID != "tx_taxcalc_fin_1" || final.PublicToken == "" {
		t.Fatalf("finalize side writes: %d commits, tax_transaction_id %q, token set %v; want 1 commit, tx_taxcalc_fin_1, a token",
			committer.calls, final.TaxTransactionID, final.PublicToken != "")
	}
	got := queuedEvents(t, db, tenantID, domain.EventInvoiceFinalized, draft.ID)
	if len(got) != 1 {
		t.Fatalf("%d invoice.finalized rows, want 1", len(got))
	}
	// The committed row is the row now, without the tax commit's two writes.
	committed := final
	committed.TaxTransactionID = ""
	committed.UpdatedAt = returned.UpdatedAt
	if want := asData(t, domain.InvoiceWebhookData(committed, nil)); !reflect.DeepEqual(got[0], want) {
		t.Errorf("invoice.finalized differs from the finalized row:\n got %v\nwant %v", got[0], want)
	}
	if returned.TaxTransactionID != "" || returned.PublicToken != final.PublicToken {
		t.Errorf("Finalize returned tax_transaction_id %q token %q; want the finalized row: no tax transaction yet, token %q",
			returned.TaxTransactionID, returned.PublicToken, final.PublicToken)
	}
}
