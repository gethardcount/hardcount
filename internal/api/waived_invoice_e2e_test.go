package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// An operator waives an invoice with a credit note for what is due, and the
// invoice is marked paid (G3, #1306). Two readers route on a paid invoice and
// assume the customer paid money for it. These tests drive the real server
// over real Postgres through what an operator does next (review of #1306).

// TestE2E_RemoveItemAfterWaive_NoFurtherCredit: the operator waives the
// subscription's first in-advance invoice, then removes an add-on later in
// the same period. The waiver already credited the whole invoice, so nothing
// more is owed: the removal goes through with no second credit note. This is
// the rule ADR-118 set for a cancel or plan swap whose funding invoices are
// already fully credited. Before, the removal failed with a 500 ("no
// creditable funding invoice for period") and the item could not be removed
// until the period ended.
//
// The control is the same removal on an invoice paid with money: it is
// credited the add-on's unused days, which shows the removal reaches the
// credit path in both arms.
//
// Mutation seam: in subscription.creditUnusedTime, return an error when
// domain.AllocateUnusedCredit places no share → the waived arm gets a 500.
func TestE2E_RemoveItemAfterWaive_NoFurtherCredit(t *testing.T) {
	skipShort(t)
	removeAt := lagP0.AddDate(0, 0, 10)

	t.Run("control: paid with money, the removal is credited", func(t *testing.T) {
		r := newLagRig(t, "in_advance", true)
		invID := r.firstInvoiceID()
		r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", lagP0.Add(time.Hour), map[string]any{"note": "fixture"}, http.StatusOK)

		r.removeAddOn(removeAt)
		if n := r.creditNotesOn(invID); n != 1 {
			t.Errorf("%d credit notes on the paid invoice after removing the add-on, want 1 (the unused days)", n)
		}
	})

	t.Run("waived: the removal goes through with no further credit", func(t *testing.T) {
		r := newLagRig(t, "in_advance", true)
		invID := r.firstInvoiceID()
		r.waive(invID, lagP0.Add(time.Hour))
		inv := flowObj(r.must(http.MethodGet, "/v1/invoices/"+invID, lagP0.Add(time.Hour), nil, http.StatusOK), "invoice")
		if s := flowStr(inv, "status"); s != string(domain.InvoicePaid) {
			t.Fatalf("waived invoice is %s, want paid", s)
		}

		r.removeAddOn(removeAt)
		if n := r.creditNotesOn(invID); n != 1 {
			t.Errorf("%d credit notes on the waived invoice, want 1 (the waiver): it was already credited in full", n)
		}
	})
}

// TestE2E_Revenue_CountsWhatPaidTheInvoice: Revenue and the revenue chart
// count what paid each invoice (the payment plus the customer credit
// applied), not its total. A credit note lowers an unpaid invoice's amount
// due and leaves its total as it was, so summing totals counted the waived
// part as revenue: an invoice waived in full added its whole total, in the
// month it was waived (G3 made it paid). The Dunning page says a waived
// invoice "no longer counts as revenue".
//
// Three invoices, all paid in the window:
//   - 30.00 paid with money: 30.00 (the control, unchanged);
//   - 49.00 waived in full by a credit note: 0;
//   - 20.00 lowered by a 5.00 credit note, then paid: 15.00.
//
// Mutation seam: sum total_amount_cents again in sumPaidRevenue or the
// revenue chart → that figure reads 99.00.
func TestE2E_Revenue_CountsWhatPaidTheInvoice(t *testing.T) {
	skipShort(t)
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Waived Revenue Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "waived-revenue", "display_name": "Waived Revenue", "email": "ap@waived-revenue.test",
	}}).want(t, 201, "create customer").body, "id")
	invoice := func(what string, cents int) string {
		id := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID, "currency": "USD",
			"line_items": []map[string]any{{"description": what, "quantity": 1, "unit_amount_cents": cents}},
		}}).want(t, 201, "create "+what).body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, "finalize "+what)
		return id
	}
	creditNote := func(invID string, cents int) {
		call("POST", "/v1/credit-notes", flowReq{body: map[string]any{
			"invoice_id": invID, "reason": "Waived", "auto_issue": true,
			"lines": []map[string]any{{"description": "Waived", "quantity": 1, "unit_amount_cents": cents}},
		}}).want(t, 201, fmt.Sprintf("credit note of %d on %s", cents, invID))
	}

	paid := invoice("paid with money", 3000)
	call("POST", "/v1/invoices/"+paid+"/record-payment", flowReq{body: map[string]any{"note": "Wire 1"}}).want(t, 200, "record payment")
	waived := invoice("waived in full", 4900)
	creditNote(waived, 4900)
	lowered := invoice("lowered then paid", 2000)
	creditNote(lowered, 500)
	call("POST", "/v1/invoices/"+lowered+"/record-payment", flowReq{body: map[string]any{"note": "Wire 2"}}).want(t, 200, "record payment")
	for _, id := range []string{paid, waived, lowered} {
		if s := flowStr(call("GET", "/v1/invoices/"+id, flowReq{}).want(t, 200, "get invoice").body, "invoice", "status"); s != string(domain.InvoicePaid) {
			t.Fatalf("invoice %s is %s, want paid", id, s)
		}
	}

	const want = 3000 + 0 + 1500
	ov := call("GET", "/v1/analytics/overview?period=30d", flowReq{}).want(t, 200, "overview").body
	if got := flowInt(ov, "revenue"); got != want {
		t.Errorf("Revenue = %d, want %d: what paid the three invoices, not their totals", got, want)
	}
	if got := flowInt(ov, "paid_invoices"); got != 3 {
		t.Errorf("paid invoices = %d, want 3", got)
	}
	var chart int64
	for _, b := range flowList(call("GET", "/v1/analytics/revenue-chart?period=30d", flowReq{}).want(t, 200, "revenue chart").body, "data") {
		chart += flowInt(b, "revenue_cents")
	}
	if chart != want {
		t.Errorf("revenue chart sums to %d, want %d", chart, want)
	}
}

// firstInvoiceID is the subscription's subscription_create invoice.
func (r *lagRig) firstInvoiceID() string {
	r.t.Helper()
	for _, i := range r.invoices() {
		if i.reason == string(domain.BillingReasonSubscriptionCreate) {
			return i.id
		}
	}
	r.logInvoices(r.invoices())
	r.t.Fatalf("no subscription_create invoice")
	return ""
}

// waive issues a credit note for everything due on the invoice, as an
// operator waiving it does from the dashboard.
func (r *lagRig) waive(invID string, at time.Time) {
	r.t.Helper()
	due := flowInt(r.must(http.MethodGet, "/v1/invoices/"+invID, at, nil, http.StatusOK), "invoice", "amount_due_cents")
	if due <= 0 {
		r.t.Fatalf("invoice %s has %d due, want an amount to waive", invID, due)
	}
	r.must(http.MethodPost, "/v1/credit-notes", at, map[string]any{
		"invoice_id": invID, "reason": "Waived", "auto_issue": true,
		"lines": []map[string]any{{"description": "Waived", "quantity": 1, "unit_amount_cents": due}},
	}, http.StatusCreated)
}

// removeAddOn removes the plan C item at the instant at and wants 204.
func (r *lagRig) removeAddOn(at time.Time) {
	r.t.Helper()
	var addOn string
	for _, it := range r.sub().Items {
		if it.PlanID == r.planC {
			addOn = it.ID
		}
	}
	if addOn == "" {
		r.t.Fatalf("no add-on item on the subscription")
	}
	if code, raw := r.do(http.MethodDelete, "/v1/subscriptions/"+r.subID+"/items/"+addOn, at, nil); code != http.StatusNoContent {
		r.t.Fatalf("remove the add-on: %d %s, want 204", code, raw)
	}
	for _, it := range r.sub().Items {
		if it.ID == addOn {
			r.t.Errorf("add-on %s still on the subscription after its removal", addOn)
		}
	}
}

// creditNotesOn counts the invoice's credit notes that are not voided.
func (r *lagRig) creditNotesOn(invID string) int {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM credit_notes WHERE invoice_id = $1 AND status <> 'voided'`, invID)
}
