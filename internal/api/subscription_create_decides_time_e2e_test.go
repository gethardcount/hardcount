package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A live create reads one instant: the subscription's created_at is its
// started_at, as when the time was bound once for the whole create.
func TestE2E_ALiveSubscriptionCreateReadsOneInstant(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, "Live One Instant Co")
	key := createAPIKeyWithMode(t, db, tenantID, true)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	plan := flowStr(call("POST", "/v1/plans", map[string]any{
		"code": "live-pro", "name": "Live Pro", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 4900, "base_bill_timing": "in_advance",
	}).want(t, 201, "live plan").body, "id")
	cust := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "live-one-instant", "display_name": "Live one instant",
	}).want(t, 201, "live customer").body, "id")
	sub := call("POST", "/v1/subscriptions", map[string]any{
		"code": "live-one-instant", "display_name": "Live one instant", "customer_id": cust,
		"items": []map[string]any{{"plan_id": plan, "quantity": 1}}, "billing_time": "anniversary",
	}).want(t, 201, "live create").body
	created, err1 := time.Parse(time.RFC3339Nano, flowStr(sub, "created_at"))
	started, err2 := time.Parse(time.RFC3339Nano, flowStr(sub, "started_at"))
	if err1 != nil || err2 != nil || !created.Equal(started) {
		t.Errorf("created_at %q, started_at %q (%v, %v); want one instant", flowStr(sub, "created_at"), flowStr(sub, "started_at"), err1, err2)
	}
}
