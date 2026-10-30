package invoice

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// #1298: finalizing a manual invoice reads its lines, calls the tax provider,
// writes the tax, then finalizes, in separate transactions. A line added in
// any of those gaps was left out of the total (the tax write put back the
// older subtotal) or finalized untaxed, and a commit line's credit was
// granted without its charge. Each write now checks, under the invoice lock,
// that the lines are the ones read, and refuses otherwise.

func raceLine(desc string, cents int64) domain.InvoiceLineItem {
	return domain.InvoiceLineItem{
		LineType: domain.LineTypeAddOn, Description: desc, Quantity: 1,
		UnitAmountCents: cents, AmountCents: cents, TotalAmountCents: cents,
	}
}

func (r *snapRig) manualDraft(t *testing.T, num string, lines ...domain.InvoiceLineItem) domain.Invoice {
	t.Helper()
	p := r.protoInvoice(num, domain.InvoiceDraft, domain.BillingReasonManual)
	p.IssuedAt = nil
	var sum int64
	for _, l := range lines {
		sum += l.AmountCents
	}
	p.SubtotalCents, p.TotalAmountCents, p.AmountDueCents = sum, sum, sum
	inv, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, lines)
	if err != nil {
		t.Fatalf("create draft %s: %v", num, err)
	}
	return inv
}

func wantLinesChanged(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want the invoice_lines_changed refusal, got nil", what)
	}
	if errs.Code(err) != "invoice_lines_changed" {
		t.Fatalf("%s: want code invoice_lines_changed, got %v", what, err)
	}
}

// A line added between the computation's reads and its tax write: the write
// is refused and the header keeps the line in its total.
func TestUpdateTaxAtomic_RefusesWhenALineWasAddedMeanwhile(t *testing.T) {
	r := newSnapRig(t, "Finalize Race Tax Corp")
	d := r.manualDraft(t, "RACE-TAX-1", raceLine("Setup", 5000))

	// What the computation read before calling the provider.
	read, err := r.store.Get(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	items, err := r.store.ListLineItems(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	update := func(inv domain.Invoice) domain.InvoiceTaxRetryUpdate {
		return domain.InvoiceTaxRetryUpdate{
			SubtotalCents: inv.SubtotalCents, TotalAmountCents: inv.SubtotalCents,
			TaxFacts:           domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
			BasisSubtotalCents: inv.SubtotalCents, BasisDiscountCents: inv.DiscountCents,
			InitialAttempt: true,
		}
	}

	// The operator adds a line while the provider answers.
	if _, _, err := r.store.AddLineItemAtomic(r.ctx, r.tenantID, d.ID, raceLine("Extra seats", 2500)); err != nil {
		t.Fatalf("add line: %v", err)
	}

	_, err = r.store.UpdateTaxAtomic(r.ctx, r.tenantID, d.ID, update(read), items)
	wantLinesChanged(t, err, "tax write over a stale read")

	after, err := r.store.Get(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.SubtotalCents != 7500 || after.TotalAmountCents != 7500 {
		t.Fatalf("header subtotal %d total %d, want 7500 and 7500: the added line must stay in the total",
			after.SubtotalCents, after.TotalAmountCents)
	}

	// The header basis is current now, but the lines are the ones read before
	// the add, one short. Same refusal.
	_, err = r.store.UpdateTaxAtomic(r.ctx, r.tenantID, d.ID, update(after), items)
	wantLinesChanged(t, err, "tax write with the old line set")

	// Read again, and the write goes through.
	fresh, err := r.store.ListLineItems(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("list lines again: %v", err)
	}
	taxed, err := r.store.UpdateTaxAtomic(r.ctx, r.tenantID, d.ID, update(after), fresh)
	if err != nil {
		t.Fatalf("tax write over a fresh read: %v", err)
	}
	if taxed.TotalAmountCents != 7500 {
		t.Fatalf("taxed total %d, want 7500", taxed.TotalAmountCents)
	}
}

// addingTaxRetrier stands in for the tax computation and adds a line while it
// runs, as an operator's add landing during the provider call would.
type addingTaxRetrier struct {
	store *PostgresStore
	add   bool
}

func (f *addingTaxRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	return f.ComputeTaxForInvoice(ctx, tenantID, invoiceID)
}

func (f *addingTaxRetrier) ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	if f.add {
		f.add = false
		if _, _, err := f.store.AddLineItemAtomic(ctx, tenantID, invoiceID, raceLine("Added during finalize", 2500)); err != nil {
			return domain.Invoice{}, err
		}
	}
	return f.store.Get(ctx, tenantID, invoiceID)
}

// A line added after Finalize read the lines: Finalize refuses and the
// invoice stays a draft. Finalizing again includes the line.
func TestFinalize_RefusesWhenALineWasAddedDuringIt(t *testing.T) {
	r := newSnapRig(t, "Finalize Race Corp")
	d := r.manualDraft(t, "RACE-FIN-1", raceLine("Setup", 5000))
	r.svc.SetTaxRetrier(&addingTaxRetrier{store: r.store, add: true})

	_, err := r.svc.Finalize(r.ctx, r.tenantID, d.ID)
	wantLinesChanged(t, err, "finalize while a line was added")
	still, err := r.store.Get(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if still.Status != domain.InvoiceDraft {
		t.Fatalf("status %s, want draft: nothing may be finalized on the old line set", still.Status)
	}

	final, err := r.svc.Finalize(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("finalize again: %v", err)
	}
	if final.Status != domain.InvoiceFinalized || final.TotalAmountCents != 7500 {
		t.Fatalf("finalized %s with total %d, want finalized with 7500", final.Status, final.TotalAmountCents)
	}
}

// The store's own check, with the line ids Finalize passes.
func TestFinalizeWithDates_RefusesAnUnreadLine(t *testing.T) {
	r := newSnapRig(t, "Finalize Race Store Corp")
	d := r.manualDraft(t, "RACE-FWD-1", raceLine("Setup", 5000))
	lines, err := r.store.ListLineItems(r.ctx, r.tenantID, d.ID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	read := []string{lines[0].ID}
	added, _, err := r.store.AddLineItemAtomic(r.ctx, r.tenantID, d.ID, raceLine("Extra", 2500))
	if err != nil {
		t.Fatalf("add line: %v", err)
	}
	issued, due := lines[0].CreatedAt, lines[0].CreatedAt.AddDate(0, 0, 30)

	_, err = r.store.FinalizeWithDates(r.ctx, r.tenantID, d.ID, issued, due, read, nil)
	wantLinesChanged(t, err, "finalize with an unread line")

	inv, err := r.store.FinalizeWithDates(r.ctx, r.tenantID, d.ID, issued, due, append(read, added.ID), nil)
	if err != nil {
		t.Fatalf("finalize with every line: %v", err)
	}
	if inv.Status != domain.InvoiceFinalized {
		t.Fatalf("status %s, want finalized", inv.Status)
	}
}
