package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// On the real chain: the hosted page decides its time from the invoice its
// token names, so a clock invoice's checkout audit row carries the clock's
// stamp. A clock that cannot be read answers 503 and opens no Stripe
// session.
func TestE2E_HostedCheckoutIsDecidedFromTheInvoicesClock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// The hosted page takes payment only with these addresses set.
	t.Setenv("HOSTED_INVOICE_BASE_URL", "https://billing.hosted-time.test")
	t.Setenv("CUSTOMER_PORTAL_URL", "https://app.hosted-time.test")
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Hosted Time Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	if _, err := tenantstripe.NewStore(db).Upsert(callerCtx(tenantID, false), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_hosted_time", PublishableKey: "pk_test_hosted_time",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	stripe := installStripeStub(t)
	opened := 0
	stripe.extra = func(call string, form url.Values) (int, string, bool) {
		switch {
		case call == "POST /v1/customers":
			return 200, `{"id":"cus_hosted_time","object":"customer"}`, true
		case call == "POST /v1/checkout/sessions" && form.Get("mode") == "payment":
			opened++
			return 200, fmt.Sprintf(`{"id":"cs_time_%d","object":"checkout.session","mode":"payment","status":"open","url":"https://checkout.stripe.test/c/cs_time_%d","expires_at":%d}`,
				opened, opened, time.Now().Add(time.Hour).Unix()), true
		}
		return 0, "", false
	}

	frozen := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	clockID := flowStr(call("POST", "/v1/test-clocks", map[string]any{
		"name": "hosted time", "frozen_time": frozen.Format(time.RFC3339),
	}).want(t, 201, "create test clock").body, "id")
	newInvoice := func(name string) (string, string) {
		t.Helper()
		cust := flowStr(call("POST", "/v1/customers", map[string]any{
			"external_id": "hosted-time-" + name, "display_name": "Hosted " + name, "email": name + "@hosted-time.test", "test_clock_id": clockID,
		}).want(t, 201, name+": create customer").body, "id")
		id := flowStr(call("POST", "/v1/invoices", map[string]any{
			"customer_id": cust,
			"line_items":  []map[string]any{{"description": "Consulting", "quantity": 1, "unit_amount_cents": 5000}},
		}).want(t, 201, name+": create invoice").body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", nil).want(t, 200, name+": finalize")
		inv := flowObj(call("GET", "/v1/invoices/"+id, nil).want(t, 200, name+": get invoice").body, "invoice")
		return id, flowStr(inv, "public_token")
	}

	paid, paidToken := newInvoice("readable")
	_, unreadToken := newInvoice("unreadable")

	flowDo(t, ts, "POST", "/v1/public/invoices/"+paidToken+"/checkout", flowReq{}).want(t, 200, "open the Checkout page")
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var at *time.Time
	var stampedClock string
	err = tx.QueryRowContext(context.Background(), `
		SELECT sim_effective_at, COALESCE(test_clock_id, '') FROM audit_log
		WHERE resource_id = $1 AND metadata->>'action' = 'hosted_checkout_started'`, paid).Scan(&at, &stampedClock)
	postgres.Rollback(tx)
	if err != nil {
		t.Fatalf("read the checkout's audit row: %v", err)
	}
	if at == nil || !at.Equal(frozen) || stampedClock != clockID {
		t.Errorf("the checkout's audit row is stamped %v on clock %q; want %s on %s", at, stampedClock, frozen, clockID)
	}

	clocks := &unreadableClocks{TestClockReader: testclock.NewPostgresStore(db)}
	srv.BillingEngine.SetTestClockReader(clocks)
	clocks.fail.Store(true)
	before := opened
	refused := flowDo(t, ts, "POST", "/v1/public/invoices/"+unreadToken+"/checkout", flowReq{})
	if refused.status != 503 || opened != before {
		t.Errorf("a checkout whose clock cannot be read: %d, sessions opened %d; want 503 and none", refused.status, opened-before)
	}
}
