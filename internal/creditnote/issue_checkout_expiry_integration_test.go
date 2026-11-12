package creditnote_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
)

// expiryRecorder records the invoices whose Checkout sessions Issue expired.
type expiryRecorder struct{ invoices []string }

func (e *expiryRecorder) ExpireCheckoutSessions(_ context.Context, _ string, inv domain.Invoice) {
	e.invoices = append(e.invoices, inv.ID)
}

// A credit note that lowers what an unpaid invoice asks for expires the
// customer's open Checkout page, which still asks for the old amount; the
// next page asks for the new one. A note on a paid invoice lowers nothing
// that is owed, so it expires nothing.
//
// Mutations: drop the expiry from Issue → the first two cases are red; drop
// the dueReduced > 0 gate → the paid case is red.
func TestIssue_ExpiresCheckoutWhenItLowersWhatIsOwed(t *testing.T) {
	issue := func(t *testing.T, r *overCreditRig, inv domain.Invoice, cents int64) {
		t.Helper()
		cn, err := r.notes.Create(r.ctx, r.tenantID, creditnote.CreateInput{
			InvoiceID: inv.ID,
			Reason:    "order_change",
			Lines:     []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: cents}},
		})
		if err != nil {
			t.Fatalf("create credit note: %v", err)
		}
		if _, err := r.notes.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
			t.Fatalf("issue credit note: %v", err)
		}
	}

	t.Run("part of what is owed", func(t *testing.T) {
		r := newOverCreditRig(t, "CN Checkout Expiry Part")
		rec := &expiryRecorder{}
		r.notes.SetCheckoutExpirer(rec)
		issue(t, r, r.inv, 3000)
		if len(rec.invoices) != 1 || rec.invoices[0] != r.inv.ID {
			t.Errorf("Checkout expiries = %v, want one for %s", rec.invoices, r.inv.ID)
		}
	})

	t.Run("all of what is owed", func(t *testing.T) {
		r := newOverCreditRig(t, "CN Checkout Expiry All")
		rec := &expiryRecorder{}
		r.notes.SetCheckoutExpirer(rec)
		issue(t, r, r.inv, r.inv.AmountDueCents)
		if got := r.invoiceNow(t); got.Status != domain.InvoicePaid {
			t.Fatalf("invoice status = %s, want paid", got.Status)
		}
		if len(rec.invoices) != 1 || rec.invoices[0] != r.inv.ID {
			t.Errorf("Checkout expiries = %v, want one for %s", rec.invoices, r.inv.ID)
		}
	})

	t.Run("a paid invoice", func(t *testing.T) {
		r := newOverCreditRig(t, "CN Checkout Expiry Paid")
		rec := &expiryRecorder{}
		r.notes.SetCheckoutExpirer(rec)
		_, paid := seedInvoiceForCN(t, r.db, r.tenantID, "checkout-expiry-paid", domain.InvoicePaid, domain.PaymentSucceeded, "pi_checkout_expiry_paid")
		issue(t, r, paid, 3000)
		if len(rec.invoices) != 0 {
			t.Errorf("Checkout expiries = %v, want none: a note on a paid invoice lowers nothing owed", rec.invoices)
		}
	})
}
