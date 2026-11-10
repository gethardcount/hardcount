package invoice_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// committingTaxCommitter stands in for billing.Engine.CommitTax under Stripe
// Tax: the commit records the tax transaction on the invoice row.
type committingTaxCommitter struct {
	store *invoice.PostgresStore
	calls int
	// committed is the invoice as the finalize committed it. CommitTax is the
	// first step after the finalize transaction commits, so it reads the row
	// before its own write.
	committed domain.Invoice
}

func (c *committingTaxCommitter) CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	c.calls++
	inv, err := c.store.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return err
	}
	c.committed = inv
	return c.store.SetTaxTransaction(ctx, tenantID, invoiceID, "tx_"+calculationID)
}

func (c *committingTaxCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return nil
}

// TestFinalize_UnderStripeTax_EventIsTheFinalizedRow: invoice.finalized
// carries the invoice as the finalize transaction committed it, with its
// hosted-page token (ADR-131). Under Stripe Tax the tax transaction is
// recorded after that commit, since it is a call to Stripe, so the event
// carries no tax_transaction_id. Finalize returns the invoice as
// GET /v1/invoices/{id} shows it, with the tax transaction.
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
	assertEventIsTheCommittedRow(t, db, tenantID, draft.ID, committer.committed)
	assertIsTheInvoiceNow(t, "Finalize", returned, final)
}

// resolvingRetrier stands in for the billing engine's tax recompute: it
// writes the draft's tax as resolved through the real store's
// UpdateTaxAtomic, keeping its Stripe Tax calculation, and runs RetryTax's
// hook on that transaction, as the engine does.
type resolvingRetrier struct {
	db    *postgres.DB
	store *invoice.PostgresStore
}

func (r resolvingRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string,
	then func(ctx context.Context, tx *sql.Tx, taxed domain.Invoice) error,
) (domain.Invoice, error) {
	inv, err := r.store.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	lines, err := r.store.ListLineItems(ctx, tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	facts := inv.TaxFacts
	facts.TaxStatus, facts.TaxErrorCode, facts.TaxPendingReason, facts.TaxDeferredAt = domain.InvoiceTaxOK, "", "", nil
	return r.store.UpdateTaxAtomic(ctx, tenantID, invoiceID, domain.InvoiceTaxRetryUpdate{
		SubtotalCents: inv.SubtotalCents, DiscountCents: inv.DiscountCents,
		TaxFacts: facts, TotalAmountCents: inv.TotalAmountCents,
		BasisSubtotalCents: inv.SubtotalCents, BasisDiscountCents: inv.DiscountCents,
	}, lines, then)
}

func (r resolvingRetrier) ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	return r.store.Get(ctx, tenantID, invoiceID)
}

// taxPendingCycleDraft seeds a subscription's draft whose Stripe Tax
// calculation is parked on a provider outage, the shape a tax retry
// finalizes on its own.
func taxPendingCycleDraft(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, num string) domain.Invoice {
	t.Helper()
	return seedInvoice(t, db, ctx, tenantID, domain.Invoice{
		InvoiceNumber: num, Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonSubscriptionCycle,
		SubtotalCents: 10000, TotalAmountCents: 10800, AmountDueCents: 10800,
		TaxFacts: domain.TaxFacts{
			TaxAmountCents: 800, TaxProvider: "stripe_tax", TaxCalculationID: "taxcalc_" + num,
			TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage",
		},
	})
}

// TestRetryTax_UnderStripeTax_ResponseIsTheInvoiceNow: a tax retry finalizes a
// subscription's draft that is still owed. POST /v1/invoices/{id}/retry-tax
// responds with what RetryTax returns, so it is the invoice as
// GET /v1/invoices/{id} shows it: with the tax transaction recorded after the
// finalize committed. invoice.finalized stays the row the finalize committed.
func TestRetryTax_UnderStripeTax_ResponseIsTheInvoiceNow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Retry Tax Response")
	store, svc := eventsWired(db)
	committer := &committingTaxCommitter{store: store}
	svc.SetTaxCommitter(committer)
	svc.SetTaxRetrier(resolvingRetrier{db: db, store: store})
	draft := taxPendingCycleDraft(t, db, ctx, tenantID, "HC-RT-RESP-1")

	returned, err := svc.RetryTax(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("retry tax: %v", err)
	}
	final, err := store.Get(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.InvoiceFinalized || final.AmountDueCents <= 0 || final.TaxTransactionID != "tx_taxcalc_HC-RT-RESP-1" {
		t.Fatalf("after the retry: status %s, %d due, tax_transaction_id %q; want finalized, still owed, tx_taxcalc_HC-RT-RESP-1",
			final.Status, final.AmountDueCents, final.TaxTransactionID)
	}
	assertEventIsTheCommittedRow(t, db, tenantID, draft.ID, committer.committed)
	assertIsTheInvoiceNow(t, "RetryTax", returned, final)
}

// assertEventIsTheCommittedRow checks that exactly one invoice.finalized was
// queued for the invoice, with the row the finalize committed.
func assertEventIsTheCommittedRow(t *testing.T, db *postgres.DB, tenantID, invoiceID string, committed domain.Invoice) {
	t.Helper()
	if committed.ID != invoiceID || committed.TaxTransactionID != "" {
		t.Fatalf("committed row %q has tax_transaction_id %q; want the invoice before its tax commit", committed.ID, committed.TaxTransactionID)
	}
	got := queuedEvents(t, db, tenantID, domain.EventInvoiceFinalized, invoiceID)
	if len(got) != 1 {
		t.Fatalf("%d invoice.finalized rows, want 1", len(got))
	}
	if want := asData(t, domain.InvoiceWebhookData(committed, nil)); !reflect.DeepEqual(got[0], want) {
		t.Errorf("invoice.finalized differs from the finalized row:\n got %v\nwant %v", got[0], want)
	}
}

// assertIsTheInvoiceNow checks that what a caller returned is the invoice as
// the database holds it: the tax transaction and updated_at of the writes
// after the finalize commit, and its hosted-page token.
func assertIsTheInvoiceNow(t *testing.T, caller string, returned, now domain.Invoice) {
	t.Helper()
	if returned.TaxTransactionID != now.TaxTransactionID || !returned.UpdatedAt.Equal(now.UpdatedAt) || returned.PublicToken != now.PublicToken {
		t.Errorf("%s returned tax_transaction_id %q updated_at %v token %q; GET shows %q %v %q",
			caller, returned.TaxTransactionID, returned.UpdatedAt, returned.PublicToken,
			now.TaxTransactionID, now.UpdatedAt, now.PublicToken)
	}
}
