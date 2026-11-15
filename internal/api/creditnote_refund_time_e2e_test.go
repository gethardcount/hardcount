package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A dismissal of a refund made in Stripe and an attach of one to a credit
// note run on the time the API's door decided for their request: on a test
// clock, its frozen time; at real time, the server's clock, the one the
// server is built with, not the machine's.
func TestE2E_StripeRefund_DismissAndAttachAreOnTheRequestsTime(t *testing.T) {
	check := func(t *testing.T, f *refundFlow, tag string, want time.Time) {
		t.Helper()
		// Attach: a Stripe refund replaces Hardcount's failed one. First, so the
		// credit-balance note below leaves no credit on this invoice.
		created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
		replInv, replPI := f.paidInvoice("rt_repl_"+tag, 10000, 0)
		f.mu.Lock()
		f.refundStatus = "failed"
		f.mu.Unlock()
		cn := f.call("POST", "/v1/invoices/"+replInv+"/refund", map[string]any{"amount_cents": 5500, "reason": "requested_by_customer"}).want(t, 200, "Hardcount refund").body
		f.deliver(refundEvent("evt_rt_repl_"+tag, "refund.created", refundSpec{id: "re_rt_repl_" + tag, pi: replPI, status: "succeeded", amount: 5500, created: created}, created)).want(t, 200, "dashboard refund")
		f.mu.Lock()
		f.stripeRefunds["re_rt_repl_"+tag] = refundJSON("re_rt_repl_"+tag, replPI, 5500, "succeeded", nil)
		f.mu.Unlock()
		got := f.call("POST", "/v1/credit-notes/"+flowStr(cn, "id")+"/attach-stripe-refund", map[string]any{"stripe_refund_id": "re_rt_repl_" + tag}).want(t, 200, "attach").body
		if at, err := time.Parse(time.RFC3339Nano, flowStr(got, "updated_at")); flowStr(got, "refund_status") != "succeeded" || err != nil || !at.Equal(want) {
			t.Errorf("after the attach: refund_status %s, updated_at %q (%v); want succeeded at %s", flowStr(got, "refund_status"), flowStr(got, "updated_at"), err, want)
		}

		// Dismissal: a refund over the creditable amount waits for review.
		overInv, overPI := f.paidInvoice("rt_over_"+tag, 10000, 0)
		f.call("POST", "/v1/credit-notes", map[string]any{
			"invoice_id": overInv, "reason": "goodwill", "auto_issue": true, "credit_amount_cents": 5000,
			"lines": []map[string]any{{"description": "Goodwill", "quantity": 1, "unit_amount_cents": 5000}},
		}).want(t, 201, "credit-balance note")
		f.deliver(refundEvent("evt_rt_over_"+tag, "refund.created", refundSpec{id: "re_rt_over_" + tag, pi: overPI, status: "succeeded", amount: 10000, created: created}, created)).want(t, 200, "refund.created")
		d := f.call("POST", "/v1/invoices/"+overInv+"/stripe-refunds/re_rt_over_"+tag+"/dismiss", map[string]any{"note": "taken back by hand"}).want(t, 200, "dismiss")
		if at, err := time.Parse(time.RFC3339Nano, flowStr(d.body, "dismissed_at")); err != nil || !at.Equal(want) {
			t.Errorf("dismissed_at %q (%v); want %s", flowStr(d.body, "dismissed_at"), err, want)
		}
	}

	t.Run("at real time, on the server's clock", func(t *testing.T) {
		serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
		db, ts, _, register := newRefundServerOn(t, clock.NewFake(serverAt))
		f := newRefundFlow(t, db, ts, "Refund time real Co")
		register(f)
		check(t, f, "real", serverAt)
	})

	t.Run("on a test clock, at its frozen time", func(t *testing.T) {
		db, ts, _, register := newRefundServer(t)
		f := newRefundFlow(t, db, ts, "Refund time clock Co")
		register(f)
		frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		clockID := flowStr(f.call("POST", "/v1/test-clocks", map[string]any{
			"name": "Refund time clock", "frozen_time": frozen.Format(time.RFC3339),
		}).want(t, 201, "create test clock").body, "id")
		f.customerID = flowStr(f.call("POST", "/v1/customers", map[string]any{
			"external_id": "refund-time-clock", "display_name": "Refund time clock", "test_clock_id": clockID,
		}).want(t, 201, "create customer on the clock").body, "id")
		check(t, f, "clock", frozen)
	})
}
