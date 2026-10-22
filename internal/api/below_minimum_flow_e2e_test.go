package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Stripe's answers to a charge below its minimum, as Stripe sends them
// (seen on a test-mode account, 2026-10-22): a 400 invalid_request_error
// with code amount_too_small. No PaymentIntent or Checkout Session is made.
const (
	minChargePIRefusal       = `{"error":{"type":"invalid_request_error","code":"amount_too_small","param":"amount","message":"Amount must be at least $0.50 usd"}}`
	minChargeCheckoutRefusal = `{"error":{"type":"invalid_request_error","code":"amount_too_small","message":"The Checkout Session's total amount due must add up to at least $0.50 usd"}}`
)

// minChargeStripe stands in for api.stripe.com. It saves cards (customer,
// setup session, payment method) and refuses every charge and every
// payment-mode Checkout Session as below the minimum.
type minChargeStripe struct {
	mu    sync.Mutex
	calls []string
	seq   int
}

func installMinChargeStripe(t *testing.T) *minChargeStripe {
	t.Helper()
	s := &minChargeStripe{}
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.stripe.com" {
			return prev.RoundTrip(req)
		}
		return s.answer(req), nil
	})
	t.Cleanup(func() { http.DefaultTransport = prev })
	return s
}

func (s *minChargeStripe) answer(req *http.Request) *http.Response {
	form := url.Values{}
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		form, _ = url.ParseQuery(string(b))
	}
	call := req.Method + " " + req.URL.Path
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.seq++
	n := s.seq
	s.mu.Unlock()

	status, body := 200, ""
	switch {
	case call == "POST /v1/customers":
		body = fmt.Sprintf(`{"id":"cus_min_%d","object":"customer"}`, n)
	case req.Method == "POST" && strings.HasPrefix(req.URL.Path, "/v1/customers/"):
		body = fmt.Sprintf(`{"id":%q,"object":"customer"}`, strings.TrimPrefix(req.URL.Path, "/v1/customers/"))
	case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/v1/payment_methods/"):
		id := strings.TrimPrefix(req.URL.Path, "/v1/payment_methods/")
		body = fmt.Sprintf(`{"id":%q,"object":"payment_method","type":"card","card":{"brand":"visa","display_brand":"visa","last4":"4242","exp_month":12,"exp_year":2034,"fingerprint":"fp_%s"}}`, id, id)
	case call == "POST /v1/checkout/sessions" && form.Get("mode") == "payment":
		status, body = 400, minChargeCheckoutRefusal
	case call == "POST /v1/checkout/sessions":
		body = fmt.Sprintf(`{"id":"cs_min_%d","object":"checkout.session","mode":"setup","url":"https://checkout.stripe.test/c/cs_min_%d"}`, n, n)
	case call == "POST /v1/payment_intents":
		status, body = 400, minChargePIRefusal
	default:
		status = 404
		body = fmt.Sprintf(`{"error":{"type":"invalid_request_error","message":"stub has no answer for %s"}}`, call)
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// count is how many calls Stripe received for "METHOD path" (a payment-mode
// and a setup-mode Checkout Session are both POST /v1/checkout/sessions).
func (s *minChargeStripe) count(call string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == call {
			n++
		}
	}
	return n
}

// TestE2E_BelowStripeMinimum_NotADecline takes invoices below Stripe's
// minimum charge ($0.30 USD) through the real router and database, with
// Stripe answering as it does (400 amount_too_small):
//
//   - finalizing charges once; the refusal is not a decline: the invoice stays
//     open and unpaid, the auto-charge queue drops it, no payment recovery
//     starts, the customer gets no email, and the notice tells the operator
//     what to do;
//   - Collect payment gets the same answer and changes nothing;
//   - the hosted page offers no Pay button, and a Pay click Stripe refuses
//     frees the payment page at once instead of holding charges for an hour;
//   - a payment recovery already running ends when its charge gets this
//     answer, without a warning email to the customer.
func TestE2E_BelowStripeMinimum_NotADecline(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// A configured server queues customer emails, so an email Velox wrongly
	// sends shows up in the outbox.
	t.Setenv("SMTP_HOST", "smtp.min-charge.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Min Charge Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
		"name": "Min charge policy", "enabled": true, "max_retry_attempts": 2, "retry_schedule": []string{"72h"},
		"grace_period_days": 1,
	}}).want(t, 201, "create dunning policy")

	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_min_charge", PublishableKey: "pk_test_min_charge",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	const signingSecret = "whsec_min_charge"
	creds := call("PATCH", "/v1/settings/stripe/test/webhook", flowReq{body: map[string]any{"webhook_secret": signingSecret}}).want(t, 200, "set webhook secret").body
	endpoint := "/v1/webhooks/stripe/" + flowStr(creds, "id")
	stripe := installMinChargeStripe(t)

	newCustomer := func(name, clockID string) string {
		t.Helper()
		body := map[string]any{"external_id": "min-" + name, "display_name": "Min " + name, "email": name + "@min-charge.test"}
		if clockID != "" {
			body["test_clock_id"] = clockID
		}
		return flowStr(call("POST", "/v1/customers", flowReq{body: body}).want(t, 201, name+": create customer").body, "id")
	}
	// saveCard runs the operator's Add payment method link and Stripe's
	// setup_intent.succeeded for the card the customer saved on it.
	saveCard := func(customerID, pm string) {
		t.Helper()
		call("POST", "/v1/customers/"+customerID+"/payment-methods/setup-session", flowReq{body: map[string]any{}}).want(t, 201, "setup session")
		payload, sig := signedStripeEvent(t, signingSecret, map[string]any{
			"id": "evt_setup_" + pm, "object": "event", "type": "setup_intent.succeeded",
			"created": time.Now().Unix(), "livemode": false,
			"data": map[string]any{"object": map[string]any{
				"id": "seti_" + pm, "object": "setup_intent", "status": "succeeded", "payment_method": pm,
				"metadata": map[string]any{"velox_customer_id": customerID},
			}},
		})
		flowDo(t, ts, "POST", endpoint, flowReq{header: map[string]string{"Stripe-Signature": sig}, body: payload}).want(t, 200, "setup_intent.succeeded")
	}
	newInvoice := func(customerID, desc string) string {
		t.Helper()
		id := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": desc, "quantity": 1, "unit_amount_cents": 30}},
		}}).want(t, 201, "create invoice: "+desc).body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, "finalize: "+desc)
		return id
	}
	getInvoice := func(id string) map[string]any {
		t.Helper()
		return flowObj(call("GET", "/v1/invoices/"+id, flowReq{}).want(t, 200, "get invoice").body, "invoice")
	}
	runsOf := func(invoiceID string) []map[string]any {
		t.Helper()
		return flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(t, 200, "list dunning runs").body, "data")
	}
	customerEmails := func(invoiceID string) int {
		t.Helper()
		return flowCount(t, db, `SELECT count(*) FROM email_outbox
			WHERE tenant_id = $1 AND email_type IN ('payment_failed', 'dunning_warning', 'dunning_escalation')
			  AND payload->>'invoice_number' = (SELECT invoice_number FROM invoices WHERE id = $2)`, tenantID, invoiceID)
	}
	actionCodes := func(att map[string]any) []string {
		var out []string
		for _, a := range flowList(att, "actions") {
			out = append(out, flowStr(a, "code"))
		}
		return out
	}
	// belowMinimum checks the invoice reads as open and unpaid, out of the
	// auto-charge queue, with Stripe's sentence and the operator's notice.
	belowMinimum := func(what string, inv map[string]any) {
		t.Helper()
		if flowStr(inv, "status") != "finalized" || flowStr(inv, "payment_status") != "pending" {
			t.Errorf("%s: status=%v payment_status=%v, want finalized/pending (not a failed payment)", what, inv["status"], inv["payment_status"])
		}
		if inv["auto_charge_pending"] == true {
			t.Errorf("%s: still queued for the auto-charge sweep", what)
		}
		if !strings.Contains(flowStr(inv, "last_payment_error"), "at least $0.50") {
			t.Errorf("%s: last_payment_error %q, want Stripe's sentence", what, flowStr(inv, "last_payment_error"))
		}
		att := flowObj(inv, "attention")
		if flowStr(att, "reason") != "amount_below_minimum" || flowStr(att, "code") != "payment.amount_too_small" {
			t.Errorf("%s: attention %v, want reason amount_below_minimum, code payment.amount_too_small", what, att)
		}
		if codes := actionCodes(att); len(codes) != 1 || codes[0] != "void_invoice" {
			t.Errorf("%s: attention actions %v, want only void_invoice", what, codes)
		}
		if !strings.Contains(flowStr(att, "provider_response"), "at least $0.50") {
			t.Errorf("%s: provider_response %q, want Stripe's sentence", what, flowStr(att, "provider_response"))
		}
	}

	// A: a customer with a card. Finalizing charges once, and Stripe refuses.
	custA := newCustomer("a", "")
	saveCard(custA, "pm_min_a")
	invA := newInvoice(custA, "Quiet month")
	if n := stripe.count("POST /v1/payment_intents"); n != 1 {
		t.Fatalf("charges sent at finalize: %d, want 1", n)
	}
	belowMinimum("after finalize", getInvoice(invA))
	if runs := runsOf(invA); len(runs) != 0 {
		t.Errorf("payment recovery runs on invoice A: %v, want none", runs)
	}
	if n := customerEmails(invA); n != 0 {
		t.Errorf("payment-failure emails for invoice A: %d, want none", n)
	}

	// Collect payment asks Stripe again and gets the same answer: a 409 the
	// dashboard can name, and nothing else changes.
	collect := call("POST", "/v1/invoices/"+invA+"/collect", flowReq{})
	if collect.status != 409 || collect.errCode() != "amount_below_minimum" {
		t.Errorf("collect: status %d code %q, want 409 amount_below_minimum; body %s", collect.status, collect.errCode(), collect.raw)
	}
	if n := stripe.count("POST /v1/payment_intents"); n != 2 {
		t.Errorf("charges sent after Collect: %d, want 2", n)
	}
	belowMinimum("after collect", getInvoice(invA))
	if runs := runsOf(invA); len(runs) != 0 {
		t.Errorf("payment recovery runs on invoice A after Collect: %v, want none", runs)
	}

	// The hosted page has no Pay button for it, and a direct POST is refused
	// before Stripe is asked.
	tokenA := flowStr(getInvoice(invA), "public_token")
	if tokenA == "" {
		t.Fatal("invoice A has no public token")
	}
	page := flowDo(t, ts, "GET", "/v1/public/invoices/"+tokenA, flowReq{}).want(t, 200, "hosted page A").body
	if page["pay_enabled"] != false || flowStr(page, "pay_unavailable_reason") != "amount_below_minimum" {
		t.Errorf("hosted page A: pay_enabled=%v reason=%q, want false and amount_below_minimum", page["pay_enabled"], flowStr(page, "pay_unavailable_reason"))
	}
	sessionsBefore := stripe.count("POST /v1/checkout/sessions")
	pay := flowDo(t, ts, "POST", "/v1/public/invoices/"+tokenA+"/checkout", flowReq{})
	if pay.status != 409 || pay.errCode() != "amount_below_minimum" {
		t.Errorf("hosted Pay on A: status %d code %q, want 409 amount_below_minimum", pay.status, pay.errCode())
	}
	if n := stripe.count("POST /v1/checkout/sessions"); n != sessionsBefore {
		t.Errorf("hosted Pay on A asked Stripe for a session (%d calls, was %d)", n, sessionsBefore)
	}

	// B: a customer with no card. Velox has never charged the invoice, so
	// only the hosted Pay click learns the amount is too small. Stripe
	// refuses the session; the page says why, and the claim is released, so
	// nothing holds charges on the invoice and a second click asks again.
	custB := newCustomer("b", "")
	invB := newInvoice(custB, "One-off")
	tokenB := flowStr(getInvoice(invB), "public_token")
	for i := 1; i <= 2; i++ {
		res := flowDo(t, ts, "POST", "/v1/public/invoices/"+tokenB+"/checkout", flowReq{})
		if res.status != 409 || res.errCode() != "amount_below_minimum" {
			t.Errorf("hosted Pay on B, click %d: status %d code %q, want 409 amount_below_minimum; body %s", i, res.status, res.errCode(), res.raw)
		}
		if n := flowCount(t, db, `SELECT count(*) FROM checkout_sessions WHERE invoice_id = $1 AND status = 'open'`, invB); n != 0 {
			t.Errorf("click %d: %d open payment-page claims on invoice B, want 0", i, n)
		}
		if code := flowStr(getInvoice(invB), "attention", "code"); code == "payment.page_open" {
			t.Errorf("click %d: invoice B reads as having its payment page open", i)
		}
	}
	if n := stripe.count("POST /v1/checkout/sessions") - sessionsBefore; n != 2 {
		t.Errorf("payment-mode sessions asked of Stripe for B: %d, want 2 (one per click)", n)
	}

	// C: payment recovery already running. The customer on a test clock had
	// no card at finalize, so recovery started; they then save a card, and
	// the next charge on the advance gets Stripe's answer. Recovery ends
	// there with no warning email, and the invoice reads as below the
	// minimum.
	frozen := time.Date(2027, 3, 1, 9, 0, 0, 0, time.UTC)
	clockID := flowStr(call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": "min charge", "frozen_time": frozen.Format(time.RFC3339),
	}}).want(t, 201, "create test clock").body, "id")
	custC := newCustomer("c", clockID)
	invC := newInvoice(custC, "Clocked quiet month")
	if runs := runsOf(invC); len(runs) != 1 || flowStr(runs[0], "state") != "active" {
		t.Fatalf("invoice C runs after a card-less finalize: %v, want one active run", runs)
	}
	saveCard(custC, "pm_min_c")
	charges := stripe.count("POST /v1/payment_intents")
	call("POST", "/v1/test-clocks/"+clockID+"/advance", flowReq{body: map[string]any{
		"frozen_time": frozen.Add(5 * 24 * time.Hour).Format(time.RFC3339),
	}}).want(t, 200, "advance clock")
	if got := stripe.count("POST /v1/payment_intents") - charges; got != 1 {
		t.Errorf("charges sent on the advance: %d, want 1 (the refusal ends recovery)", got)
	}
	runs := runsOf(invC)
	if len(runs) != 1 || flowStr(runs[0], "state") != "resolved" || flowStr(runs[0], "resolution") != "amount_below_minimum" {
		t.Errorf("invoice C run after the advance: %v, want resolved with amount_below_minimum", runs)
	}
	belowMinimum("invoice C after the advance", getInvoice(invC))
	if n := customerEmails(invC); n != 0 {
		t.Errorf("payment-failure emails for invoice C: %d, want none", n)
	}
}
