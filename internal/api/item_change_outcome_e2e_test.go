package api

import (
	"net/http"
	"testing"
	"time"
)

// What an item change answers about its money (#1280). The change itself is
// tested elsewhere (one_credit_rule_e2e_test.go, partial_credit_relief_e2e_test.go);
// these tests read only what the response reports, over the real server and
// Postgres.

// A decrease whose credit lands partly on a paid invoice and partly on an
// open one reports each part. Fixture as T2 of the one crediting rule: 8 → 3
// seats on Sep 21 owes $16.67; $15.00 comes off the open upgrade invoice and
// $1.67 goes to the balance. Before, the response said "credit" with $16.67
// and nothing about the open invoice.
func TestE2E_ItemChangeOutcome_ReportsEachPart(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	up := r.upgradeTo8(false)

	res := r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, ocDown, map[string]any{"quantity": 3}, http.StatusOK)
	p := flowObj(res, "proration")
	if got := flowStr(p, "type"); got != "credit" {
		t.Errorf("proration type = %q, want credit", got)
	}
	if got := flowInt(p, "amount_cents"); got != 1667 {
		t.Errorf("amount_cents = %d, want 1667 (the whole credit)", got)
	}
	if got := flowInt(p, "credited_to_balance_cents"); got != 167 {
		t.Errorf("credited_to_balance_cents = %d, want 167", got)
	}
	if got := flowInt(p, "reduced_open_invoices_cents"); got != 1500 {
		t.Errorf("reduced_open_invoices_cents = %d, want 1500", got)
	}
	// The reported parts are what happened.
	if bal := r.creditBalance(); bal != 167 {
		t.Errorf("balance = %dc, want 167c", bal)
	}
	if due := flowInt(r.invoice(up), "amount_due_cents"); due != 0 {
		t.Errorf("upgrade invoice still has %dc due, want 0c", due)
	}
}

// With every charging invoice paid, the whole credit is reported as going to
// the balance and nothing as taken off open invoices.
func TestE2E_ItemChangeOutcome_AllPaidReportsBalanceOnly(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	r.upgradeTo8(true)

	res := r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, ocDown, map[string]any{"quantity": 3}, http.StatusOK)
	p := flowObj(res, "proration")
	if got := flowInt(p, "credited_to_balance_cents"); got != 1667 {
		t.Errorf("credited_to_balance_cents = %d, want 1667", got)
	}
	if _, ok := p["reduced_open_invoices_cents"]; !ok {
		t.Errorf("reduced_open_invoices_cents missing from %v, want it present as 0", p)
	}
	if got := flowInt(p, "reduced_open_invoices_cents"); got != 0 {
		t.Errorf("reduced_open_invoices_cents = %d, want 0", got)
	}
}

// An immediate swap to a plan with another billing interval starts a new
// period now and invoices it at once. The response names that invoice and its
// total, so the dashboard can say what was billed. Before, it returned no
// outcome at all.
func TestE2E_CrossIntervalSwap_ReportsTheNewPeriodInvoice(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", false)
	first := r.firstInvoiceID()
	r.must(http.MethodPost, "/v1/invoices/"+first+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)

	res := r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, reliefCancelAt,
		map[string]any{"new_plan_id": r.planY, "immediate": true}, http.StatusOK)
	np := flowObj(res, "new_period_invoice")
	id := flowStr(np, "id")
	if id == "" {
		t.Fatalf("no new_period_invoice in the swap response: %v", res)
	}
	inv := r.invoice(id)
	if got, want := flowInt(np, "total_amount_cents"), flowInt(inv, "total_amount_cents"); got != want || got != 30000 {
		t.Errorf("reported total = %dc, invoice total = %dc, want both 30000c (a year of the new plan)", got, want)
	}
	if got, want := flowStr(np, "invoice_number"), flowStr(inv, "invoice_number"); got == "" || got != want {
		t.Errorf("reported number = %q, invoice number = %q, want them equal", got, want)
	}
	if got := flowStr(np, "currency"); got != "USD" {
		t.Errorf("reported currency = %q, want USD", got)
	}
	start, err := time.Parse(time.RFC3339, flowStr(inv, "billing_period_start"))
	if err != nil || !start.Equal(reliefCancelAt) {
		t.Errorf("reported invoice starts its period at %q, want the swap instant %v", flowStr(inv, "billing_period_start"), reliefCancelAt)
	}
}

// invoice is the invoice as GET /v1/invoices/{id} returns it.
func (r *lagRig) invoice(invID string) map[string]any {
	r.t.Helper()
	return flowObj(r.must(http.MethodGet, "/v1/invoices/"+invID, r.clk.Now(r.t.Context()), nil, http.StatusOK), "invoice")
}
