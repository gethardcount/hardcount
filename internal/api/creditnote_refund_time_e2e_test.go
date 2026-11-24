package api

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A dismissal of a refund made in Stripe and an attach of one to a credit
// note run on the instant the API's door bound for their request: one read
// of the server's clock, the one the server is built with, not the
// machine's.
func TestE2E_StripeRefund_DismissAndAttachAreOnTheRequestsTime(t *testing.T) {
	serverAt := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	db, ts, _, register := newRefundServerOn(t, clock.NewFake(serverAt))
	f := newRefundFlow(t, db, ts, "Refund time real Co")
	register(f)

	// Attach: a Stripe refund replaces Hardcount's failed one. First, so the
	// credit-balance note below leaves no credit on this invoice.
	created := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	replInv, replPI := f.paidInvoice("rt_repl_real", 10000, 0)
	f.mu.Lock()
	f.refundStatus = "failed"
	f.mu.Unlock()
	cn := f.call("POST", "/v1/invoices/"+replInv+"/refund", map[string]any{"amount_cents": 5500, "reason": "requested_by_customer"}).want(t, 200, "Hardcount refund").body
	f.deliver(refundEvent("evt_rt_repl_real", "refund.created", refundSpec{id: "re_rt_repl_real", pi: replPI, status: "succeeded", amount: 5500, created: created}, created)).want(t, 200, "dashboard refund")
	f.mu.Lock()
	f.stripeRefunds["re_rt_repl_real"] = refundJSON("re_rt_repl_real", replPI, 5500, "succeeded", nil)
	f.mu.Unlock()
	got := f.call("POST", "/v1/credit-notes/"+flowStr(cn, "id")+"/attach-stripe-refund", map[string]any{"stripe_refund_id": "re_rt_repl_real"}).want(t, 200, "attach").body
	if at, err := time.Parse(time.RFC3339Nano, flowStr(got, "updated_at")); flowStr(got, "refund_status") != "succeeded" || err != nil || !at.Equal(serverAt) {
		t.Errorf("after the attach: refund_status %s, updated_at %q (%v); want succeeded at %s", flowStr(got, "refund_status"), flowStr(got, "updated_at"), err, serverAt)
	}

	// Dismissal: a refund over the creditable amount waits for review.
	overInv, overPI := f.paidInvoice("rt_over_real", 10000, 0)
	f.call("POST", "/v1/credit-notes", map[string]any{
		"invoice_id": overInv, "reason": "goodwill", "auto_issue": true, "credit_amount_cents": 5000,
		"lines": []map[string]any{{"description": "Goodwill", "quantity": 1, "unit_amount_cents": 5000}},
	}).want(t, 201, "credit-balance note")
	f.deliver(refundEvent("evt_rt_over_real", "refund.created", refundSpec{id: "re_rt_over_real", pi: overPI, status: "succeeded", amount: 10000, created: created}, created)).want(t, 200, "refund.created")
	d := f.call("POST", "/v1/invoices/"+overInv+"/stripe-refunds/re_rt_over_real/dismiss", map[string]any{"note": "taken back by hand"}).want(t, 200, "dismiss")
	if at, err := time.Parse(time.RFC3339Nano, flowStr(d.body, "dismissed_at")); err != nil || !at.Equal(serverAt) {
		t.Errorf("dismissed_at %q (%v); want %s", flowStr(d.body, "dismissed_at"), err, serverAt)
	}
}
