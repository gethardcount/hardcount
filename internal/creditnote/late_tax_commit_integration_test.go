package creditnote_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// storingCommitter stands in for the engine's CommitTax: the provider call
// succeeds and the transaction id is stored on the invoice.
type storingCommitter struct {
	invoices *invoice.PostgresStore
	txID     string
	commits  int
}

func (c *storingCommitter) CommitTax(ctx context.Context, tenantID, invoiceID, _ string) error {
	c.commits++
	return c.invoices.SetTaxTransaction(ctx, tenantID, invoiceID, c.txID)
}

func (c *storingCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return nil
}

// An invoice's tax commit fails for days (a Stripe key that cannot write tax
// transactions). On day 1 a credit note is issued on it: the note owes a tax
// reversal, but there is no tax transaction to reverse yet, so the note waits,
// marked and unlisted. On day 3 the key is fixed.
//
// The commit sweep must still take the invoice (its calculation is good for
// 90 days), and the note's reversal must then be made against the transaction
// the commit recorded. Before, the commit list dropped the invoice 24 hours
// after its last write: the tax was never recorded, and the note's reversal
// was never made.
func TestCreditNoteOnAnInvoiceCommittedLate_ItsReversalIsMade(t *testing.T) {
	f := newReversalFixture(t, "CN Late Commit")
	inv := f.invoice("INV-LATE-COMMIT", "stripe_tax", 1000, "")
	execBypassCN(t, f.db, `
		UPDATE invoices SET tax_status = 'ok', tax_calculation_id = 'taxcalc_late', is_simulated = false,
		       issued_at = now() - interval '3 days', updated_at = now() - interval '3 days'
		 WHERE id = $1`, inv.ID)
	execBypassCN(t, f.db, `
		INSERT INTO tax_calculations (tenant_id, invoice_id, provider, provider_ref, request, response, created_at)
		SELECT tenant_id, id, 'stripe_tax', 'taxcalc_late', '{}'::jsonb,
		       jsonb_build_object('id', 'taxcalc_late', 'expires_at', extract(epoch from now() + interval '87 days')::bigint),
		       now() - interval '3 days'
		  FROM invoices WHERE id = $1`, inv.ID)

	// Day 1: the credit note is issued while the commit is still owed.
	draft, err := f.notes.Create(f.ctx, f.tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: f.custID, CreditNoteNumber: "CN-LATE-COMMIT",
		Status: domain.CreditNoteDraft, Reason: "fraudulent",
		SubtotalCents: 5000, TaxAmountCents: 500, TotalCents: 5500,
		CreditAmountCents: 5500,
		Currency:          "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	notes := creditnote.NewService(f.notes, f.invoices, nil, okGranterTx{})
	noReversalYet := &recordingReverser{id: "tx_must_not_be_made"}
	notes.SetTaxReverser(noReversalYet)
	if _, err := notes.Issue(f.ctx, f.tenantID, draft.ID); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	execBypassCN(t, f.db, `UPDATE credit_notes SET issued_at = now() - interval '2 days', updated_at = now() - interval '2 days' WHERE id = $1`, draft.ID)
	if got := f.note(draft.ID); !got.TaxReversalPending || got.TaxTransactionID != "" {
		t.Fatalf("day 1: pending=%v tx=%q, want the reversal marked owed and none made", got.TaxReversalPending, got.TaxTransactionID)
	}
	if len(noReversalYet.requests) != 0 {
		t.Fatalf("day 1: %d reversal(s) sent with no tax transaction on the invoice, want none", len(noReversalYet.requests))
	}
	if f.listed(draft.ID) {
		t.Fatal("day 1: the note must wait unlisted until its invoice has a tax transaction")
	}
	if n, sweepErrs := notes.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 0 || len(sweepErrs) != 0 {
		t.Fatalf("day 1 sweep: reversed=%d errs=%v, want nothing to do", n, sweepErrs)
	}

	// Day 3: the commit sweep still takes the invoice, and the commit lands.
	committer := &storingCommitter{invoices: f.invoices, txID: "tx_committed_day_3"}
	invoices := invoice.NewService(f.invoices, clock.Real(), nil)
	invoices.SetTaxCommitter(committer)
	if n, sweepErrs := invoices.RetryPendingTaxCommit(f.ctx, 50); n != 1 || len(sweepErrs) != 0 || committer.commits != 1 {
		t.Fatalf("day 3 commit sweep: recovered=%d errs=%v commits=%d, want the three-day-old invoice committed once", n, sweepErrs, committer.commits)
	}

	// The note's reversal is now due, against the transaction just recorded.
	if !f.listed(draft.ID) {
		t.Fatal("after the commit: the note must be listed for the reversal sweep")
	}
	rev := &recordingReverser{id: "tx_reversal_after_late_commit"}
	notes.SetTaxReverser(rev)
	if n, sweepErrs := notes.RetryPendingCreditNoteTaxReversal(f.ctx, 50); n != 1 || len(sweepErrs) != 0 {
		t.Fatalf("reversal sweep: reversed=%d errs=%v, want 1 and none", n, sweepErrs)
	}
	if len(rev.requests) != 1 || rev.requests[0].OriginalTransactionID != "tx_committed_day_3" || rev.requests[0].GrossAmountCents != 5500 {
		t.Fatalf("reversal requests = %+v, want one, against the late commit's transaction, for the note's total", rev.requests)
	}
	got := f.note(draft.ID)
	if got.TaxTransactionID != "tx_reversal_after_late_commit" || got.TaxReversalPending {
		t.Fatalf("after the reversal: tx=%q pending=%v, want the reversal stored and the marker cleared", got.TaxTransactionID, got.TaxReversalPending)
	}
}
