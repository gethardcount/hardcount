package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/testclock"
)

// On the real chain: a credit grant and an adjustment for a clock customer
// whose clock cannot be read are refused for retry (503) and write nothing.
// Once the clock reads again, both are written at the clock's time.
func TestE2E_CreditGrantAndAdjustForAnUnreadableClockAreRefusedForRetry(t *testing.T) {
	r := newDatesRig(t, "Credit Time Co")
	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := r.newClock("credit time", frozen)
	cust := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "credit-time", "display_name": "Credit time", "test_clock_id": clockID,
	}}).want(t, 201, "create clock customer").body, "id")

	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(r.db)}
	r.srv.BillingEngine.SetTestClockReader(clocks)
	clocks.fail.Store(true)

	grant := flowReq{body: map[string]any{"customer_id": cust, "currency": "USD", "amount_cents": 500, "description": "Goodwill"}}
	adjust := flowReq{body: map[string]any{"customer_id": cust, "currency": "USD", "amount_cents": -100, "description": "Deduct"}}
	for name, req := range map[string]flowReq{"grant": grant, "adjust": adjust} {
		if got := r.call("POST", "/v1/credits/"+name, req); got.status != 503 {
			t.Errorf("%s while the clock cannot be read: %d %s; want 503", name, got.status, got.raw)
		}
	}

	clocks.fail.Store(false)
	if entries := flowList(r.call("GET", "/v1/credits/ledger/"+cust, flowReq{}).want(t, 200, "ledger").body, "data"); len(entries) != 0 {
		t.Fatalf("%d ledger entries written while the clock could not be read; want none", len(entries))
	}
	for _, c := range []struct {
		name string
		req  flowReq
	}{{"grant", grant}, {"adjust", adjust}} {
		entry := r.call("POST", "/v1/credits/"+c.name, c.req).want(t, 201, c.name+" once the clock reads").body
		if at, err := time.Parse(time.RFC3339Nano, flowStr(entry, "created_at")); err != nil || !at.Equal(frozen) {
			t.Errorf("%s created_at %q (%v); want the clock's %s", c.name, flowStr(entry, "created_at"), err, frozen)
		}
	}
}
