package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Through the door, a request on the route of a customer on no clock runs at
// real time: its live credit balance is computed now.
func TestClockDoor_ALiveBalanceRunsAtTheCustomersTime(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()
	tenantID := testutil.CreateTestTenant(t, db, "Clock Door Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}

	onNone := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "door-on-none", "display_name": "On no clock",
	}).want(t, 201, "create customer").body, "id")

	s := flowStr(call("GET", "/v1/credits/balance/"+onNone+"/live", nil).want(t, 200, "live balance").body, "computed_at")
	got, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("computed_at %q: %v", s, err)
	}
	if time.Since(got) > time.Minute {
		t.Errorf("a customer on no clock: computed at %s, want real time", got)
	}
}
