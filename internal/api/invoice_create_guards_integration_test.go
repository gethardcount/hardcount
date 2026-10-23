package api

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Operator invoice writes through the real router and database: what a
// line may be, which subscription an invoice may name, and that a refused
// create does not use up an invoice number.

type invoiceGuardEnv struct {
	ts  *httptest.Server
	key string
}

func newInvoiceGuardEnv(t *testing.T, name string) invoiceGuardEnv {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, name)
	return invoiceGuardEnv{ts: ts, key: createAPIKeyWithMode(t, db, tenantID, false)}
}

func (e invoiceGuardEnv) call(t *testing.T, method, path string, body any) flowResp {
	t.Helper()
	return flowDo(t, e.ts, method, path, flowReq{bearer: e.key, body: body})
}

func (e invoiceGuardEnv) customer(t *testing.T, ext string) string {
	t.Helper()
	return flowStr(e.call(t, "POST", "/v1/customers", map[string]any{
		"external_id": ext, "display_name": ext, "email": ext + "@guards.test",
	}).want(t, 201, "create customer "+ext).body, "id")
}

// addOnInvoice creates a valid one-off invoice and returns its number.
func (e invoiceGuardEnv) addOnInvoice(t *testing.T, customerID string) string {
	t.Helper()
	return flowStr(e.call(t, "POST", "/v1/invoices", map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Setup", "quantity": 1, "unit_amount_cents": 4900}},
	}).want(t, 201, "create valid invoice").body, "invoice_number")
}

func (e invoiceGuardEnv) invoiceCount(t *testing.T, customerID string) int {
	t.Helper()
	return len(flowList(e.call(t, "GET", "/v1/invoices?customer_id="+customerID, nil).want(t, 200, "list invoices").body, "data"))
}

// numberSeq is the trailing sequence of an invoice number (VLX-000042 → 42).
func numberSeq(t *testing.T, number string) int {
	t.Helper()
	i := strings.LastIndexFunc(number, func(r rune) bool { return r < '0' || r > '9' })
	n, err := strconv.Atoi(number[i+1:])
	if err != nil {
		t.Fatalf("invoice number %q has no trailing sequence: %v", number, err)
	}
	return n
}

// wantNextNumber creates a valid invoice and checks it takes the number
// right after prev: the refused request in between used none up.
func (e invoiceGuardEnv) wantNextNumber(t *testing.T, customerID, prev string) {
	t.Helper()
	if next := e.addOnInvoice(t, customerID); numberSeq(t, next) != numberSeq(t, prev)+1 {
		t.Errorf("next invoice number %s after %s: the refused request used up a number", next, prev)
	}
}

func wantInvalid(t *testing.T, r flowResp, param, what string) {
	t.Helper()
	if r.status != 422 {
		t.Errorf("%s: status %d, want 422; body: %s", what, r.status, r.raw)
		return
	}
	if got := flowStr(r.body, "error", "param"); got != param {
		t.Errorf("%s: error param %q, want %q; body: %s", what, got, param, r.raw)
	}
}

func TestInvoiceCreate_RefusesAnotherCustomersSubscription(t *testing.T) {
	e := newInvoiceGuardEnv(t, "Invoice Sub Ownership Co")
	custA, custB := e.customer(t, "owner-a"), e.customer(t, "owner-b")
	planID := flowStr(e.call(t, "POST", "/v1/plans", map[string]any{
		"code": "guards-plan", "name": "Guards Plan", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 1000, "base_bill_timing": "in_arrears",
	}).want(t, 201, "create plan").body, "id")
	subA := flowStr(e.call(t, "POST", "/v1/subscriptions", map[string]any{
		"code": "guards-sub-a", "display_name": "Guards A", "customer_id": custA,
		"items": []map[string]any{{"plan_id": planID, "quantity": 1}}, "billing_time": "anniversary",
	}).want(t, 201, "create subscription on A").body, "id")
	addOn := []map[string]any{{"description": "Add-on", "line_type": "add_on", "quantity": 1, "unit_amount_cents": 4900}}

	first := e.addOnInvoice(t, custB)
	bad := e.call(t, "POST", "/v1/invoices", map[string]any{
		"customer_id": custB, "subscription_id": subA, "line_items": addOn,
	})
	wantInvalid(t, bad, "subscription_id", "B's invoice naming A's subscription")
	if n := e.invoiceCount(t, custB); n != 1 {
		t.Errorf("customer B has %d invoices, want only the first", n)
	}
	e.wantNextNumber(t, custB, first)

	// Control: the owner of the subscription may name it.
	ok := e.call(t, "POST", "/v1/invoices", map[string]any{
		"customer_id": custA, "subscription_id": subA, "line_items": addOn,
	}).want(t, 201, "A's invoice naming A's subscription").body
	if got := flowStr(ok, "subscription_id"); got != subA {
		t.Errorf("control invoice subscription_id %q, want %q", got, subA)
	}
}

func TestInvoiceCreate_RefusesDiscountAndTaxLines(t *testing.T) {
	e := newInvoiceGuardEnv(t, "Invoice Line Types Co")
	for _, lt := range []string{"discount", "tax"} {
		t.Run(lt, func(t *testing.T) {
			cust := e.customer(t, "create-"+lt)
			first := e.addOnInvoice(t, cust)
			r := e.call(t, "POST", "/v1/invoices", map[string]any{
				"customer_id": cust,
				"line_items": []map[string]any{
					{"description": "Setup", "quantity": 1, "unit_amount_cents": 4900},
					{"description": "Operator " + lt, "line_type": lt, "quantity": 1, "unit_amount_cents": 300},
				},
			})
			wantInvalid(t, r, "line_type", "create with a "+lt+" line")
			if n := e.invoiceCount(t, cust); n != 1 {
				t.Errorf("customer has %d invoices, want only the first", n)
			}
			e.wantNextNumber(t, cust, first)
		})
	}
}

// Each commit line passes the per-line rules; only the invoice-level rule
// (one commit per invoice) refuses the pair, and it must do so before the
// number is taken.
func TestInvoiceCreate_TwoCommitLinesUseNoNumber(t *testing.T) {
	e := newInvoiceGuardEnv(t, "Invoice Two Commits Co")
	cust := e.customer(t, "two-commits")
	first := e.addOnInvoice(t, cust)
	commit := func(desc string) map[string]any {
		return map[string]any{"description": desc, "quantity": 1, "unit_amount_cents": 10000, "commit_granted_cents": 10000}
	}
	r := e.call(t, "POST", "/v1/invoices", map[string]any{
		"customer_id": cust, "line_items": []map[string]any{commit("Commit 1"), commit("Commit 2")},
	})
	wantInvalid(t, r, "commit_granted_cents", "create with two commit lines")
	if n := e.invoiceCount(t, cust); n != 1 {
		t.Errorf("customer has %d invoices, want only the first", n)
	}
	e.wantNextNumber(t, cust, first)
}

func TestAddLineItem_RefusesDiscountAndTaxLines(t *testing.T) {
	e := newInvoiceGuardEnv(t, "Invoice Add Line Co")
	cust := e.customer(t, "add-line")
	for _, lt := range []string{"discount", "tax"} {
		t.Run(lt, func(t *testing.T) {
			id := flowStr(e.call(t, "POST", "/v1/invoices", map[string]any{
				"customer_id": cust,
				"line_items":  []map[string]any{{"description": "Setup", "quantity": 1, "unit_amount_cents": 1000}},
			}).want(t, 201, "create draft").body, "id")
			r := e.call(t, "POST", "/v1/invoices/"+id+"/line-items", map[string]any{
				"description": "Operator " + lt, "line_type": lt, "quantity": 1, "unit_amount_cents": 300,
			})
			wantInvalid(t, r, "line_type", "add a "+lt+" line")
			got := e.call(t, "GET", "/v1/invoices/"+id, nil).want(t, 200, "get invoice").body
			if sub := flowInt(got, "invoice", "subtotal_cents"); sub != 1000 {
				t.Errorf("subtotal_cents %d after the refused %s line, want 1000", sub, lt)
			}
			if n := len(flowList(got, "line_items")); n != 1 {
				t.Errorf("%d lines after the refused %s line, want 1", n, lt)
			}
		})
	}
}

// Positive control for the two refusals above: the types an operator may
// add are accepted on both writes and stored as sent; an omitted type is
// an add-on.
func TestInvoiceLines_AcceptOperatorLineTypes(t *testing.T) {
	e := newInvoiceGuardEnv(t, "Invoice Line Accept Co")
	cust := e.customer(t, "accept")
	for _, c := range []struct{ send, want string }{
		{"add_on", "add_on"}, {"base_fee", "base_fee"}, {"usage", "usage"}, {"", "add_on"},
	} {
		t.Run(fmt.Sprintf("type=%q", c.send), func(t *testing.T) {
			line := map[string]any{"description": "Line", "quantity": 1, "unit_amount_cents": 700}
			if c.send != "" {
				line["line_type"] = c.send
			}
			id := flowStr(e.call(t, "POST", "/v1/invoices", map[string]any{
				"customer_id": cust, "line_items": []map[string]any{line},
			}).want(t, 201, "create with line").body, "id")
			lines := flowList(e.call(t, "GET", "/v1/invoices/"+id, nil).want(t, 200, "get invoice").body, "line_items")
			if len(lines) != 1 || flowStr(lines[0], "line_type") != c.want {
				t.Errorf("create: lines %v, want one %s line", lines, c.want)
			}
			added := e.call(t, "POST", "/v1/invoices/"+id+"/line-items", line).want(t, 201, "add line").body
			if got := flowStr(added, "line_type"); got != c.want {
				t.Errorf("add line: line_type %q, want %q", got, c.want)
			}
		})
	}
}
