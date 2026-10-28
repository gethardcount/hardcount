package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Stripe's answers to a charge below its minimum, as Stripe sends them
// (seen on a test-mode account, 2026-10-22): a 400 invalid_request_error
// with code amount_too_small. No PaymentIntent or Checkout Session is made.
const (
	minChargePIRefusal       = `{"error":{"type":"invalid_request_error","code":"amount_too_small","param":"amount","message":"Amount must be at least $0.50 usd"}}`
	minChargeCheckoutRefusal = `{"error":{"type":"invalid_request_error","code":"amount_too_small","message":"The Checkout Session's total amount due must add up to at least $0.50 usd"}}`
	// A charge at or above the minimum is declined, so a payment recovery
	// starts the ordinary way.
	minChargeDecline = `{"error":{"type":"card_error","code":"card_declined","decline_code":"generic_decline","message":"Your card was declined.","payment_intent":{"id":"pi_min_declined_%d","object":"payment_intent","status":"requires_payment_method"}}}`
)

// minChargeStripe stands in for api.stripe.com. It saves cards (customer,
// setup session, payment method), refuses every charge under its minimum (50
// cents unless setMinimum changes it) and every payment-mode Checkout
// Session as below the minimum, and declines a charge at or above it.
type minChargeStripe struct {
	mu      sync.Mutex
	calls   []string
	seq     int
	minimum int
}

// setMinimum changes the minimum Stripe applies, as a change to the
// account's settlement currency does for a converted charge.
func (s *minChargeStripe) setMinimum(cents int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.minimum = cents
}

func installMinChargeStripe(t *testing.T) *minChargeStripe {
	t.Helper()
	s := &minChargeStripe{minimum: 50}
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
	minimum := s.minimum
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
		if amount, _ := strconv.Atoi(form.Get("amount")); amount >= minimum {
			status, body = 402, fmt.Sprintf(minChargeDecline, n)
		} else {
			status, body = 400, minChargePIRefusal
		}
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
//   - Collect payment asks Stripe again (Stripe answers at once, nothing is
//     charged) and answers with the refusal, and pays the invoice from a
//     credit balance that covers it;
//   - the payment timeline shows each refusal as a warning, not as a failed
//     payment;
//   - the hosted page offers no Pay button; for a customer with no card a
//     Pay click is the first charge, and Stripe's refusal of it is recorded
//     like a charge's: the payment page is freed at once instead of holding
//     charges for an hour, and the no-card payment recovery ends (B);
//   - a payment recovery already running ends when its charge gets this
//     answer, without a warning email to the customer: started with no card
//     and ended by the auto-charge sweep once a card is saved (C), or
//     started by a decline and ended by its own retry once credits bring the
//     amount below the minimum (D);
//   - a run on an invoice stamped below the minimum ends at its next tick
//     even when the resolve that should have ended it was lost (E);
//   - the stamp is Stripe's latest answer, not a permanent fact: once Stripe
//     would take the amount, Collect asks it again and records the new
//     answer, and the hosted page offers Pay again (F).
func TestE2E_BelowStripeMinimum_NotADecline(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// A configured server queues customer emails, so an email Hardcount wrongly
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
		"name": "Min charge policy", "enabled": true, "retry_days": []int{1, 3},
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
				"metadata": map[string]any{"hardcount_customer_id": customerID},
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

	// Collect payment with no credit to apply asks Stripe again (its minimum
	// for a converted charge can change) and answers with the refusal, a 409
	// the dashboard can name.
	collect := call("POST", "/v1/invoices/"+invA+"/collect", flowReq{})
	if collect.status != 409 || collect.errCode() != "amount_below_minimum" {
		t.Errorf("collect: status %d code %q, want 409 amount_below_minimum; body %s", collect.status, collect.errCode(), collect.raw)
	}
	if n := stripe.count("POST /v1/payment_intents"); n != 2 {
		t.Errorf("charges sent after Collect: %d, want 2 (Collect asks Stripe again)", n)
	}
	belowMinimum("after collect", getInvoice(invA))
	if runs := runsOf(invA); len(runs) != 0 {
		t.Errorf("payment recovery runs on invoice A after Collect: %v, want none", runs)
	}
	// The payment timeline shows each refusal (finalize, Collect) as a
	// warning that nothing was charged, never as a failed payment.
	lineKinds := func(id string) map[string][]map[string]any {
		t.Helper()
		out := map[string][]map[string]any{}
		for _, e := range flowList(call("GET", "/v1/invoices/"+id+"/payment-timeline", flowReq{}).want(t, 200, "payment timeline").body, "events") {
			l := flowObj(e, "line")
			out[flowStr(l, "kind")] = append(out[flowStr(l, "kind")], l)
		}
		return out
	}
	kindsA := lineKinds(invA)
	if rows := kindsA["payment.failed"]; len(rows) != 0 {
		t.Errorf("invoice A timeline shows %d 'Payment failed' rows, want none: %v", len(rows), rows)
	}
	if rows := kindsA["payment.below_minimum"]; len(rows) != 2 || flowStr(rows[0], "tone") != "warning" || flowStr(rows[1], "tone") != "warning" {
		t.Errorf("invoice A timeline below-minimum rows %v, want two warning rows", rows)
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

	// With a credit balance, Collect payment applies it, and a credit that
	// covers the amount pays the invoice with no charge.
	call("POST", "/v1/credits/grant", flowReq{body: map[string]any{
		"customer_id": custA, "currency": "USD", "amount_cents": 50, "description": "Goodwill",
	}}).want(t, 201, "grant credit A")
	call("POST", "/v1/invoices/"+invA+"/collect", flowReq{}).want(t, 200, "collect A with credit")
	if inv := getInvoice(invA); flowStr(inv, "status") != "paid" || flowInt(inv, "amount_due_cents") != 0 {
		t.Errorf("invoice A after Collect with credit: status %v amount due %v, want paid and 0", inv["status"], inv["amount_due_cents"])
	}
	if n := stripe.count("POST /v1/payment_intents"); n != 2 {
		t.Errorf("charges sent after Collect with credit: %d, want 2 (the credit paid it)", n)
	}

	// B: a customer with no card. Hardcount has never charged the invoice, so
	// only the hosted Pay click learns the amount is too small. Stripe
	// refuses the session, and Hardcount records the refusal as it does for a
	// charge: the claim is released, the invoice reads as below the minimum,
	// the page has no Pay button, and the payment recovery the card-less
	// finalize started ends, so no reminder or final action follows. A
	// second click is refused before Stripe is asked.
	custB := newCustomer("b", "")
	invB := newInvoice(custB, "One-off")
	if runs := runsOf(invB); len(runs) != 1 || flowStr(runs[0], "state") != "active" {
		t.Fatalf("invoice B runs after a card-less finalize: %v, want one active run", runs)
	}
	tokenB := flowStr(getInvoice(invB), "public_token")
	for i := 1; i <= 2; i++ {
		res := flowDo(t, ts, "POST", "/v1/public/invoices/"+tokenB+"/checkout", flowReq{})
		if res.status != 409 || res.errCode() != "amount_below_minimum" {
			t.Errorf("hosted Pay on B, click %d: status %d code %q, want 409 amount_below_minimum; body %s", i, res.status, res.errCode(), res.raw)
		}
		if n := flowCount(t, db, `SELECT count(*) FROM checkout_sessions WHERE invoice_id = $1 AND status = 'open'`, invB); n != 0 {
			t.Errorf("click %d: %d open payment-page claims on invoice B, want 0", i, n)
		}
		belowMinimum(fmt.Sprintf("invoice B after Pay click %d", i), getInvoice(invB))
	}
	if n := stripe.count("POST /v1/checkout/sessions") - sessionsBefore; n != 1 {
		t.Errorf("payment-mode sessions asked of Stripe for B: %d, want 1 (the second click is refused before Stripe)", n)
	}
	pageB := flowDo(t, ts, "GET", "/v1/public/invoices/"+tokenB, flowReq{}).want(t, 200, "hosted page B").body
	if pageB["pay_enabled"] != false || flowStr(pageB, "pay_unavailable_reason") != "amount_below_minimum" {
		t.Errorf("hosted page B: pay_enabled=%v reason=%q, want false and amount_below_minimum", pageB["pay_enabled"], flowStr(pageB, "pay_unavailable_reason"))
	}
	if runs := runsOf(invB); len(runs) != 1 || flowStr(runs[0], "state") != "resolved" || flowStr(runs[0], "resolution") != "amount_below_minimum" {
		t.Errorf("invoice B run after the refused Pay click: %v, want resolved with amount_below_minimum", runs)
	}
	if n := customerEmails(invB); n != 0 {
		t.Errorf("payment-failure emails for invoice B: %d, want none", n)
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

	// D: an $0.80 invoice is declined at finalize, so recovery starts and
	// the invoice leaves the auto-charge queue. A $0.50 credit then leaves
	// $0.30, which the first retry applies before it charges: Stripe refuses
	// the charge, and the retry ends recovery at its own instant.
	custD := newCustomer("d", clockID)
	saveCard(custD, "pm_min_d")
	idD := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
		"customer_id": custD,
		"line_items":  []map[string]any{{"description": "Clocked usage", "quantity": 1, "unit_amount_cents": 80}},
	}}).want(t, 201, "create invoice D").body, "id")
	call("POST", "/v1/invoices/"+idD+"/finalize", flowReq{}).want(t, 200, "finalize D")
	if inv := getInvoice(idD); flowStr(inv, "payment_status") != "failed" {
		t.Fatalf("invoice D after the declined finalize: payment_status %v, want failed", inv["payment_status"])
	}
	runs = runsOf(idD)
	if len(runs) != 1 || flowStr(runs[0], "state") != "active" {
		t.Fatalf("invoice D runs after the decline: %v, want one active run", runs)
	}
	call("POST", "/v1/credits/grant", flowReq{body: map[string]any{
		"customer_id": custD, "currency": "USD", "amount_cents": 50, "description": "Goodwill",
	}}).want(t, 201, "grant credit D")
	warningsBefore := flowCount(t, db, `SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = 'dunning_warning'`, tenantID)
	charges = stripe.count("POST /v1/payment_intents")
	call("POST", "/v1/test-clocks/"+clockID+"/advance", flowReq{body: map[string]any{
		"frozen_time": frozen.Add(10 * 24 * time.Hour).Format(time.RFC3339),
	}}).want(t, 200, "advance clock for D")
	if got := stripe.count("POST /v1/payment_intents") - charges; got != 1 {
		t.Errorf("charges sent on D's advance: %d, want 1 (the first retry; the refusal ends recovery)", got)
	}
	runs = runsOf(idD)
	if len(runs) != 1 || flowStr(runs[0], "state") != "resolved" || flowStr(runs[0], "resolution") != "amount_below_minimum" {
		t.Errorf("invoice D run after the advance: %v, want resolved with amount_below_minimum", runs)
	}
	if len(runs) == 1 && flowInt(runs[0], "attempt_count") != 1 {
		t.Errorf("invoice D run attempt_count %v, want 1", runs[0]["attempt_count"])
	}
	invD := getInvoice(idD)
	belowMinimum("invoice D after the advance", invD)
	if flowInt(invD, "amount_due_cents") != 30 {
		t.Errorf("invoice D amount due %d, want 30 after the credit", flowInt(invD, "amount_due_cents"))
	}
	if n := flowCount(t, db, `SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = 'dunning_warning'`, tenantID) - warningsBefore; n != 0 {
		t.Errorf("dunning warning emails on D's advance: %d, want none", n)
	}
	// The retry's own row tells the refusal; the charge is not shown again
	// as a failed payment beside it.
	kindsD := lineKinds(idD)
	if rows := kindsD["payment_recovery.retry_refused"]; len(rows) != 1 {
		t.Errorf("invoice D timeline retry-refused rows %v, want one", rows)
	}
	if rows := kindsD["payment.below_minimum"]; len(rows) != 0 {
		t.Errorf("invoice D timeline shows the retry's refusal again as its own row: %v", rows)
	}
	for _, l := range kindsD["payment.failed"] {
		if strings.Contains(flowStr(l, "problem"), "at least $0.50") {
			t.Errorf("invoice D timeline shows the below-minimum refusal as a failed payment: %v", l)
		}
	}
	// E: the resolve that should end a run is lost. A card-less customer's
	// $0.30 invoice has its no-card run; Stripe's refusal of a Pay click is
	// stamped on the invoice by the real writer (as RecordCheckoutBelowMinimum
	// writes it), and the resolve that follows is dropped (a DB blip, the tab
	// closed, a restart). A card-less retry never asks Stripe, so only the
	// stamp can end the run: at its next tick, with no reminder and no final
	// action.
	clockE := flowStr(call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": "min charge lost resolve", "frozen_time": frozen.Format(time.RFC3339),
	}}).want(t, 201, "create test clock E").body, "id")
	custE := newCustomer("e", clockE)
	idE := newInvoice(custE, "Clocked one-off")
	if runs := runsOf(idE); len(runs) != 1 || flowStr(runs[0], "state") != "active" {
		t.Fatalf("invoice E runs after a card-less finalize: %v, want one active run", runs)
	}
	storeCtx := postgres.WithLivemode(context.Background(), false)
	invStore := invoice.NewPostgresStore(db)
	rowE, err := invStore.Get(storeCtx, tenantID, idE)
	if err != nil {
		t.Fatalf("read invoice E: %v", err)
	}
	if _, applied, err := invStore.StampBelowMinimum(storeCtx, tenantID, idE, rowE.ChargeAttemptSeq,
		"The Checkout Session's total amount due must add up to at least $0.50 usd"); err != nil || !applied {
		t.Fatalf("stamp invoice E: applied=%v err=%v", applied, err)
	}
	emailsE := customerEmails(idE)
	call("POST", "/v1/test-clocks/"+clockE+"/advance", flowReq{body: map[string]any{
		"frozen_time": frozen.Add(30 * 24 * time.Hour).Format(time.RFC3339),
	}}).want(t, 200, "advance clock E")
	runs = runsOf(idE)
	if len(runs) != 1 || flowStr(runs[0], "state") != "resolved" || flowStr(runs[0], "resolution") != "amount_below_minimum" {
		t.Errorf("invoice E run after the advance: %v, want resolved with amount_below_minimum", runs)
	}
	if n := customerEmails(idE) - emailsE; n != 0 {
		t.Errorf("dunning emails for invoice E after the stamp: %d, want none", n)
	}

	// F: Stripe's answer changes. A $0.30 invoice is refused at finalize;
	// then Stripe's minimum for it drops (the account now settles in the
	// charge's currency, so no conversion). Collect asks Stripe again rather
	// than answering from the stamp, Stripe's new answer (here a decline) is
	// recorded, and the invoice no longer reads as below the minimum: the
	// notice is the decline's and the hosted page offers Pay.
	custF := newCustomer("f", "")
	saveCard(custF, "pm_min_f")
	idF := newInvoice(custF, "Converted month")
	belowMinimum("invoice F after finalize", getInvoice(idF))
	stripe.setMinimum(25)
	charges = stripe.count("POST /v1/payment_intents")
	collectF := call("POST", "/v1/invoices/"+idF+"/collect", flowReq{})
	if got := stripe.count("POST /v1/payment_intents") - charges; got != 1 {
		t.Errorf("charges sent by Collect on F: %d, want 1 (Collect asks Stripe again)", got)
	}
	if collectF.errCode() == "amount_below_minimum" {
		t.Errorf("Collect on F answered from the stamp: status %d body %s", collectF.status, collectF.raw)
	}
	invF := getInvoice(idF)
	if flowStr(invF, "payment_status") != "failed" || flowStr(flowObj(invF, "attention"), "reason") == "amount_below_minimum" {
		t.Errorf("invoice F after the decline: payment_status %v attention %v, want failed and not below the minimum", invF["payment_status"], invF["attention"])
	}
	pageF := flowDo(t, ts, "GET", "/v1/public/invoices/"+flowStr(invF, "public_token"), flowReq{}).want(t, 200, "hosted page F").body
	if pageF["pay_enabled"] != true {
		t.Errorf("hosted page F: pay_enabled=%v reason=%q, want Pay offered again", pageF["pay_enabled"], flowStr(pageF, "pay_unavailable_reason"))
	}
}
