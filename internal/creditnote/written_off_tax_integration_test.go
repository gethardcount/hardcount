package creditnote_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// taxCalls records every upstream tax reversal, from a credit note or a void.
type taxCalls struct{ calls []tax.ReversalRequest }

func (r *taxCalls) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	r.calls = append(r.calls, req)
	return &tax.ReversalResult{TransactionID: "tx_rev_" + req.CreditNoteID + req.Reference}, nil
}

func (r *taxCalls) grossReversed(t *testing.T, total int64) int64 {
	t.Helper()
	var sum int64
	for _, c := range r.calls {
		switch c.Mode {
		case tax.ReversalModeFull:
			sum += total
		case tax.ReversalModePartial:
			sum += c.GrossAmountCents
		}
	}
	return sum
}

// A relief note on a written-off invoice reverses its own slice of tax, and
// whatever follows reverses the rest at most once (ADR-050 amendment
// 2026-10-23, ADR-111):
//   - a later void reverses what no note reversed, so the invoice's tax is
//     reversed exactly once in total;
//   - a later payment reverses nothing more: the used part was sold and paid.
//
// The invoice is 100.00 with 10.00 of Stripe Tax and 30.00 of customer credits
// applied (70.00 due). It is written off (no tax leg), then a cancel's 50.00
// relief note issues on the unpaid path: 50.00 off what is asked, 70.00 due
// becomes 20.00, no credits displaced.
//
// The engine's relief writes the same draft (CreateAdjustmentDraftTx) on the
// change's transaction; the HTTP e2e tests in internal/api run that path
// without tax.
//
// Mutations: make the void reverse the full transaction (ModeFull) → the void
// case reverses 150.00 of gross on a 100.00 invoice; skip the note's own
// reversal on a written-off invoice → both cases are red.
func TestWrittenOff_ReliefNoteThenVoidOrPayment_TaxReversedOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		then       string
		wantStatus domain.InvoiceStatus
		wantCalls  int
		wantGross  int64 // gross reversed in total, every call summed
		wantCredit int64 // customer balance at the end
	}{
		{"then void: the rest is reversed once", "void", domain.InvoiceVoided, 2, 10000, 3000},
		{"then a late payment: nothing more is reversed", "pay", domain.InvoicePaid, 1, 5000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOverCreditRig(t, "CN Written Off Tax "+tc.then)
			setInvoiceTax(t, r, 9000, 1000)
			r.applyCredits(t, 3000)

			rev := &taxCalls{}
			r.notes.SetTaxReverser(rev)
			r.invSvc.SetTaxReverser(rev)
			r.invSvc.SetCreditNoteTotaler(r.notes)

			if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, r.inv.ID); err != nil {
				t.Fatalf("write off: %v", err)
			}
			if len(rev.calls) != 0 {
				t.Fatalf("the write-off reversed tax (%d calls), want none (ADR-111)", len(rev.calls))
			}

			tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			draft, err := r.notes.CreateAdjustmentDraftTx(r.ctx, tx, r.tenantID, r.inv.ID, 5000, "subscription_cancellation", "unused time")
			if err != nil {
				t.Fatalf("relief draft on a written-off invoice: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit draft: %v", err)
			}
			cn, err := r.notes.Issue(r.ctx, r.tenantID, draft.ID)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if cn.Status != domain.CreditNoteIssued || cn.TotalCents != 5000 || cn.TaxAmountCents != 500 {
				t.Fatalf("note %s, total %d, tax %d; want issued, 5000, 500 (its share of the 10%% tax)", cn.Status, cn.TotalCents, cn.TaxAmountCents)
			}
			if len(rev.calls) != 1 || rev.calls[0].Mode != tax.ReversalModePartial || rev.calls[0].GrossAmountCents != 5000 {
				t.Fatalf("tax reversals after the note = %+v, want one partial of 5000 (the note's slice)", rev.calls)
			}
			if inv := r.invoiceNow(t); inv.Status != domain.InvoiceUncollectible || inv.AmountDueCents != 2000 {
				t.Fatalf("after the note: invoice %s with %d due, want uncollectible with 2000 due", inv.Status, inv.AmountDueCents)
			}

			switch tc.then {
			case "void":
				if _, err := r.invSvc.Void(r.ctx, r.tenantID, r.inv.ID); err != nil {
					t.Fatalf("Void: %v", err)
				}
			case "pay":
				if _, err := r.invSvc.RecordOfflinePayment(r.ctx, r.tenantID, r.inv.ID, "late wire"); err != nil {
					t.Fatalf("RecordOfflinePayment: %v", err)
				}
			}
			inv := r.invoiceNow(t)
			if inv.Status != tc.wantStatus {
				t.Fatalf("invoice %s, want %s", inv.Status, tc.wantStatus)
			}
			if tc.then == "pay" && inv.AmountPaidCents != 2000 {
				t.Errorf("paid %d, want 2000: what is left after the credits and the note", inv.AmountPaidCents)
			}
			if len(rev.calls) != tc.wantCalls {
				t.Fatalf("tax reversal calls = %+v, want %d", rev.calls, tc.wantCalls)
			}
			if got := rev.grossReversed(t, inv.TotalAmountCents); got != tc.wantGross {
				t.Errorf("gross reversed in total = %d, want %d: more under-remits the tenant's tax, less over-remits it", got, tc.wantGross)
			}
			if got := r.balance(t); got != tc.wantCredit {
				t.Errorf("customer balance = %d, want %d: credits applied come back once, on a void only", got, tc.wantCredit)
			}
		})
	}
}

// setInvoiceTax gives the rig's 100.00 invoice a committed Stripe Tax
// transaction: subtotal + tax = 100.00.
func setInvoiceTax(t *testing.T, r *overCreditRig, subtotal, taxCents int64) {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `
		UPDATE invoices SET subtotal_cents = $2, tax_amount_cents = $3, tax_provider = 'stripe_tax',
			tax_transaction_id = 'tx_committed', tax_status = 'ok'
		WHERE id = $1`, r.inv.ID, subtotal, taxCents); err != nil {
		t.Fatalf("set tax: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
