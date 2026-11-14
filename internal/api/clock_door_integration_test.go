package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testclock"
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

// unreadableClocks is the real test-clock store, except that while fail is
// set no clock can be read.
type unreadableClocks struct {
	billing.TestClockReader
	fail atomic.Bool
}

func (u *unreadableClocks) Get(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	if u.fail.Load() {
		return domain.TestClock{}, errors.New("connection reset by peer")
	}
	return u.TestClockReader.Get(ctx, tenantID, id)
}

// On the real resolver and the real chain: a write on a clock subscription
// whose clock cannot be read is answered 503 naming the subscription, and
// nothing runs. The idempotency layer does not store that answer, so the
// retry with the same Idempotency-Key runs the cancel, dated at the clock's
// time. A subscription that does not exist is still the handler's 404 while
// clocks cannot be read: the resolver's not-found reaches the door as one.
func TestClockDoor_AClockThatCannotBeReadIsRetriedNotReplayed(t *testing.T) {
	r := newDatesRig(t, "Clock Door Unreadable Co")
	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	subID, _ := r.trialSub("door-unreadable", r.newClock("door unreadable", frozen))
	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(r.db)}
	r.srv.BillingEngine.SetTestClockReader(clocks)

	clocks.fail.Store(true)
	cancel := flowReq{header: map[string]string{"Idempotency-Key": "door-unreadable-cancel"}}
	refused := r.call("POST", "/v1/subscriptions/"+subID+"/cancel", cancel)
	want := "could not read the test clock of subscription " + subID + "; retry"
	if refused.status != 503 || !strings.Contains(string(refused.raw), want) {
		t.Fatalf("cancel while the clock cannot be read: %d %s; want 503 %q", refused.status, refused.raw, want)
	}
	r.call("POST", "/v1/subscriptions/sub_door_none/cancel", flowReq{}).want(t, 404, "cancel a subscription that does not exist")

	clocks.fail.Store(false)
	if status := flowStr(r.call("GET", "/v1/subscriptions/"+subID, flowReq{}).want(t, 200, "read the subscription").body, "status"); status == "canceled" {
		t.Fatal("the refused cancel ran")
	}
	again := r.call("POST", "/v1/subscriptions/"+subID+"/cancel", cancel).want(t, 200, "retry the cancel with the same key")
	canceledAt, err := time.Parse(time.RFC3339Nano, flowStr(again.body, "canceled_at"))
	if err != nil || !canceledAt.Equal(frozen) {
		t.Errorf("the retried cancel: canceled_at %q (%v); want the clock's %s", flowStr(again.body, "canceled_at"), err, frozen)
	}
}
