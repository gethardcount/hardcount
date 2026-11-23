package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A live create reads one instant: a one-off invoice with no billing window
// has its period start at its created_at.
func TestE2E_ALiveInvoiceCreateReadsOneInstant(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, "Live Invoice One Instant Co")
	key := createAPIKeyWithMode(t, db, tenantID, true)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	cust := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "live-invoice-one-instant", "display_name": "Live invoice one instant",
	}).want(t, 201, "live customer").body, "id")
	inv := call("POST", "/v1/invoices", map[string]any{
		"customer_id": cust,
		"line_items":  []map[string]any{{"description": "Setup", "quantity": 1, "unit_amount_cents": 4900}},
	}).want(t, 201, "live invoice").body
	created, err1 := time.Parse(time.RFC3339Nano, flowStr(inv, "created_at"))
	start, err2 := time.Parse(time.RFC3339Nano, flowStr(inv, "billing_period_start"))
	if err1 != nil || err2 != nil || !created.Equal(start) {
		t.Errorf("created_at %q, billing_period_start %q (%v, %v); want one instant", flowStr(inv, "created_at"), flowStr(inv, "billing_period_start"), err1, err2)
	}
}

// An invoice whose body names another customer's subscription is refused
// (422) with nothing created, before any time is decided.
func TestE2E_InvoiceCreateForAnotherCustomersSubscriptionIsRefused(t *testing.T) {
	r := newDatesRig(t, "Invoice Other Sub Co")
	_, _ = r.trialSub("owner")
	owner := flowList(r.call("GET", "/v1/subscriptions", flowReq{}).want(t, 200, "list subscriptions").body, "data")
	if len(owner) != 1 {
		t.Fatalf("%d subscriptions; want the owner's one", len(owner))
	}
	other := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "other-customer", "display_name": "Other customer",
	}}).want(t, 201, "create the other customer").body, "id")
	got := r.call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": other, "subscription_id": flowStr(owner[0], "id"),
		"line_items": []map[string]any{{"description": "Setup", "quantity": 1, "unit_amount_cents": 4900}},
	}})
	if got.status != 422 {
		t.Errorf("an invoice on another customer's subscription: %d %s; want 422", got.status, got.raw)
	}
	if invs := flowList(r.call("GET", "/v1/invoices?customer_id="+other, flowReq{}).want(t, 200, "list invoices").body, "data"); len(invs) != 0 {
		t.Errorf("%d invoices created for the other customer; want none", len(invs))
	}
}
