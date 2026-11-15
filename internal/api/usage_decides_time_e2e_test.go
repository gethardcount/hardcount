package api

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
)

// On the real chain: usage for a clock customer whose clock cannot be read
// is refused for retry (503), single or in a batch, and nothing is recorded;
// the customer's public cost dashboard answers 503 too. Once the clock reads
// again, the same event is recorded at the clock's time.
func TestE2E_UsageForAnUnreadableClockIsRefusedForRetry(t *testing.T) {
	r := newDatesRig(t, "Usage Time Co")
	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := r.newClock("usage time", frozen)
	flowStr(r.call("POST", "/v1/meters", flowReq{body: map[string]any{
		"event_name": "calls", "name": "calls", "unit": "units", "aggregation": "sum",
	}}).want(t, 201, "create meter").body, "id")
	cust := flowStr(r.call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "usage-time", "display_name": "Usage time", "test_clock_id": clockID,
	}}).want(t, 201, "create clock customer").body, "id")
	token := flowStr(r.call("POST", "/v1/customers/"+cust+"/rotate-cost-dashboard-token", flowReq{}).want(t, 200, "rotate token").body, "token")

	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(r.db)}
	r.srv.BillingEngine.SetTestClockReader(clocks)
	clocks.fail.Store(true)

	event := map[string]any{"external_customer_id": "usage-time", "event_name": "calls", "quantity": 3, "idempotency_key": "usage-time-1"}
	if got := r.call("POST", "/v1/usage-events", flowReq{body: event}); got.status != 503 {
		t.Errorf("single event while the clock cannot be read: %d %s; want 503", got.status, got.raw)
	}
	if got := r.call("POST", "/v1/usage-events/batch", flowReq{body: []map[string]any{event}}); got.status != 503 {
		t.Errorf("batch while the clock cannot be read: %d %s; want 503", got.status, got.raw)
	}
	if got := flowDo(t, r.ts, "GET", "/v1/public/cost-dashboard/"+token, flowReq{}); got.status != 503 {
		t.Errorf("cost dashboard while the clock cannot be read: %d %s; want 503", got.status, got.raw)
	}
	if n := usageEventsOf(t, r.db, cust); n != 0 {
		t.Fatalf("%d usage events recorded while the clock could not be read; want none", n)
	}

	clocks.fail.Store(false)
	recorded := r.call("POST", "/v1/usage-events", flowReq{body: event}).want(t, 201, "the same event once the clock reads").body
	if at, err := time.Parse(time.RFC3339Nano, flowStr(recorded, "timestamp")); err != nil || !at.Equal(frozen) {
		t.Errorf("recorded at %q (%v); want the clock's %s", flowStr(recorded, "timestamp"), err, frozen)
	}
	flowDo(t, r.ts, "GET", "/v1/public/cost-dashboard/"+token, flowReq{}).want(t, 200, "cost dashboard once the clock reads")
}

// usageEventsOf counts the customer's stored usage events.
func usageEventsOf(t *testing.T, db *postgres.DB, customerID string) int {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(context.Background(), `SELECT count(*) FROM usage_events WHERE customer_id = $1`, customerID).Scan(&n); err != nil {
		t.Fatalf("count usage events: %v", err)
	}
	return n
}
