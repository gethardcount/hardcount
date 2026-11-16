package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A credit note a subscription action issues is dated at the time the action
// was decided on. On a test clock that is its frozen time: the cancel route,
// a plan downgrade and a cross-interval plan swap (the door decides each from
// the subscription), and a scheduled cancel the catch-up fires (on the
// clock). Every note the action issues must carry that time as issued_at.
func TestE2E_SubscriptionCreditNotesAreOnTheDecidedTime(t *testing.T) {
	r := newDatesRig(t, "Sub credit note time Co")
	plan := func(code, interval string, cents int) string {
		return flowStr(r.call("POST", "/v1/plans", flowReq{body: map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": interval,
			"base_amount_cents": cents, "base_bill_timing": "in_advance",
		}}).want(t, 201, "create plan "+code).body, "id")
	}
	bigger := plan("cn-time-bigger", "monthly", 9900)
	yearly := plan("cn-time-yearly", "yearly", 99000)
	frozen := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)

	// paidOnClock creates a subscription on its own clock, pays its day-1
	// invoice offline (an unpaid period invoice blocks a plan change), and
	// returns the subscription, its item, its customer and its clock.
	paidOnClock := func(key, planID string) (sub, item, cust, clockID string) {
		clockID = r.newClock(key, frozen)
		sub, item = r.subWithItem(key, clockID, planID)
		r.call("POST", "/v1/invoices/"+flowStr(r.dayOne(sub), "id")+"/record-payment", flowReq{body: map[string]any{"note": "Wire"}}).want(t, 200, key+": pay day-1 offline")
		cust = flowStr(r.call("GET", "/v1/subscriptions/"+sub, flowReq{}).want(t, 200, key+": read subscription").body, "customer_id")
		return sub, item, cust, clockID
	}
	notesAt := func(t *testing.T, cust string, want time.Time) {
		t.Helper()
		notes := flowList(r.call("GET", "/v1/credit-notes?customer_id="+cust+"&limit=100", flowReq{}).want(t, 200, "list credit notes").body, "data")
		if len(notes) == 0 {
			t.Fatal("the action issued no credit note")
		}
		for _, n := range notes {
			if got := at(t, n, "issued_at"); !got.Equal(want) {
				t.Errorf("credit note %s issued_at %s; want %s", n["id"], got, want)
			}
		}
	}
	tenDays := frozen.AddDate(0, 0, 10)

	t.Run("the cancel route", func(t *testing.T) {
		sub, _, cust, clockID := paidOnClock("cn-time-cancel", r.planID)
		r.advance(clockID, tenDays)
		r.call("POST", "/v1/subscriptions/"+sub+"/cancel", flowReq{}).want(t, 200, "cancel now")
		notesAt(t, cust, tenDays)
	})
	t.Run("a plan downgrade", func(t *testing.T) {
		sub, item, cust, clockID := paidOnClock("cn-time-down", bigger)
		r.advance(clockID, tenDays)
		r.call("PATCH", "/v1/subscriptions/"+sub+"/items/"+item, flowReq{body: map[string]any{"new_plan_id": r.planID, "immediate": true}}).want(t, 200, "downgrade now")
		notesAt(t, cust, tenDays)
	})
	t.Run("a cross-interval plan swap", func(t *testing.T) {
		sub, item, cust, clockID := paidOnClock("cn-time-swap", yearly)
		r.advance(clockID, tenDays)
		r.call("PATCH", "/v1/subscriptions/"+sub+"/items/"+item, flowReq{body: map[string]any{"new_plan_id": r.planID, "immediate": true}}).want(t, 200, "swap now")
		notesAt(t, cust, tenDays)
	})
	t.Run("a scheduled cancel the catch-up fires", func(t *testing.T) {
		sub, _, cust, clockID := paidOnClock("cn-time-due", r.planID)
		cancelAt := frozen.AddDate(0, 1, 10)
		r.call("POST", "/v1/subscriptions/"+sub+"/schedule-cancel", flowReq{body: map[string]any{"cancel_at": cancelAt.Format(time.RFC3339)}}).want(t, 200, "schedule the cancel")
		r.advance(clockID, cancelAt)
		if st := flowStr(r.call("GET", "/v1/subscriptions/"+sub, flowReq{}).want(t, 200, "read subscription").body, "status"); st != "canceled" {
			t.Fatalf("status after the advance: %s; want canceled", st)
		}
		notesAt(t, cust, cancelAt)
	})
}

// At real time (a subscription on no test clock) the same actions run on the
// server's clock, the one the server is built with (here a fixed past
// instant, not the machine's), and every credit note they issue carries it.
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
		sub, item = r.subWithItem(key, "", planID)
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
