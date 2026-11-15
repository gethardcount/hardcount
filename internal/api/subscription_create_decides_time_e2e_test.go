package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/testclock"
)

// On the real chain: creating a subscription for a clock customer whose clock
// cannot be read is refused for retry (503) and creates nothing. Once the
// clock reads again, the same request creates it at the clock's time.
func TestE2E_SubscriptionCreateForAnUnreadableClockIsRefusedForRetry(t *testing.T) {
	r := newDatesRig(t, "Sub Create Time Co")
	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := r.newClock("sub create time", frozen)
	cust := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "sub-create-time", "display_name": "Sub create time", "test_clock_id": clockID,
	}}).want(t, 201, "create clock customer").body, "id")

	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(r.db)}
	r.srv.BillingEngine.SetTestClockReader(clocks)
	clocks.fail.Store(true)

	create := flowReq{body: map[string]any{
		"code": "sub-create-time", "display_name": "Sub create time", "customer_id": cust,
		"items": []map[string]any{{"plan_id": r.planID, "quantity": 1}}, "billing_time": "anniversary",
	}}
	if got := r.call("POST", "/v1/subscriptions", create); got.status != 503 {
		t.Errorf("create while the clock cannot be read: %d %s; want 503", got.status, got.raw)
	}
	if subs := flowList(r.call("GET", "/v1/subscriptions?customer_id="+cust, flowReq{}).want(t, 200, "list subscriptions").body, "data"); len(subs) != 0 {
		t.Fatalf("%d subscriptions created while the clock could not be read; want none", len(subs))
	}

	clocks.fail.Store(false)
	sub := r.call("POST", "/v1/subscriptions", create).want(t, 201, "the same create once the clock reads").body
	if at, err := time.Parse(time.RFC3339Nano, flowStr(sub, "created_at")); err != nil || !at.Equal(frozen) {
		t.Errorf("created_at %q (%v); want the clock's %s", flowStr(sub, "created_at"), err, frozen)
	}
}
