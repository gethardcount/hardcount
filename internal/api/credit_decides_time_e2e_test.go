package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// With the server's clock at a fixed past instant, a live grant is dated at
// that instant, and one expiring a day after it, long before the machine's
// time, is accepted.
func TestE2E_ALiveGrantIsDatedByTheServersClock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	ts := httptest.NewServer(NewServer(db, clock.NewFake(past), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, "Live Grant Server Clock Co")
	key := createAPIKeyWithMode(t, db, tenantID, true)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	cust := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "live-grant-server-clock", "display_name": "Live grant server clock",
	}).want(t, 201, "live customer").body, "id")
	entry := call("POST", "/v1/credits/grant", map[string]any{
		"customer_id": cust, "currency": "USD", "amount_cents": 500, "description": "Goodwill",
		"expires_at": past.Add(24 * time.Hour).Format(time.RFC3339),
	}).want(t, 201, "live grant expiring a day after the server's time").body
	if at, err := time.Parse(time.RFC3339Nano, flowStr(entry, "created_at")); err != nil || !at.Equal(past) {
		t.Errorf("created_at %q (%v); want the server's %s", flowStr(entry, "created_at"), err, past)
	}
}
