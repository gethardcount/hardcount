package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// On the real chain: creating an invoice for a clock customer whose clock
// cannot be read is refused for retry (503) and creates nothing. Once the
// clock reads again, the same request creates it at the clock's time.
func TestE2E_InvoiceCreateForAnUnreadableClockIsRefusedForRetry(t *testing.T) {
	r := newDatesRig(t, "Invoice Create Time Co")
	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := r.newClock("invoice create time", frozen)
	cust := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "invoice-create-time", "display_name": "Invoice create time", "test_clock_id": clockID,
	}}).want(t, 201, "create clock customer").body, "id")

	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(r.db)}
	r.srv.BillingEngine.SetTestClockReader(clocks)
	clocks.fail.Store(true)

	create := flowReq{body: map[string]any{
		"customer_id": cust,
		"line_items":  []map[string]any{{"description": "Setup", "quantity": 1, "unit_amount_cents": 4900}},
	}}
	if got := r.call("POST", "/v1/invoices", create); got.status != 503 {
		t.Errorf("create while the clock cannot be read: %d %s; want 503", got.status, got.raw)
	}
	if invs := flowList(r.call("GET", "/v1/invoices?customer_id="+cust, flowReq{}).want(t, 200, "list invoices").body, "data"); len(invs) != 0 {
		t.Fatalf("%d invoices created while the clock could not be read; want none", len(invs))
	}

	clocks.fail.Store(false)
	inv := r.call("POST", "/v1/invoices", create).want(t, 201, "the same create once the clock reads").body
	if at, err := time.Parse(time.RFC3339Nano, flowStr(inv, "created_at")); err != nil || !at.Equal(frozen) {
		t.Errorf("created_at %q (%v); want the clock's %s", flowStr(inv, "created_at"), err, frozen)
	}
}

// A live create reads one instant: a one-off invoice with no billing window
// has its period start at its created_at, as when the time was bound once
// for the whole create.
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
