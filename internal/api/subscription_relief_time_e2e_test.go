package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A credit note a subscription action issues (the cancel route, a plan
// downgrade, a cross-interval plan swap) is dated at the time the action was
// decided on: at real time, the server's clock, the one the server is built
// with (here a fixed past instant, not the machine's).
func TestE2E_SubscriptionCreditNotesAtRealTimeAreOnTheServersClock(t *testing.T) {
	serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	r := newDatesRigOn(t, "Sub credit note real time Co", clock.NewFake(serverAt))
	plan := func(code, interval string, cents int) string {
		return flowStr(r.call("POST", "/v1/plans", flowReq{body: map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": interval,
			"base_amount_cents": cents, "base_bill_timing": "in_advance",
		}}).want(t, 201, "create plan "+code).body, "id")
	}
	bigger := plan("cn-real-bigger", "monthly", 9900)
	yearly := plan("cn-real-yearly", "yearly", 99000)
	paid := func(key, planID string) (sub, item, cust string) {
		sub, item = r.subWithItem(key, planID)
		r.call("POST", "/v1/invoices/"+flowStr(r.dayOne(sub), "id")+"/record-payment", flowReq{body: map[string]any{"note": "Wire"}}).want(t, 200, key+": pay day-1 offline")
		cust = flowStr(r.call("GET", "/v1/subscriptions/"+sub, flowReq{}).want(t, 200, key+": read subscription").body, "customer_id")
		return sub, item, cust
	}
	notesAt := func(t *testing.T, cust string) {
		t.Helper()
		notes := flowList(r.call("GET", "/v1/credit-notes?customer_id="+cust+"&limit=100", flowReq{}).want(t, 200, "list credit notes").body, "data")
		if len(notes) == 0 {
			t.Fatal("the action issued no credit note")
		}
		for _, n := range notes {
			if got := at(t, n, "issued_at"); !got.Equal(serverAt) {
				t.Errorf("credit note %s issued_at %s; want the server's %s", n["id"], got, serverAt)
			}
		}
	}

	t.Run("the cancel route", func(t *testing.T) {
		sub, _, cust := paid("cn-real-cancel", r.planID)
		r.call("POST", "/v1/subscriptions/"+sub+"/cancel", flowReq{}).want(t, 200, "cancel now")
		notesAt(t, cust)
	})
	t.Run("a plan downgrade", func(t *testing.T) {
		sub, item, cust := paid("cn-real-down", bigger)
		r.call("PATCH", "/v1/subscriptions/"+sub+"/items/"+item, flowReq{body: map[string]any{"new_plan_id": r.planID, "immediate": true}}).want(t, 200, "downgrade now")
		notesAt(t, cust)
	})
	t.Run("a cross-interval plan swap", func(t *testing.T) {
		sub, item, cust := paid("cn-real-swap", yearly)
		r.call("PATCH", "/v1/subscriptions/"+sub+"/items/"+item, flowReq{body: map[string]any{"new_plan_id": r.planID, "immediate": true}}).want(t, 200, "swap now")
		notesAt(t, cust)
	})
}
