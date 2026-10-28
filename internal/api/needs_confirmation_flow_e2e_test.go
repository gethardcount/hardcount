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

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Stripe's answer to an off-session charge the customer's bank wants them to
// confirm (3-D Secure, test card 4000 0027 6000 3184): a 402 card_error
// whose code and decline code are both authentication_required, with the
// PaymentIntent left at requires_payment_method. The message is the
// developer text the Stripe sandbox walk of 2026-10-22 saw in Hardcount's
// activity (ux-review-2026-10-22-stripe.md, P4).
const (
	confirmDevText = "This payment requires authentication. If the customer is off session, ask them to return to your application and complete the payment."
	confirmRefusal = `{"error":{"type":"card_error","code":"authentication_required","decline_code":"authentication_required","message":%q,"payment_intent":{"id":"pi_3ds_%d","object":"payment_intent","status":"requires_payment_method"}}}`
	confirmDecline = `{"error":{"type":"card_error","code":"card_declined","decline_code":"generic_decline","message":"Your card was declined.","payment_intent":{"id":"pi_nsf_%d","object":"payment_intent","status":"requires_payment_method"}}}`
	confirmPaid    = `{"id":"pi_ok_%d","object":"payment_intent","status":"succeeded","amount":%d,"currency":"usd"}`
)

// confirmStripe stands in for api.stripe.com. It saves cards and answers each
// off-session charge by the card it is asked to charge: a pm_3ds_ card needs
// the customer to confirm, a pm_nsf_ card is declined, and any other card is
// charged.
type confirmStripe struct {
	mu      sync.Mutex
	calls   []string
	charged []string // payment_method of every charge Stripe received
	seq     int
}

func installConfirmStripe(t *testing.T) *confirmStripe {
	t.Helper()
	s := &confirmStripe{}
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

func (s *confirmStripe) answer(req *http.Request) *http.Response {
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
	if call == "POST /v1/payment_intents" {
		s.charged = append(s.charged, form.Get("payment_method"))
	}
	s.mu.Unlock()

	status, body := 200, ""
	switch {
	case call == "POST /v1/customers":
		body = fmt.Sprintf(`{"id":"cus_3ds_%d","object":"customer"}`, n)
	case req.Method == "POST" && strings.HasPrefix(req.URL.Path, "/v1/customers/"):
		body = fmt.Sprintf(`{"id":%q,"object":"customer"}`, strings.TrimPrefix(req.URL.Path, "/v1/customers/"))
	case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/v1/payment_methods/"):
		id := strings.TrimPrefix(req.URL.Path, "/v1/payment_methods/")
		body = fmt.Sprintf(`{"id":%q,"object":"payment_method","type":"card","card":{"brand":"visa","display_brand":"visa","last4":"3184","exp_month":12,"exp_year":2034,"fingerprint":"fp_%s"}}`, id, id)
	case call == "POST /v1/checkout/sessions":
		body = fmt.Sprintf(`{"id":"cs_3ds_%d","object":"checkout.session","mode":"setup","url":"https://checkout.stripe.test/c/cs_3ds_%d"}`, n, n)
	case call == "POST /v1/payment_intents":
		pm := form.Get("payment_method")
		amount, _ := strconv.Atoi(form.Get("amount"))
		switch {
		case strings.HasPrefix(pm, "pm_3ds_"):
			status, body = 402, fmt.Sprintf(confirmRefusal, confirmDevText, n)
		case strings.HasPrefix(pm, "pm_nsf_"):
			status, body = 402, fmt.Sprintf(confirmDecline, n)
		default:
			body = fmt.Sprintf(confirmPaid, n, amount)
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

// chargedCards is every card Stripe was asked to charge, in order.
func (s *confirmStripe) chargedCards() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.charged...)
}

// TestE2E_OffSessionCharge_BankNeedsCustomerToConfirm takes an off-session
// charge that Stripe answers with authentication_required (the customer's
// bank wants them to confirm the payment with 3-D Secure) through the real
// router and database (P4 of the Stripe card-flow review, 2026-10-22):
//
//   - the operator's notice says the bank needs the customer to confirm the
//     payment and offers emailing the payment link, not Retry payment, which
//     charges off-session again;
//   - the activity says it in plain words, never Stripe's developer text, and
//     so does Collect payment's answer;
//   - the customer's email says their bank asked them to confirm the payment,
//     with the pay link, and says nothing about a decline;
//   - dunning follows Stripe Billing, which lists authentication_required
//     among the codes it does not retry: the schedule and attempt count carry
//     on, no retry charges the card, and each reminder carries the pay link;
//   - a different card the customer saves becomes the default, and the next
//     retry charges it;
//   - control: an ordinary decline is still a decline, and its retry charges
//     the same card again.
func TestE2E_OffSessionCharge_BankNeedsCustomerToConfirm(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// A configured server queues customer emails, so each one shows up in the
	// outbox.
	t.Setenv("SMTP_HOST", "smtp.needs-confirm.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Needs Confirm Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
		"name": "Confirm policy", "enabled": true, "retry_days": []int{1, 3, 3},
	}}).want(t, 201, "create dunning policy")
	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_needs_confirm", PublishableKey: "pk_test_needs_confirm",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	const signingSecret = "whsec_needs_confirm"
	creds := call("PATCH", "/v1/settings/stripe/test/webhook", flowReq{body: map[string]any{"webhook_secret": signingSecret}}).want(t, 200, "set webhook secret").body
	endpoint := "/v1/webhooks/stripe/" + flowStr(creds, "id")
	stripe := installConfirmStripe(t)

	frozen := time.Date(2027, 5, 3, 9, 0, 0, 0, time.UTC)
	clockID := flowStr(call("POST", "/v1/test-clocks", flowReq{body: map[string]any{
		"name": "needs confirm", "frozen_time": frozen.Format(time.RFC3339),
	}}).want(t, 201, "create test clock").body, "id")
	advance := func(days int) {
		t.Helper()
		call("POST", "/v1/test-clocks/"+clockID+"/advance", flowReq{body: map[string]any{
			"frozen_time": frozen.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339),
		}}).want(t, 200, fmt.Sprintf("advance clock to day %d", days))
	}
	newCustomer := func(name string) string {
		t.Helper()
		return flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
			"external_id": "confirm-" + name, "display_name": "Confirm " + name, "email": name + "@needs-confirm.test",
			"test_clock_id": clockID,
		}}).want(t, 201, name+": create customer").body, "id")
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
			"line_items":  []map[string]any{{"description": desc, "quantity": 1, "unit_amount_cents": 5000}},
		}}).want(t, 201, "create invoice: "+desc).body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, "finalize: "+desc)
		return id
	}
	getInvoice := func(id string) map[string]any {
		t.Helper()
		return flowObj(call("GET", "/v1/invoices/"+id, flowReq{}).want(t, 200, "get invoice").body, "invoice")
	}
	runOf := func(invoiceID string) map[string]any {
		t.Helper()
		runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(t, 200, "list dunning runs").body, "data")
		if len(runs) != 1 {
			t.Fatalf("dunning runs on %s: %v, want one", invoiceID, runs)
		}
		return runs[0]
	}
	actionCodes := func(att map[string]any) []string {
		var out []string
		for _, a := range flowList(att, "actions") {
			out = append(out, flowStr(a, "code"))
		}
		return out
	}
	// emailsOf is each customer email queued for the invoice, oldest first:
	// its type, the reason it shows (the dispatcher reads a payment_failed
	// row's reason, a dunning_warning row's failure_reason), and which copy
	// it uses.
	type queued struct{ kind, reason, ask string }
	emailsOf := func(invoiceID string) []queued {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer postgres.Rollback(tx)
		rows, err := tx.QueryContext(t.Context(), `SELECT email_type, COALESCE(CASE WHEN email_type = 'payment_failed' THEN payload->>'reason' ELSE payload->>'failure_reason' END, ''), COALESCE(payload->>'decline_ask', '')
			FROM email_outbox
			WHERE tenant_id = $1 AND email_type IN ('payment_failed', 'dunning_warning', 'dunning_escalation')
			  AND payload->>'invoice_number' = (SELECT invoice_number FROM invoices WHERE id = $2)
			ORDER BY created_at, id`, tenantID, invoiceID)
		if err != nil {
			t.Fatalf("read email outbox: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []queued
		for rows.Next() {
			var q queued
			if err := rows.Scan(&q.kind, &q.reason, &q.ask); err != nil {
				t.Fatalf("scan email outbox: %v", err)
			}
			out = append(out, q)
		}
		return out
	}
	timeline := func(id string) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, e := range flowList(call("GET", "/v1/invoices/"+id+"/payment-timeline", flowReq{}).want(t, 200, "payment timeline").body, "events") {
			out = append(out, flowObj(e, "line"))
		}
		return out
	}
	linesOfKind := func(lines []map[string]any, kind string) []map[string]any {
		var out []map[string]any
		for _, l := range lines {
			if flowStr(l, "kind") == kind {
				out = append(out, l)
			}
		}
		return out
	}
	noDevText := func(what, s string) {
		t.Helper()
		if lower := strings.ToLower(s); strings.Contains(lower, "off session") || strings.Contains(lower, "requires authentication") ||
			strings.Contains(lower, "return to your application") {
			t.Errorf("%s shows Stripe's developer text: %q", what, s)
		}
	}

	// A: the customer's only card needs them to confirm off-session charges.
	custA := newCustomer("a")
	saveCard(custA, "pm_3ds_a")
	invA := newInvoice(custA, "May usage")
	if got := stripe.chargedCards(); len(got) != 1 || got[0] != "pm_3ds_a" {
		t.Fatalf("charges at finalize: %v, want one on pm_3ds_a", got)
	}
	inv := getInvoice(invA)
	if flowStr(inv, "status") != "finalized" || flowStr(inv, "payment_status") != "failed" {
		t.Fatalf("invoice A after finalize: status %v payment_status %v, want finalized/failed", inv["status"], inv["payment_status"])
	}

	// The operator's notice.
	att := flowObj(inv, "attention")
	if flowStr(att, "reason") != "payment_failed" || flowStr(att, "code") != "payment.authentication_required" {
		t.Errorf("attention reason %q code %q, want payment_failed / payment.authentication_required", flowStr(att, "reason"), flowStr(att, "code"))
	}
	msg := flowStr(att, "message")
	if !strings.Contains(msg, "bank needs them to confirm") || strings.Contains(strings.ToLower(msg), "declined") {
		t.Errorf("attention message %q, want it to say the bank needs the customer to confirm, and not call it a decline", msg)
	}
	if codes := actionCodes(att); len(codes) != 1 || codes[0] != "send_reminder" {
		t.Errorf("attention actions %v, want only send_reminder (email the payment link); Retry would charge off-session again", codes)
	}
	noDevText("attention detail", flowStr(att, "detail"))

	// The activity, in plain words.
	failed := linesOfKind(timeline(invA), "payment.failed")
	if len(failed) != 1 {
		t.Fatalf("invoice A timeline 'Payment failed' rows: %v, want one", failed)
	}
	problem := flowStr(failed[0], "problem")
	noDevText("the Payment failed row", problem)
	if !strings.Contains(problem, "confirm") {
		t.Errorf("the Payment failed row says %q, want it to say the bank asked the customer to confirm", problem)
	}

	// The customer's email: confirm the payment, with the pay link.
	emails := emailsOf(invA)
	if len(emails) != 1 || emails[0].kind != "payment_failed" {
		t.Fatalf("emails for invoice A after finalize: %+v, want one payment_failed", emails)
	}
	if emails[0].ask != "confirm" {
		t.Errorf("payment_failed email copy %q, want confirm (the bank asked the customer to confirm the payment)", emails[0].ask)
	}
	noDevText("the payment_failed email", emails[0].reason)

	// Collect payment still asks Stripe (an operator's charge is not blocked),
	// and its answer says what happened in plain words.
	collect := call("POST", "/v1/invoices/"+invA+"/collect", flowReq{})
	if collect.status < 400 {
		t.Fatalf("collect on invoice A: status %d, want a refusal; body %s", collect.status, collect.raw)
	}
	if m := collect.errMessage(); strings.Contains(strings.ToLower(m), "declined") || !strings.Contains(m, "confirm") {
		t.Errorf("collect answered %q, want it to say the customer must confirm the payment, not that the card was declined", m)
	}
	// That charge emails the customer again (one email per failed charge,
	// P3 of the same review), with the same copy.
	emails = emailsOf(invA)
	if last := emails[len(emails)-1]; last.kind != "payment_failed" || last.ask != "confirm" {
		t.Errorf("email after Collect payment: %+v, want a payment_failed asking the customer to confirm", last)
	}
	sentBeforeRetry := len(emails)

	// B: an ordinary decline, for the control.
	custB := newCustomer("b")
	saveCard(custB, "pm_nsf_b")
	invB := newInvoice(custB, "May usage B")
	attB := flowObj(getInvoice(invB), "attention")
	if flowStr(attB, "code") != "payment.declined" {
		t.Errorf("invoice B attention code %q, want payment.declined", flowStr(attB, "code"))
	}
	if codes := actionCodes(attB); len(codes) != 2 || codes[1] != "retry_payment" {
		t.Errorf("invoice B attention actions %v, want update_payment_method then retry_payment", codes)
	}
	chargesBefore := len(stripe.chargedCards())

	// Retry 1 (day 1 of grace): no charge on A's card; the attempt counts and
	// the reminder asks the customer to confirm, with the pay link. B's
	// retry charges B's card again.
	advance(2)
	charged := stripe.chargedCards()[chargesBefore:]
	if len(charged) != 1 || charged[0] != "pm_nsf_b" {
		t.Errorf("charges on retry 1: %v, want only pm_nsf_b (A's card needs the customer, not a retry)", charged)
	}
	runA := runOf(invA)
	if flowStr(runA, "state") != "active" || flowInt(runA, "attempt_count") != 1 {
		t.Errorf("invoice A run after retry 1: state %v attempts %v, want active / 1", runA["state"], runA["attempt_count"])
	}
	emails = emailsOf(invA)[sentBeforeRetry:]
	if len(emails) != 1 || emails[0].kind != "dunning_warning" || emails[0].ask != "confirm" {
		t.Errorf("emails for invoice A on retry 1: %+v, want one dunning_warning asking the customer to confirm", emails)
	}
	if len(emails) == 1 {
		noDevText("the dunning warning", emails[0].reason)
	}
	notAttempted := linesOfKind(timeline(invA), "payment_recovery.retry_not_attempted")
	if len(notAttempted) != 1 || !strings.Contains(flowStr(notAttempted[0], "problem"), "confirm") {
		t.Errorf("invoice A timeline 'not attempted' rows %v, want one saying the customer must confirm", notAttempted)
	}
	emailsB := emailsOf(invB)
	if len(emailsB) != 2 || emailsB[1].kind != "dunning_warning" || emailsB[1].ask != "" {
		t.Errorf("emails for invoice B after retry 1: %+v, want the ordinary dunning warning", emailsB)
	}
	// The ordinary decline's email gives Stripe's message for the card error
	// as the reason.
	if len(emailsB) > 0 && (emailsB[0].kind != "payment_failed" || emailsB[0].reason != "Your card was declined.") {
		t.Errorf("first email for invoice B: %+v, want a payment_failed with Stripe's reason", emailsB[0])
	}
	if codes := actionCodes(flowObj(getInvoice(invA), "attention")); len(codes) != 1 || codes[0] != "send_reminder" {
		t.Errorf("invoice A attention actions after retry 1: %v, want only send_reminder", codes)
	}

	// The customer saves a different card through the setup link. It becomes
	// the default, and retry 2 (day 4) charges it.
	saveCard(custA, "pm_ok_a")
	if code := flowStr(flowObj(getInvoice(invA), "attention"), "code"); code != "payment.declined" {
		t.Errorf("invoice A attention once a different card is the default: %q, want the ordinary payment.declined", code)
	}
	chargesBefore = len(stripe.chargedCards())
	advance(5)
	charged = stripe.chargedCards()[chargesBefore:]
	if len(charged) != 2 || charged[0] == charged[1] || (charged[0] != "pm_ok_a" && charged[1] != "pm_ok_a") {
		t.Errorf("charges on retry 2: %v, want pm_ok_a for A and pm_nsf_b for B", charged)
	}
	if inv := getInvoice(invA); flowStr(inv, "status") != "paid" {
		t.Errorf("invoice A after retry 2 on the new card: status %v, want paid", inv["status"])
	}
	if run := runOf(invA); flowStr(run, "state") != "resolved" || flowStr(run, "resolution") != "payment_recovered" {
		t.Errorf("invoice A run after retry 2: %v, want resolved / payment_recovered", run)
	}
}
