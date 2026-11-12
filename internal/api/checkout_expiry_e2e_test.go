package api

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// An invoice's open Checkout page is expired at Stripe once the invoice stops
// asking for that money. Recording an offline payment, or issuing a credit
// note that lowers what an unpaid invoice asks for, expires the open session
// after its commit, as a void and a write-off do.
func TestE2E_CheckoutPageExpiresWhenTheInvoiceStopsAsking(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// The hosted page takes payment only with these addresses set.
	t.Setenv("HOSTED_INVOICE_BASE_URL", "https://billing.checkout-expiry.test")
	t.Setenv("CUSTOMER_PORTAL_URL", "https://app.checkout-expiry.test")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Checkout Expiry Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	if _, err := tenantstripe.NewStore(db).Upsert(callerCtx(tenantID, false), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_checkout_expiry", PublishableKey: "pk_test_checkout_expiry",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}

	stripe := installStripeStub(t)
	var mu sync.Mutex
	opened, customers := 0, 0
	stripe.extra = func(call string, form url.Values) (int, string, bool) {
		switch {
		case call == "POST /v1/customers":
			// Each customer gets its own Stripe customer: the id is unique.
			mu.Lock()
			customers++
			n := customers
			mu.Unlock()
			return 200, fmt.Sprintf(`{"id":"cus_expiry_%d","object":"customer"}`, n), true
		case call == "POST /v1/checkout/sessions" && form.Get("mode") == "payment":
			mu.Lock()
			opened++
			n := opened
			mu.Unlock()
			return 200, fmt.Sprintf(`{"id":"cs_pay_%d","object":"checkout.session","mode":"payment","status":"open","url":"https://checkout.stripe.test/c/cs_pay_%d","expires_at":%d}`,
				n, n, time.Now().Add(time.Hour).Unix()), true
		case strings.HasPrefix(call, "POST /v1/checkout/sessions/") && strings.HasSuffix(call, "/expire"):
			id := strings.TrimSuffix(strings.TrimPrefix(call, "POST /v1/checkout/sessions/"), "/expire")
			return 200, fmt.Sprintf(`{"id":%q,"object":"checkout.session","status":"expired"}`, id), true
		}
		return 0, "", false
	}

	newInvoice := func(name string) string {
		t.Helper()
		cust := flowStr(call("POST", "/v1/customers", map[string]any{
			"external_id": "expiry-" + name, "display_name": "Expiry " + name, "email": name + "@checkout-expiry.test",
		}).want(t, 201, name+": create customer").body, "id")
		id := flowStr(call("POST", "/v1/invoices", map[string]any{
			"customer_id": cust,
			"line_items":  []map[string]any{{"description": "Consulting", "quantity": 1, "unit_amount_cents": 5000}},
		}).want(t, 201, name+": create invoice").body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", nil).want(t, 200, name+": finalize")
		return id
	}
	// openPage opens the invoice's hosted Checkout page and returns its
	// Stripe session id.
	openPage := func(invoiceID string) string {
		t.Helper()
		inv := flowObj(call("GET", "/v1/invoices/"+invoiceID, nil).want(t, 200, "get invoice").body, "invoice")
		flowDo(t, ts, "POST", "/v1/public/invoices/"+flowStr(inv, "public_token")+"/checkout", flowReq{}).want(t, 200, "open the Checkout page")
		tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer postgres.Rollback(tx)
		var sessionID string
		if err := tx.QueryRowContext(t.Context(),
			`SELECT stripe_session_id FROM checkout_sessions WHERE invoice_id = $1 AND status = 'open'`, invoiceID).Scan(&sessionID); err != nil {
			t.Fatalf("read the open Checkout session: %v", err)
		}
		return sessionID
	}
	expires := func(sessionID string) int {
		return stripe.count("POST /v1/checkout/sessions/" + sessionID + "/expire")
	}
	creditNote := func(invoiceID string, cents int) {
		t.Helper()
		call("POST", "/v1/credit-notes", map[string]any{
			"invoice_id": invoiceID, "reason": "Discount agreed", "auto_issue": true,
			"lines": []map[string]any{{"description": "Discount", "quantity": 1, "unit_amount_cents": cents}},
		}).want(t, 201, "issue credit note")
	}

	t.Run("an offline payment", func(t *testing.T) {
		inv := newInvoice("offline")
		session := openPage(inv)
		call("POST", "/v1/invoices/"+inv+"/record-payment", map[string]any{"note": "wire"}).want(t, 200, "record offline payment")
		if n := expires(session); n != 1 {
			t.Errorf("expire calls for %s = %d, want 1: the paid invoice's page would take a second payment", session, n)
		}
	})

	t.Run("a credit note for part of what is owed", func(t *testing.T) {
		inv := newInvoice("partial")
		session := openPage(inv)
		creditNote(inv, 2000)
		if n := expires(session); n != 1 {
			t.Errorf("expire calls for %s = %d, want 1: the page still asks for the old amount", session, n)
		}
	})

	t.Run("a credit note for all of what is owed", func(t *testing.T) {
		inv := newInvoice("all")
		session := openPage(inv)
		creditNote(inv, 5000)
		if n := expires(session); n != 1 {
			t.Errorf("expire calls for %s = %d, want 1: nothing is owed, yet the page would take a payment", session, n)
		}
	})
}
