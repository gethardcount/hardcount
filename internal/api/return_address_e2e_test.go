package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_ReturnAddressesUnset walks every flow that sends a customer to
// Stripe and back, over the real router, on a server with none of the three
// return addresses set: HOSTED_INVOICE_BASE_URL, CUSTOMER_PORTAL_URL and
// PAYMENT_UPDATE_RETURN_URL. Each used to fall back to http://localhost:5173
// in every environment, so in production a customer paid or saved a card and
// landed on "site can't be reached". Now each flow refuses before any Stripe
// call, and nothing claims a link was sent.
func TestE2E_ReturnAddressesUnset(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("SMTP_HOST", "smtp.return-address.invalid")
	t.Setenv("PAYMENT_UPDATE_URL", "https://billing.return-address.test/update-payment")
	t.Setenv("PAYMENT_UPDATE_RETURN_URL", "")
	t.Setenv("CUSTOMER_PORTAL_URL", "")
	t.Setenv("HOSTED_INVOICE_BASE_URL", "")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Return Address Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	if _, err := tenantstripe.NewStore(db).Upsert(callerCtx(tenantID, false), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_return_address", PublishableKey: "pk_test_return_address",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	stripe := installStripeStub(t)

	customerID := flowStr(flowDo(t, ts, "POST", "/v1/customers", flowReq{bearer: key, body: map[string]any{
		"external_id": "return-address", "display_name": "Return Address Customer", "email": "ap@return-address.test",
	}}).want(t, 201, "create customer").body, "id")
	invoiceID := flowStr(flowDo(t, ts, "POST", "/v1/invoices", flowReq{bearer: key, body: map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Seats", "quantity": 1, "unit_amount_cents": 8800}},
	}}).want(t, 201, "create invoice").body, "id")
	inv := flowDo(t, ts, "POST", "/v1/invoices/"+invoiceID+"/finalize", flowReq{bearer: key}).want(t, 200, "finalize").body

	// No "add a payment method" email: its page could not add the card. The
	// invoice is not stamped as notified, so the email goes out once the
	// setting is fixed, and whoami says setup links can't be emailed.
	if got := flowStr(inv, "no_pm_notified_at"); got != "" {
		t.Errorf("no_pm_notified_at = %q, want unset: no setup link can work", got)
	}
	for _, e := range flowList(flowDo(t, ts, "GET", "/v1/customers/"+customerID+"/sent-emails", flowReq{bearer: key}).want(t, 200, "sent emails").body, "data") {
		if flowStr(e, "email_type") == "payment_setup_request" {
			t.Errorf("a setup-link email was sent: %v", e)
		}
	}
	who := flowDo(t, ts, "GET", "/v1/whoami", flowReq{bearer: key}).want(t, 200, "whoami").body
	if v, ok := who["can_email_setup_links"].(bool); !ok || v {
		t.Errorf("whoami can_email_setup_links = %v, want false", who["can_email_setup_links"])
	}

	// The hosted invoice page offers no Pay button and says why; a direct
	// POST gets the same answer.
	publicToken := flowStr(inv, "public_token")
	if publicToken == "" {
		t.Fatalf("finalized invoice has no public_token: %v", inv)
	}
	page := flowDo(t, ts, "GET", "/v1/public/invoices/"+publicToken, flowReq{}).want(t, 200, "hosted page").body
	if v, _ := page["pay_enabled"].(bool); v || flowStr(page, "pay_unavailable_reason") != "online_payment_unavailable" {
		t.Errorf("hosted page pay_enabled=%v reason=%q, want false and online_payment_unavailable", page["pay_enabled"], flowStr(page, "pay_unavailable_reason"))
	}
	if res := flowDo(t, ts, "POST", "/v1/public/invoices/"+publicToken+"/checkout", flowReq{}).want(t, 409, "hosted checkout"); res.errCode() != "online_payment_unavailable" {
		t.Errorf("hosted checkout code %q, want online_payment_unavailable", res.errCode())
	}

	// The operator's setup links are refused with a code that names the
	// cause. A setup session with the caller's own return_url still works
	// without CUSTOMER_PORTAL_URL, so that is not tried here.
	if res := flowDo(t, ts, "POST", "/v1/customers/"+customerID+"/payment-methods/setup-session", flowReq{bearer: key, body: map[string]any{}}).want(t, 409, "setup session with no return_url"); res.errCode() != "return_address_not_set" {
		t.Errorf("setup-session code %q, want return_address_not_set", res.errCode())
	}
	if res := flowDo(t, ts, "POST", "/v1/customers/"+customerID+"/payment-methods/send-setup-email", flowReq{bearer: key, body: map[string]any{}}).want(t, 409, "send setup email"); res.errCode() != "return_address_not_set" {
		t.Errorf("send-setup-email code %q, want return_address_not_set", res.errCode())
	}

	// An add-a-card link minted before the setting was removed: the page
	// still opens, the button is refused, and the link is not used up.
	linkCtx := postgres.WithLivemode(context.Background(), false)
	token, err := payment.NewTokenService(db).Create(linkCtx, tenantID, customerID, invoiceID)
	if err != nil {
		t.Fatalf("mint payment-update token: %v", err)
	}
	if res := flowDo(t, ts, "POST", "/v1/public/payment-updates/"+token+"/checkout", flowReq{}).want(t, 409, "add-a-card checkout"); res.errCode() != "online_payment_unavailable" {
		t.Errorf("add-a-card checkout code %q, want online_payment_unavailable", res.errCode())
	}
	flowDo(t, ts, "GET", "/v1/public/payment-updates/"+token, flowReq{}).want(t, 200, "open the link after the refused checkout")

	// None of it reached Stripe.
	for _, call := range []string{"POST /v1/customers", "POST /v1/checkout/sessions"} {
		if n := stripe.count(call); n != 0 {
			t.Errorf("Stripe %s calls: %d, want 0", call, n)
		}
	}
	if u := stripe.unexpected(); len(u) != 0 {
		t.Errorf("unexpected Stripe calls: %v", u)
	}
}
