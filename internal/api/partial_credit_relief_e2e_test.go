package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// A funding invoice that earlier credit notes have partly credited can take
// only what is left of its total. When a cancel, an immediate plan swap or an
// item change owes more than that, Hardcount places what fits and records the rest
// on the change as uncredited_cents (ADR-118 amendment 2026-10-22).
// Before, a cancel or swap gave nothing and an item change answered 500.
//
// Two real ways to get there, both driven over the real server and Postgres:
//   - a card-paid invoice the operator partly credited;
//   - a partial waive: customer credits paid part of the invoice and a credit
//     note waived the rest, so the invoice is paid and its room is the
//     credits applied (ADR-061, amendment 2026-10-19).
//
// Fixture (lagRig): plan A 3000c plus add-on C 1000c, monthly, in advance,
// period [Apr 1, May 1). The first invoice is 4000c, with no tax.
//
// Mutation seams (domain.AllocateUnusedCredit, shared by every path):
//   - return no shares when anything is unplaced → the cancel, swap and
//     removal tests get no relief note;
//   - drop the per-invoice room (grossCap = total) AND the net→gross clamp →
//     the cancel arm credits more than the invoice total.

var reliefCancelAt = lagP0.AddDate(0, 0, 15) // Apr 16: 15 of 30 days unused

// TestE2E_CancelAfterPartialCredit_GivesBackWhatFits: card-paid invoice,
// then an operator note of 3000c, so 1000c of room is left. An immediate
// cancel on Apr 16 owes 2000c of unused time: it credits the 1000c that fits.
// The control, with no earlier note, credits the full 2000c.
func TestE2E_CancelAfterPartialCredit_GivesBackWhatFits(t *testing.T) {
	skipShort(t)

	t.Run("control: no earlier note, the full unused part", func(t *testing.T) {
		r := newLagRig(t, "in_advance", true)
		invID := r.firstInvoiceID()
		r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)

		r.cancelNow(reliefCancelAt)
		if n, sum := r.creditNotesOn(invID), r.creditNoteTotalOn(invID); n != 1 || sum != 2000 {
			t.Errorf("%d notes summing to %dc on the invoice, want one relief note of 2000c", n, sum)
		}
		if bal := r.creditBalance(); bal != 2000 {
			t.Errorf("balance = %dc, want 2000c", bal)
		}
	})

	t.Run("partly credited: the 1000c that fits", func(t *testing.T) {
		r := newLagRig(t, "in_advance", true)
		invID := r.firstInvoiceID()
		r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)
		r.creditNote(invID, 3000, lagP0.Add(2*time.Hour))
		before := r.creditBalance()

		r.cancelNow(reliefCancelAt)
		if n := r.creditNotesOn(invID); n != 2 {
			t.Errorf("%d notes on the invoice, want 2 (the operator's and the relief)", n)
		}
		if sum := r.creditNoteTotalOn(invID); sum != 4000 {
			t.Errorf("notes on the invoice sum to %dc, want 4000c: the relief is the 1000c left, and never more than the total", sum)
		}
		if got := r.creditBalance() - before; got != 1000 {
			t.Errorf("the cancel added %dc to the balance, want 1000c", got)
		}
	})
}

// TestE2E_CancelAfterPartialWaive_ReturnsTheCredits is the ADR-061 example:
// 1000c of customer credit paid part of the first invoice, the customer has no
// card so 3000c stayed due, and the operator waived it. A cancel halfway
// returns the 1000c of credits. Before, it returned nothing.
//
// The negative control is a full waive: nothing was paid for the invoice, so
// the cancel gives no relief.
func TestE2E_CancelAfterPartialWaive_ReturnsTheCredits(t *testing.T) {
	skipShort(t)

	t.Run("partial waive: the credits come back", func(t *testing.T) {
		r := newLagRigGranted(t, "in_advance", true, 1000)
		invID := r.firstInvoiceID()
		inv := flowObj(r.must(http.MethodGet, "/v1/invoices/"+invID, lagP0.Add(time.Hour), nil, http.StatusOK), "invoice")
		if applied, due := flowInt(inv, "credits_applied_cents"), flowInt(inv, "amount_due_cents"); applied != 1000 || due != 3000 {
			t.Fatalf("first invoice: %dc credits applied, %dc due; want 1000c and 3000c", applied, due)
		}
		r.waive(invID, lagP0.Add(2*time.Hour))
		inv = flowObj(r.must(http.MethodGet, "/v1/invoices/"+invID, lagP0.Add(2*time.Hour), nil, http.StatusOK), "invoice")
		if s := flowStr(inv, "status"); s != string(domain.InvoicePaid) {
			t.Fatalf("waived invoice is %s, want paid", s)
		}
		if bal := r.creditBalance(); bal != 0 {
			t.Fatalf("balance before the cancel = %dc, want 0c (the grant paid the invoice)", bal)
		}

		r.cancelNow(reliefCancelAt)
		if n, sum := r.creditNotesOn(invID), r.creditNoteTotalOn(invID); n != 2 || sum != 4000 {
			t.Errorf("%d notes summing to %dc, want 2 summing to 4000c (the 3000c waiver and a 1000c relief)", n, sum)
		}
		if bal := r.creditBalance(); bal != 1000 {
			t.Errorf("balance after the cancel = %dc, want the 1000c of credits back", bal)
		}
	})

	t.Run("full waive: no relief", func(t *testing.T) {
		r := newLagRig(t, "in_advance", true)
		invID := r.firstInvoiceID()
		r.waive(invID, lagP0.Add(time.Hour))

		r.cancelNow(reliefCancelAt)
		if n := r.creditNotesOn(invID); n != 1 {
			t.Errorf("%d notes on the waived invoice, want 1 (the waiver): it was already credited in full", n)
		}
		if bal := r.creditBalance(); bal != 0 {
			t.Errorf("balance = %dc, want 0c", bal)
		}
	})
}

// TestE2E_RemoveItemAfterPartialCredit_GivesBackWhatFits: card-paid invoice,
// then an operator note of 3600c, so 400c of room is left. Removing the
// add-on on Apr 11 owes 667c (1000c x 20/30): the removal goes through and
// credits the 400c that fits. Before, it answered 500.
func TestE2E_RemoveItemAfterPartialCredit_GivesBackWhatFits(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", true)
	invID := r.firstInvoiceID()
	r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)
	r.creditNote(invID, 3600, lagP0.Add(2*time.Hour))

	r.removeAddOn(lagP0.AddDate(0, 0, 10))
	if n, sum := r.creditNotesOn(invID), r.creditNoteTotalOn(invID); n != 2 || sum != 4000 {
		t.Errorf("%d notes summing to %dc, want 2 summing to 4000c (the 3600c note and a 400c credit)", n, sum)
	}
	if got := r.auditInt(string(domain.AuditActionUpdate), "uncredited_cents"); got != 267 {
		t.Errorf("removal audit uncredited_cents = %d, want 267 (667c owed, 400c fit)", got)
	}
}

// TestE2E_CrossIntervalSwapAfterPartialCredit_GivesBackWhatFits: plan A
// alone (3000c), card-paid, then an operator note of 2500c, so 500c of room
// is left. An immediate swap to the yearly plan on Apr 16 owes 1500c of
// unused time on April's invoice: it credits the 500c that fits. Before, it
// credited nothing.
func TestE2E_CrossIntervalSwapAfterPartialCredit_GivesBackWhatFits(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", false)
	invID := r.firstInvoiceID()
	r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)
	r.creditNote(invID, 2500, lagP0.Add(2*time.Hour))

	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, reliefCancelAt,
		map[string]any{"new_plan_id": r.planY, "immediate": true}, http.StatusOK)
	if n, sum := r.creditNotesOn(invID), r.creditNoteTotalOn(invID); n != 2 || sum != 3000 {
		t.Errorf("%d notes summing to %dc on April's invoice, want 2 summing to 3000c (the 2500c note and a 500c relief)", n, sum)
	}
	if got := r.auditInt(domain.AuditActionSubscriptionItemUpdated, "uncredited_cents"); got != 1000 {
		t.Errorf("swap audit uncredited_cents = %d, want 1000 (1500c owed, 500c fit)", got)
	}
}

// cancelNow cancels the subscription immediately at the instant at.
func (r *lagRig) cancelNow(at time.Time) {
	r.t.Helper()
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/cancel", at, nil, http.StatusOK)
	if s := r.sub(); s.Status != domain.SubscriptionCanceled {
		r.t.Fatalf("status = %s after the cancel, want canceled", s.Status)
	}
}

// creditNote issues an operator credit note of cents on the invoice. On a
// paid invoice it goes to the customer's balance.
func (r *lagRig) creditNote(invID string, cents int64, at time.Time) {
	r.t.Helper()
	r.must(http.MethodPost, "/v1/credit-notes", at, map[string]any{
		"invoice_id": invID, "reason": "Goodwill", "auto_issue": true,
		"lines": []map[string]any{{"description": "Goodwill", "quantity": 1, "unit_amount_cents": cents}},
	}, http.StatusCreated)
}

// creditNoteTotalOn sums the totals of the invoice's credit notes that are
// not voided.
func (r *lagRig) creditNoteTotalOn(invID string) int {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT COALESCE(SUM(total_cents), 0)::int FROM credit_notes WHERE invoice_id = $1 AND status <> 'voided'`, invID)
}
