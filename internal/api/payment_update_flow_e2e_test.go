package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_PaymentUpdateLink walks the customer's update-payment link over
// the real router and database. An invoice finalizes with no card on file,
// so the customer is emailed a single-use link. The page reads the invoice
// through GET /v1/public/payment-updates/{token}, and its button opens a
// Stripe Checkout session through POST .../{token}/checkout. Neither public
// endpoint had a handler test; only the token store was tested.
func TestE2E_PaymentUpdateLink(t *testing.T) {
	db := testutil.SetupTestDB(t)
	const returnURL = "https://billing.update-flow.test/payment-method-added"
	t.Setenv("PAYMENT_UPDATE_URL", "https://billing.update-flow.test/update-payment")
	t.Setenv("PAYMENT_UPDATE_RETURN_URL", returnURL)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Update Link Flow Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)

	const customerName, customerEmail = "Update Link Customer", "ap@update-flow.test"
	customerID := flowStr(flowDo(t, ts, "POST", "/v1/customers", flowReq{bearer: key, body: map[string]any{
		"external_id": "update-link-flow", "display_name": customerName, "email": customerEmail,
	}}).want(t, 201, "create customer").body, "id")
	invoiceID := flowStr(flowDo(t, ts, "POST", "/v1/invoices", flowReq{bearer: key, body: map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Seats", "quantity": 1, "unit_amount_cents": 8800}},
	}}).want(t, 201, "create invoice").body, "id")
	inv := flowDo(t, ts, "POST", "/v1/invoices/"+invoiceID+"/finalize", flowReq{bearer: key}).want(t, 200, "finalize").body
	if flowStr(inv, "status") != "finalized" || flowStr(inv, "no_pm_notified_at") == "" {
		t.Fatalf("finalized invoice: %v, want finalized with the setup-link email sent (no_pm_notified_at)", inv)
	}

	// The customer's sent emails show the setup-link email for this invoice.
	sent := flowList(flowDo(t, ts, "GET", "/v1/customers/"+customerID+"/sent-emails", flowReq{bearer: key}).want(t, 200, "sent emails").body, "sent_emails")
	if len(sent) != 1 || flowStr(sent[0], "email_type") != "payment_setup_request" ||
		flowStr(sent[0], "recipient") != customerEmail || flowStr(sent[0], "invoice_number") != flowStr(inv, "invoice_number") {
		t.Errorf("sent emails = %v, want one payment_setup_request to %s for %s", sent, customerEmail, flowStr(inv, "invoice_number"))
	}

	token := flowEmailLinkToken(t, db, "payment_setup_request", "update_url")
	page := func() flowResp {
		t.Helper()
		return flowDo(t, ts, "GET", "/v1/public/payment-updates/"+token, flowReq{})
	}
	checkout := func() flowResp {
		t.Helper()
		return flowDo(t, ts, "POST", "/v1/public/payment-updates/"+token+"/checkout", flowReq{})
	}

	// The page shows the invoice the API holds.
	shown := page().want(t, 200, "open the link").body
	if flowStr(shown, "customer_name") != customerName || flowStr(shown, "invoice_number") != flowStr(inv, "invoice_number") ||
		flowStr(shown, "invoice_status") != "finalized" || flowInt(shown, "amount_due_cents") != flowInt(inv, "amount_due_cents") ||
		flowStr(shown, "currency") != flowStr(inv, "currency") {
		t.Errorf("page = %v, want %s, invoice %s, finalized, %d %s due", shown, customerName,
			flowStr(inv, "invoice_number"), flowInt(inv, "amount_due_cents"), flowStr(inv, "currency"))
	}
	if res := flowDo(t, ts, "GET", "/v1/public/payment-updates/vlx_pt_not_a_real_link", flowReq{}).want(t, 401, "open a made-up link"); res.errCode() != "invalid_token" {
		t.Errorf("made-up link error code %q, want invalid_token", res.errCode())
	}

	// Before Stripe is connected the button fails, and the link still works.
	if res := checkout().want(t, 502, "checkout before Stripe is connected"); res.errCode() != "stripe_error" {
		t.Errorf("checkout without Stripe: code %q, want stripe_error", res.errCode())
	}
	page().want(t, 200, "open the link after the failed checkout")

	// With Stripe connected, the button opens a Checkout session in setup
	// mode for this customer, returning to the payment-method-added page.
	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_update_flow", PublishableKey: "pk_test_update_flow",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	stripe := installStripeStub(t)
	opened := checkout().want(t, 201, "checkout")
	if got := flowStr(opened.body, "url"); got != "https://checkout.stripe.test/c/cs_flow" {
		t.Errorf("checkout url %q, want the session's url", got)
	}
	if n := stripe.count("POST /v1/customers"); n != 1 {
		t.Errorf("Stripe customers created: %d, want 1 (the customer had none)", n)
	}
	form := stripe.form("POST /v1/checkout/sessions")
	for field, want := range map[string]string{
		"mode":        "setup",
		"customer":    "cus_flow",
		"success_url": returnURL + "?status=success",
		"cancel_url":  returnURL + "?status=cancel",
		"setup_intent_data[metadata][velox_customer_id]": customerID,
		"setup_intent_data[metadata][velox_invoice_id]":  invoiceID,
		"setup_intent_data[metadata][velox_purpose]":     "payment_update_token",
	} {
		if got := form.Get(field); got != want {
			t.Errorf("checkout session %s = %q, want %q", field, got, want)
		}
	}

	// The link is single-use: spent now, for the page and the button.
	if res := page().want(t, 401, "open a used link"); res.errCode() != "invalid_token" {
		t.Errorf("used link error code %q, want invalid_token", res.errCode())
	}
	checkout().want(t, 401, "checkout on a used link")
	if n := stripe.count("POST /v1/checkout/sessions"); n != 1 {
		t.Errorf("Checkout sessions created: %d, want 1", n)
	}
	if u := stripe.unexpected(); len(u) != 0 {
		t.Errorf("unexpected Stripe calls: %v", u)
	}
}
