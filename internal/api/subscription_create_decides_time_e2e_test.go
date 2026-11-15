package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
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

	// Its first in-advance invoice is on the clock too: issued at the clock's
	// time, for a period that starts on the clock's day, due on the clock's
	// timeline (real time is more than a year later).
	invs := flowList(r.call("GET", "/v1/invoices?subscription_id="+flowStr(sub, "id"), flowReq{}).want(t, 200, "list invoices").body, "data")
	if len(invs) != 1 {
		t.Fatalf("%d invoices for the new subscription; want its first in-advance invoice", len(invs))
	}
	at := func(field string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, flowStr(invs[0], field))
		if err != nil {
			t.Fatalf("invoice %s %q: %v", field, flowStr(invs[0], field), err)
		}
		return v
	}
	if issued := at("issued_at"); !issued.Equal(frozen) {
		t.Errorf("issued_at %s; want the clock's %s", issued, frozen)
	}
	if start := at("billing_period_start"); start.Format(time.DateOnly) != frozen.Format(time.DateOnly) {
		t.Errorf("billing_period_start %s; want the clock's day %s", start, frozen.Format(time.DateOnly))
	}
	if due := at("due_at"); due.Before(frozen) || due.After(frozen.AddDate(0, 0, 60)) {
		t.Errorf("due_at %s; want on the clock's timeline (from %s)", due, frozen)
	}
}

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
		"items": []map[string]any{{"plan_id": plan, "quantity": 1}}, "billing_time": "anniversary", "trial_days": 14,
	}).want(t, 201, "live create").body
	created, err1 := time.Parse(time.RFC3339Nano, flowStr(sub, "created_at"))
	started, err2 := time.Parse(time.RFC3339Nano, flowStr(sub, "started_at"))
	if err1 != nil || err2 != nil || !created.Equal(started) {
		t.Errorf("created_at %q, started_at %q (%v, %v); want one instant", flowStr(sub, "created_at"), flowStr(sub, "started_at"), err1, err2)
	}
}
