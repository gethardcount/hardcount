package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// With the server's clock at a fixed past instant, a card payment is paid at
// the server's instant, not the machine's: the settle reads the result's time
// from the clock the server is built with.
func TestE2E_APaymentIsDatedByTheServersClock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	ts := httptest.NewServer(NewServer(db, clock.NewFake(past), nil))
	t.Cleanup(ts.Close)

	tenantID := testutil.CreateTestTenant(t, db, "Payment Server Clock Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "payment-server-clock", "display_name": "Payment server clock",
	}}).want(t, 201, "create customer").body, "id")
	invID := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Service", "quantity": 1, "unit_amount_cents": 4500}},
	}}).want(t, 201, "create invoice").body, "id")
	inv := call("POST", "/v1/invoices/"+invID+"/finalize", flowReq{}).want(t, 200, "finalize").body

	if _, err := tenantstripe.NewStore(db).Upsert(callerCtx(tenantID, false), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_payment_server_clock", PublishableKey: "pk_test_payment_server_clock",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	const signingSecret = "whsec_payment_server_clock"
	creds := call("PATCH", "/v1/settings/stripe/test/webhook", flowReq{body: map[string]any{"webhook_secret": signingSecret}}).want(t, 200, "set webhook secret").body
	installStripeStub(t)
	payload, sig := signedStripeEvent(t, signingSecret, map[string]any{
		"id": "evt_payment_server_clock", "object": "event", "type": "payment_intent.succeeded",
		"created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{
			"id": "pi_payment_server_clock", "object": "payment_intent", "status": "succeeded", "amount": 4500,
			"currency": strings.ToLower(flowStr(inv, "currency")),
			"metadata": map[string]any{"hardcount_tenant_id": tenantID, "hardcount_invoice_id": invID, "hardcount_customer_id": customerID},
		}},
	})
	flowDo(t, ts, "POST", "/v1/webhooks/stripe/"+flowStr(creds, "id"), flowReq{header: map[string]string{"Stripe-Signature": sig}, body: payload}).want(t, 200, "payment_intent.succeeded")

	paid := flowObj(call("GET", "/v1/invoices/"+invID, flowReq{}).want(t, 200, "get invoice").body, "invoice")
	if at, err := time.Parse(time.RFC3339Nano, flowStr(paid, "paid_at")); flowStr(paid, "status") != "paid" || err != nil || !at.Equal(past) {
		t.Errorf("status %s, paid_at %q (%v); want paid at the server's %s", flowStr(paid, "status"), flowStr(paid, "paid_at"), err, past)
	}
}
