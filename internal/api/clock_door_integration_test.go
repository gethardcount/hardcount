package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Through the door, a request on a clock customer's route runs at the
// clock's time: a live credit balance is computed at the clock's frozen
// time, and one for a customer on no clock at real time.
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

	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := flowStr(call("POST", "/v1/test-clocks", map[string]any{
		"name": "door clock", "frozen_time": frozen.Format(time.RFC3339),
	}).want(t, 201, "create test clock").body, "id")
	onClock := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "door-on-clock", "display_name": "On a clock", "test_clock_id": clockID,
	}).want(t, 201, "create clock customer").body, "id")
	onNone := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "door-on-none", "display_name": "On no clock",
	}).want(t, 201, "create customer").body, "id")

	computedAt := func(customerID string) time.Time {
		t.Helper()
		s := flowStr(call("GET", "/v1/credits/balance/"+customerID+"/live", nil).want(t, 200, "live balance").body, "computed_at")
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("computed_at %q: %v", s, err)
		}
		return at
	}
	if got := computedAt(onClock); !got.Equal(frozen) {
		t.Errorf("a clock customer's live balance computed at %s, want the clock's %s", got, frozen)
	}
	if got := computedAt(onNone); time.Since(got) > time.Minute || got.Before(frozen.Add(24*time.Hour)) {
		t.Errorf("a customer on no clock: computed at %s, want real time", got)
	}
}
