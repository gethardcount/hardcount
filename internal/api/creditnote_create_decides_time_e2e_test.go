package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/testclock"
)

// On the real chain: a credit note on a clock invoice whose clock cannot be
// read is refused for retry (503) and creates nothing. Once the clock reads
// again, the same request with auto_issue creates and issues it, both at the
// clock's time.
func TestE2E_CreditNoteCreateForAnUnreadableClockIsRefusedForRetry(t *testing.T) {
	r := newDatesRig(t, "Credit Note Time Co")
	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := r.newClock("credit note time", frozen)
	cust := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "credit-note-time", "display_name": "Credit note time", "test_clock_id": clockID,
	}}).want(t, 201, "create clock customer").body, "id")
	inv := flowStr(r.call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": cust,
		"line_items":  []map[string]any{{"description": "Setup", "quantity": 1, "unit_amount_cents": 4900}},
	}}).want(t, 201, "create invoice").body, "id")
	r.call("POST", "/v1/invoices/"+inv+"/finalize", flowReq{}).want(t, 200, "finalize invoice")

	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(r.db)}
	r.srv.BillingEngine.SetTestClockReader(clocks)
	clocks.fail.Store(true)

	note := flowReq{body: map[string]any{
		"invoice_id": inv, "reason": "Billing error", "auto_issue": true,
		"lines": []map[string]any{{"description": "Billing error", "quantity": 1, "unit_amount_cents": 1000}},
	}}
	if got := r.call("POST", "/v1/credit-notes", note); got.status != 503 {
		t.Errorf("credit note while the clock cannot be read: %d %s; want 503", got.status, got.raw)
	}
	if notes := flowList(r.call("GET", "/v1/credit-notes?invoice_id="+inv, flowReq{}).want(t, 200, "list credit notes").body, "data"); len(notes) != 0 {
		t.Fatalf("%d credit notes created while the clock could not be read; want none", len(notes))
	}

	clocks.fail.Store(false)
	cn := r.call("POST", "/v1/credit-notes", note).want(t, 201, "the same credit note once the clock reads").body
	for _, field := range []string{"created_at", "issued_at"} {
		if at, err := time.Parse(time.RFC3339Nano, flowStr(cn, field)); err != nil || !at.Equal(frozen) {
			t.Errorf("%s %q (%v); want the clock's %s", field, flowStr(cn, field), err, frozen)
		}
	}
}
