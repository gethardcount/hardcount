package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// A written-off (uncollectible) in_advance base invoice is billed but unpaid.
// A mid-period change treats it exactly as it treats an unpaid one (ADR-050,
// amendment 2026-10-23):
//   - a change that would charge more for the period is refused with
//     unpaid_invoice_blocks_change;
//   - a cancel, a downgrade or a removed item lowers what it asks for by the
//     unused time (an adjustment credit note), and voids it when none of the
//     period was used.
//
// Before, the base and funding lookups skipped a written-off invoice. An
// upgrade then took the "no base invoice yet" branch and charged nothing for
// the rest of the period, and a cancel gave no relief, so a customer who paid
// the written-off invoice later paid for time they never had.
//
// Fixture (lagRig): plan A 3000c (plus add-on C 1000c where named), monthly,
// in advance, period [Apr 1, May 1). No card, so the first invoice stays
// unpaid. Every case runs three ways: unpaid (the control the written-off
// case must match), written off, and paid (the paid control).
//
// Mutation seams: put 'uncollectible' back in the base lookup's predicate
// (the upgrade test) or the funding lookup's (the cancel and removal tests);
// refuse an issue_pending note on an uncollectible invoice at create, or void
// it as an orphan at issue (the cancel and removal tests).

// writeOff marks the invoice uncollectible.
func (r *lagRig) writeOff(invID string, at time.Time) {
	r.t.Helper()
	r.must(http.MethodPost, "/v1/invoices/"+invID+"/mark-uncollectible", at, nil, http.StatusOK)
}

// invoiceMoney reads the invoice's status, amount due and amount paid.
func (r *lagRig) invoiceMoney(invID string, at time.Time) (status string, due, paid int64) {
	r.t.Helper()
	inv := flowObj(r.must(http.MethodGet, "/v1/invoices/"+invID, at, nil, http.StatusOK), "invoice")
	return flowStr(inv, "status"), flowInt(inv, "amount_due_cents"), flowInt(inv, "amount_paid_cents")
}

// TestE2E_WrittenOffBase_UpgradeIsRefusedAsUnpaid: an upgrade to plan B on
// Apr 16 would charge 1500c for the rest of April. Over an unpaid base it is
// refused; over a written-off base it must be refused the same way. Over a
// paid base it goes ahead and writes the 1500c proration invoice.
func TestE2E_WrittenOffBase_UpgradeIsRefusedAsUnpaid(t *testing.T) {
	skipShort(t)
	upgrade := func(r *lagRig) (int, []byte) {
		return r.do(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, reliefCancelAt,
			map[string]any{"new_plan_id": r.planB, "immediate": true})
	}

	for _, tc := range []struct {
		name     string
		writeOff bool
	}{
		{"unpaid base: refused", false},
		{"written-off base: refused the same way", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLagRig(t, "in_advance", false)
			invID := r.firstInvoiceID()
			if tc.writeOff {
				r.writeOff(invID, lagP0.Add(time.Hour))
			}
			code, raw := upgrade(r)
			wantAPIError(t, code, raw, http.StatusConflict, "unpaid_invoice_blocks_change", "is unpaid")
			if s := r.sub(); len(s.Items) != 1 || s.Items[0].PlanID != r.planA {
				t.Errorf("subscription items after the refused upgrade = %+v, want plan A unchanged", s.Items)
			}
			for _, i := range r.invoices() {
				if i.reason == string(domain.BillingReasonSubscriptionUpdate) {
					t.Errorf("a refused upgrade wrote proration invoice %s (%dc)", i.id, i.subtotal)
				}
			}
		})
	}

	t.Run("paid base: the upgrade charges the rest of April", func(t *testing.T) {
		r := newLagRig(t, "in_advance", false)
		r.must(http.MethodPost, "/v1/invoices/"+r.firstInvoiceID()+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)
		if code, raw := upgrade(r); code != http.StatusOK {
			t.Fatalf("upgrade over a paid base: %d %s, want 200", code, raw)
		}
		if pro := r.prorationInvoice(); pro.subtotal != 1500 {
			t.Errorf("proration invoice subtotal = %dc, want 1500c ((6000c - 3000c) x 15/30)", pro.subtotal)
		}
	})
}

// TestE2E_WrittenOffBase_CancelThenLatePayment: an immediate cancel on Apr 16
// leaves 15 of April's 30 days unused, 1500c of the 3000c base. Whichever way
// the invoice stood, the customer ends up paying 1500c for the half they used:
//   - unpaid: a 1500c adjustment note lowers what is due to 1500c, and the
//     later payment records 1500c;
//   - written off: the same note on the written-off invoice, the same 1500c
//     due, the same 1500c payment. It stays written off until paid;
//   - paid: 3000c paid, and the note puts 1500c on the customer's balance.
//
// A cancel on Apr 1, with none of the period used, voids an unpaid base; a
// written-off base is voided the same way, so nothing is left to pay.
func TestE2E_WrittenOffBase_CancelThenLatePayment(t *testing.T) {
	skipShort(t)
	payAt := reliefCancelAt.Add(24 * time.Hour)

	for _, tc := range []struct {
		name          string
		writeOff      bool
		wantStatusMid string
	}{
		{"unpaid base", false, string(domain.InvoiceFinalized)},
		{"written-off base", true, string(domain.InvoiceUncollectible)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLagRig(t, "in_advance", false)
			invID := r.firstInvoiceID()
			if tc.writeOff {
				r.writeOff(invID, lagP0.Add(time.Hour))
			}

			r.cancelNow(reliefCancelAt)
			if n, sum := r.creditNotesOn(invID), r.creditNoteTotalOn(invID); n != 1 || sum != 1500 {
				t.Errorf("%d notes summing to %dc on the invoice, want one relief note of 1500c (15 of 30 days unused)", n, sum)
			}
			if issued := flowCount(t, r.db, `SELECT count(*)::int FROM credit_notes WHERE invoice_id = $1 AND status = 'issued'`, invID); issued != 1 {
				t.Errorf("%d issued notes on the invoice, want 1: the relief must be applied, not held or voided", issued)
			}
			if status, due, _ := r.invoiceMoney(invID, reliefCancelAt.Add(time.Hour)); status != tc.wantStatusMid || due != 1500 {
				t.Errorf("after the cancel: invoice %s with %dc due, want %s with 1500c due", status, due, tc.wantStatusMid)
			}

			r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", payAt, map[string]any{"note": "late wire"}, http.StatusOK)
			if status, due, paid := r.invoiceMoney(invID, payAt.Add(time.Hour)); status != string(domain.InvoicePaid) || due != 0 || paid != 1500 {
				t.Errorf("after the late payment: invoice %s, %dc due, %dc paid; want paid, 0c due, 1500c paid (the used half)", status, due, paid)
			}
			if bal := r.creditBalance(); bal != 0 {
				t.Errorf("credit balance = %dc, want 0c: no money was paid beyond what was used", bal)
			}
		})
	}

	t.Run("paid base: control", func(t *testing.T) {
		r := newLagRig(t, "in_advance", false)
		invID := r.firstInvoiceID()
		r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)
		r.cancelNow(reliefCancelAt)
		_, _, paid := r.invoiceMoney(invID, reliefCancelAt.Add(time.Hour))
		if bal := r.creditBalance(); paid-bal != 1500 {
			t.Errorf("paid %dc, balance credit %dc: net %dc, want 1500c (the used half)", paid, bal, paid-bal)
		}
	})

	for _, tc := range []struct {
		name     string
		writeOff bool
	}{
		{"unpaid base, nothing used: voided", false},
		{"written-off base, nothing used: voided the same way", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLagRig(t, "in_advance", false)
			invID := r.firstInvoiceID()
			if tc.writeOff {
				r.writeOff(invID, lagP0.Add(time.Hour))
			}
			r.cancelNow(lagP0.Add(2 * time.Hour))
			if status, _, _ := r.invoiceMoney(invID, lagP0.Add(3*time.Hour)); status != string(domain.InvoiceVoided) {
				t.Errorf("invoice %s after a cancel on its first day, want voided: none of the period was used", status)
			}
		})
	}
}

// TestE2E_WrittenOffBase_RemovedItemLowersWhatIsDue: plan A 3000c plus add-on
// C 1000c, first invoice 4000c. Removing the add-on on Apr 16 owes 500c of
// unused time (1000c x 15/30). On an unpaid invoice that is a 500c adjustment
// that leaves 3500c due; a written-off invoice must get the same.
func TestE2E_WrittenOffBase_RemovedItemLowersWhatIsDue(t *testing.T) {
	skipShort(t)
	for _, tc := range []struct {
		name       string
		writeOff   bool
		wantStatus string
	}{
		{"unpaid base", false, string(domain.InvoiceFinalized)},
		{"written-off base", true, string(domain.InvoiceUncollectible)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLagRig(t, "in_advance", true)
			invID := r.firstInvoiceID()
			if tc.writeOff {
				r.writeOff(invID, lagP0.Add(time.Hour))
			}
			r.removeAddOn(reliefCancelAt)
			if n, sum := r.creditNotesOn(invID), r.creditNoteTotalOn(invID); n != 1 || sum != 500 {
				t.Errorf("%d notes summing to %dc on the invoice, want one 500c adjustment", n, sum)
			}
			if status, due, _ := r.invoiceMoney(invID, reliefCancelAt.Add(time.Hour)); status != tc.wantStatus || due != 3500 {
				t.Errorf("after the removal: invoice %s with %dc due, want %s with 3500c due", status, due, tc.wantStatus)
			}
			if bal := r.creditBalance(); bal != 0 {
				t.Errorf("credit balance = %dc, want 0c: nothing was paid, so nothing goes to the balance", bal)
			}
		})
	}
}
