package api

import (
	"bytes"
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

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A dunning retry whose payment Stripe is still processing must not close
// dunning as recovered (#1289). Before, the run was resolved as
// payment_recovered with dunning.resolved the moment the retry came back
// processing; when that payment then failed, nothing retried the invoice.
//
// These drive the real router, database, charge path, signed webhooks and a
// test clock. Only api.stripe.com is stood in for: the finalize-time charge
// is declined (so dunning starts), and each dunning retry gets the next
// answer scripted for its card.

// inFlightStripe stands in for api.stripe.com.
type inFlightStripe struct {
	mu      sync.Mutex
	seq     int
	script  map[string][]string // card → answers for its dunning retries, in order
	retries []string            // the card of every dunning retry charge, in order
	lastPI  map[string]string   // card → the PaymentIntent of its latest retry
}

func installInFlightStripe(t *testing.T, script map[string][]string) *inFlightStripe {
	t.Helper()
	s := &inFlightStripe{script: script, lastPI: map[string]string{}}
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

func (s *inFlightStripe) answer(req *http.Request) *http.Response {
	form := url.Values{}
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		form, _ = url.ParseQuery(string(b))
	}
	call := req.Method + " " + req.URL.Path
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	n := s.seq

	status, body := 200, ""
	switch {
	case call == "POST /v1/customers":
		body = fmt.Sprintf(`{"id":"cus_slow_%d","object":"customer"}`, n)
	case req.Method == "POST" && strings.HasPrefix(req.URL.Path, "/v1/customers/"):
		body = fmt.Sprintf(`{"id":%q,"object":"customer"}`, strings.TrimPrefix(req.URL.Path, "/v1/customers/"))
	case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/v1/payment_methods/"):
		id := strings.TrimPrefix(req.URL.Path, "/v1/payment_methods/")
		body = fmt.Sprintf(`{"id":%q,"object":"payment_method","type":"card","card":{"brand":"visa","display_brand":"visa","last4":"4242","exp_month":12,"exp_year":2034,"fingerprint":"fp_%s"}}`, id, id)
	case call == "POST /v1/checkout/sessions":
		body = fmt.Sprintf(`{"id":"cs_slow_%d","object":"checkout.session","mode":"setup","url":"https://checkout.stripe.test/c/cs_slow_%d"}`, n, n)
	case call == "POST /v1/payment_intents":
		pm := form.Get("payment_method")
		amount, _ := strconv.Atoi(form.Get("amount"))
		if form.Get("metadata[hardcount_purpose]") != "dunning_retry" {
			// The charge at finalize is declined, so dunning starts.
			status, body = 402, fmt.Sprintf(confirmDecline, n)
			break
		}
		s.retries = append(s.retries, pm)
		next := "paid"
		if q := s.script[pm]; len(q) > 0 {
			next, s.script[pm] = q[0], q[1:]
		}
		switch next {
		case "processing":
			s.lastPI[pm] = fmt.Sprintf("pi_slow_%d", n)
			body = fmt.Sprintf(`{"id":"pi_slow_%d","object":"payment_intent","status":"processing","amount":%d,"currency":"usd"}`, n, amount)
		case "decline":
			s.lastPI[pm] = fmt.Sprintf("pi_nsf_%d", n)
			status, body = 402, fmt.Sprintf(confirmDecline, n)
		default:
			s.lastPI[pm] = fmt.Sprintf("pi_ok_%d", n)
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

func (s *inFlightStripe) retryCards() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.retries...)
}

func (s *inFlightStripe) piOf(pm string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPI[pm]
}

// inFlightRig is one tenant with a dunning policy, Stripe connected, a
// webhook endpoint and a test clock.
type inFlightRig struct {
	t        *testing.T
	db       *postgres.DB
	ts       *httptest.Server
	tenantID string
	key      string
	endpoint string
	secret   string
	clockID  string
	frozen   time.Time
	stripe   *inFlightStripe
	evtSeq   int
}

func newInFlightRig(t *testing.T, name string, retryDays []int, script map[string][]string) *inFlightRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	// A configured server queues customer emails, so each one shows up in
	// the outbox.
	t.Setenv("SMTP_HOST", "smtp.retry-in-flight.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	r := &inFlightRig{t: t, db: db, ts: ts, secret: "whsec_retry_in_flight",
		frozen: time.Date(2027, 5, 3, 9, 0, 0, 0, time.UTC)}
	r.tenantID = testutil.CreateTestTenant(t, db, name)
	r.key = createAPIKeyWithMode(t, db, r.tenantID, false)
	r.call("POST", "/v1/dunning/policies", map[string]any{
		"name": name + " policy", "enabled": true, "retry_days": retryDays,
		"final_subscription_action": "none", "final_invoice_action": "none",
	}).want(t, 201, "create dunning policy")
	if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
		TenantID: r.tenantID, Livemode: false, SecretKey: "sk_test_retry_in_flight", PublishableKey: "pk_test_retry_in_flight",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	creds := r.call("PATCH", "/v1/settings/stripe/test/webhook", map[string]any{"webhook_secret": r.secret}).want(t, 200, "set webhook secret").body
	r.endpoint = "/v1/webhooks/stripe/" + flowStr(creds, "id")
	r.stripe = installInFlightStripe(t, script)
	r.clockID = flowStr(r.call("POST", "/v1/test-clocks", map[string]any{
		"name": name, "frozen_time": r.frozen.Format(time.RFC3339),
	}).want(t, 201, "create test clock").body, "id")
	return r
}

func (r *inFlightRig) call(method, path string, body any) flowResp {
	r.t.Helper()
	return flowDo(r.t, r.ts, method, path, flowReq{bearer: r.key, body: body})
}

func (r *inFlightRig) day(days int) time.Time {
	return r.frozen.Add(time.Duration(days) * 24 * time.Hour)
}

func (r *inFlightRig) advance(days int) {
	r.t.Helper()
	r.call("POST", "/v1/test-clocks/"+r.clockID+"/advance", map[string]any{
		"frozen_time": r.day(days).Format(time.RFC3339),
	}).want(r.t, 200, fmt.Sprintf("advance clock to day %d", days))
}

// webhook signs and posts one Stripe event to the tenant's endpoint.
func (r *inFlightRig) webhook(evt map[string]any) flowResp {
	r.t.Helper()
	payload, sig := signedStripeEvent(r.t, r.secret, evt)
	return flowDo(r.t, r.ts, "POST", r.endpoint, flowReq{header: map[string]string{"Stripe-Signature": sig}, body: payload})
}

// piEvent is a payment_intent.* event for a dunning retry's PaymentIntent.
func (r *inFlightRig) piEvent(eventType, pi, invoiceID, status string, lastErr map[string]any) map[string]any {
	r.evtSeq++
	obj := map[string]any{
		"id": pi, "object": "payment_intent", "status": status, "amount": 5000, "currency": "usd",
		"metadata": map[string]any{
			"hardcount_tenant_id": r.tenantID, "hardcount_invoice_id": invoiceID, "hardcount_purpose": "dunning_retry",
		},
	}
	if status == "succeeded" {
		obj["amount_received"] = 5000
	}
	if lastErr != nil {
		obj["last_payment_error"] = lastErr
	}
	return map[string]any{
		"id": fmt.Sprintf("evt_%s_%d", pi, r.evtSeq), "object": "event", "type": eventType,
		"created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": obj},
	}
}

func cardError(declineCode, message, pm string) map[string]any {
	return map[string]any{
		"type": "card_error", "code": "card_declined", "decline_code": declineCode, "message": message,
		"payment_method": map[string]any{"id": pm, "object": "payment_method"},
	}
}

// newCustomer is a customer on the test clock whose saved card is pm.
func (r *inFlightRig) newCustomer(name, pm string) string {
	r.t.Helper()
	id := flowStr(r.call("POST", "/v1/customers", map[string]any{
		"external_id": "slow-" + name, "display_name": "Slow " + name, "email": name + "@retry-in-flight.test",
		"test_clock_id": r.clockID,
	}).want(r.t, 201, name+": create customer").body, "id")
	r.call("POST", "/v1/customers/"+id+"/payment-methods/setup-session", map[string]any{}).want(r.t, 201, "setup session")
	r.webhook(map[string]any{
		"id": "evt_setup_" + pm, "object": "event", "type": "setup_intent.succeeded",
		"created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{
			"id": "seti_" + pm, "object": "setup_intent", "status": "succeeded", "payment_method": pm,
			"metadata": map[string]any{"hardcount_customer_id": id},
		}},
	}).want(r.t, 200, "setup_intent.succeeded")
	return id
}

// newInvoice finalizes a $50 invoice; its charge is declined and dunning
// starts.
func (r *inFlightRig) newInvoice(customerID string) string {
	r.t.Helper()
	id := flowStr(r.call("POST", "/v1/invoices", map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "May usage", "quantity": 1, "unit_amount_cents": 5000}},
	}).want(r.t, 201, "create invoice").body, "id")
	r.call("POST", "/v1/invoices/"+id+"/finalize", nil).want(r.t, 200, "finalize")
	if inv := r.invoice(id); flowStr(inv, "payment_status") != "failed" {
		r.t.Fatalf("invoice after its finalize-time decline: payment_status %v, want failed", inv["payment_status"])
	}
	return id
}

func (r *inFlightRig) invoice(id string) map[string]any {
	r.t.Helper()
	return flowObj(r.call("GET", "/v1/invoices/"+id, nil).want(r.t, 200, "get invoice").body, "invoice")
}

func (r *inFlightRig) run(invoiceID string) map[string]any {
	r.t.Helper()
	runs := flowList(r.call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, nil).want(r.t, 200, "list dunning runs").body, "data")
	if len(runs) != 1 {
		r.t.Fatalf("dunning runs on %s: %v, want one", invoiceID, runs)
	}
	return runs[0]
}

func (r *inFlightRig) runTime(run map[string]any, field string) time.Time {
	r.t.Helper()
	v := flowStr(run, field)
	at, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		r.t.Fatalf("run %s %q: %v", field, v, err)
	}
	return at.UTC()
}

// warnings are the dunning warning emails queued for the invoice, oldest
// first: the attempt each is for, its reason line and its copy.
type queuedWarning struct {
	attempt     string
	reason, ask string
}

func (r *inFlightRig) warnings(invoiceID string) []queuedWarning {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.t.Context(), postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.t.Context(), `SELECT COALESCE(payload->>'attempt_number', ''), COALESCE(payload->>'failure_reason', ''), COALESCE(payload->>'decline_ask', '')
		FROM email_outbox
		WHERE tenant_id = $1 AND email_type = 'dunning_warning'
		  AND payload->>'invoice_number' = (SELECT invoice_number FROM invoices WHERE id = $2)
		ORDER BY created_at, id`, r.tenantID, invoiceID)
	if err != nil {
		r.t.Fatalf("read email outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []queuedWarning
	for rows.Next() {
		var w queuedWarning
		if err := rows.Scan(&w.attempt, &w.reason, &w.ask); err != nil {
			r.t.Fatalf("scan email outbox: %v", err)
		}
		out = append(out, w)
	}
	return out
}

func (r *inFlightRig) emailsOfType(invoiceID, emailType string) int {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2
		AND payload->>'invoice_number' = (SELECT invoice_number FROM invoices WHERE id = $3)`, r.tenantID, emailType, invoiceID)
}

func (r *inFlightRig) resolvedEvents(invoiceID string) int {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = 'dunning.resolved'
		AND payload->'dunning_run'->>'invoice_id' = $2`, r.tenantID, invoiceID)
}

// held reports whether the invoice's charge hold is set.
func (r *inFlightRig) held(invoiceID string) bool {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM invoices WHERE id = $1 AND auto_charge_claimed_until IS NOT NULL`, invoiceID) == 1
}

// timelineKinds are the kinds of the invoice's payment-timeline rows, in order.
func (r *inFlightRig) timelineKinds(invoiceID string) []string {
	r.t.Helper()
	var out []string
	for _, e := range flowList(r.call("GET", "/v1/invoices/"+invoiceID+"/payment-timeline", nil).want(r.t, 200, "payment timeline").body, "events") {
		out = append(out, flowStr(flowObj(e, "line"), "kind"))
	}
	return out
}

func countOf(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

// TestE2E_DunningRetryProcessing_ThenFails is #1289's case: retry 1 comes back
// processing and the payment later fails.
//
//   - While it is processing the run stays active at one attempt: no
//     dunning.resolved, no warning email, a "processing" row on the timeline.
//   - The failure arrives from the webhook and the reconciler at once
//     (concurrent settles): one failed row, one warning email with Stripe's
//     card message, the charge hold released, and retry 2 moved to the
//     failure plus the next wait.
//   - Retry 2 runs on that date. It is declined at once: its own warning, so
//     each attempt has one email.
//   - Retry 3 succeeds: the run is resolved once, with one dunning.resolved.
func TestE2E_DunningRetryProcessing_ThenFails(t *testing.T) {
	r := newInFlightRig(t, "Retry In Flight Fails", []int{1, 3, 3}, map[string][]string{
		"pm_slow_a": {"processing", "decline", "paid"},
	})
	cust := r.newCustomer("a", "pm_slow_a")
	inv := r.newInvoice(cust)

	// Retry 1 (day 1): the payment comes back processing.
	r.advance(2)
	if cards := r.stripe.retryCards(); len(cards) != 1 {
		t.Fatalf("dunning retry charges after day 2: %v, want one", cards)
	}
	pi := r.stripe.piOf("pm_slow_a")
	if got := r.invoice(inv); flowStr(got, "payment_status") != "processing" || flowStr(got, "status") != "finalized" {
		t.Fatalf("invoice after retry 1: %v/%v, want finalized/processing", got["status"], got["payment_status"])
	}
	run := r.run(inv)
	if flowStr(run, "state") != "active" || flowInt(run, "attempt_count") != 1 {
		t.Fatalf("run while retry 1 is processing: state %v attempts %v, want active / 1 (before #1289: resolved as recovered)",
			run["state"], run["attempt_count"])
	}
	if got, want := r.runTime(run, "next_action_at"), r.day(1).Add(3*24*time.Hour); !got.Equal(want) {
		t.Errorf("next retry while processing: %v, want %v (rescheduled as a failure is)", got, want)
	}
	if n := r.resolvedEvents(inv); n != 0 {
		t.Errorf("dunning.resolved events while the payment is processing: %d, want 0", n)
	}
	if w := r.warnings(inv); len(w) != 0 {
		t.Errorf("warning emails while the payment is processing: %+v, want none", w)
	}
	if kinds := r.timelineKinds(inv); countOf(kinds, "payment_recovery.retry_processing") != 1 {
		t.Errorf("timeline kinds %v, want one payment_recovery.retry_processing row", kinds)
	}

	// The failure (day 2), reported by the webhook and the reconciler's
	// webhook-equivalent at the same moment.
	failure := cardError("insufficient_funds", "Your card has insufficient funds.", "pm_slow_a")
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		payload, sig := signedStripeEvent(t, r.secret, r.piEvent("payment_intent.payment_failed", pi, inv, "requires_payment_method", failure))
		wg.Add(1)
		go func(i int, payload []byte, sig string) {
			defer wg.Done()
			req, err := http.NewRequest("POST", r.ts.URL+r.endpoint, bytes.NewReader(payload))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Stripe-Signature", sig)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			_ = resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i, payload, sig)
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Fatalf("failure report %d: status %d, want 200", i, s)
		}
	}
	if got := r.invoice(inv); flowStr(got, "payment_status") != "failed" {
		t.Fatalf("invoice after the failure: payment_status %v, want failed", got["payment_status"])
	}
	w := r.warnings(inv)
	if len(w) != 1 || w[0].attempt != "1" || w[0].reason != "Your card has insufficient funds." || w[0].ask != "" {
		t.Fatalf("warning emails after two reports of the failure: %+v, want one for attempt 1 with the card message", w)
	}
	run = r.run(inv)
	if flowStr(run, "state") != "active" || flowInt(run, "attempt_count") != 1 {
		t.Errorf("run after the failure: %v / %v, want active / 1", run["state"], run["attempt_count"])
	}
	if got, want := r.runTime(run, "next_action_at"), r.day(2).Add(3*24*time.Hour); !got.Equal(want) {
		t.Errorf("next retry after the failure: %v, want the failure plus the next wait %v", got, want)
	}
	if r.held(inv) {
		t.Error("the retry's charge hold is still set after its payment failed: the next retry would wait for the stale-hold sweep")
	}
	kinds := r.timelineKinds(inv)
	if countOf(kinds, "payment_recovery.retry_failed") != 1 || countOf(kinds, "payment_recovery.retry_processing") != 1 {
		t.Errorf("timeline kinds %v, want one processing row and one failed row for retry 1", kinds)
	}

	// Retry 2 (day 5) is declined at once: its own warning.
	r.advance(6)
	if cards := r.stripe.retryCards(); len(cards) != 2 {
		t.Fatalf("dunning retry charges after day 6: %v, want two (retry 2 ran on its moved date)", cards)
	}
	w = r.warnings(inv)
	if len(w) != 2 || w[1].attempt != "2" {
		t.Fatalf("warning emails after retry 2: %+v, want two, one per attempt", w)
	}
	if run := r.run(inv); flowStr(run, "state") != "active" || flowInt(run, "attempt_count") != 2 {
		t.Errorf("run after retry 2: %v / %v, want active / 2", run["state"], run["attempt_count"])
	}

	// Retry 3 (day 8) succeeds.
	r.advance(9)
	if got := r.invoice(inv); flowStr(got, "status") != "paid" {
		t.Fatalf("invoice after retry 3: %v, want paid", got["status"])
	}
	run = r.run(inv)
	if flowStr(run, "state") != "resolved" || flowStr(run, "resolution") != "payment_recovered" || flowInt(run, "attempt_count") != 3 {
		t.Errorf("run after retry 3: %v, want resolved / payment_recovered / 3", run)
	}
	if n := r.resolvedEvents(inv); n != 1 {
		t.Errorf("dunning.resolved events: %d, want 1", n)
	}
	if n := len(r.warnings(inv)); n != 2 {
		t.Errorf("warning emails in all: %d, want 2", n)
	}
}

// TestE2E_DunningRetryProcessing_ThenSucceeds: the processing payment
// succeeds. Its webhook resolves the run as recovered, once, and no warning
// goes out.
func TestE2E_DunningRetryProcessing_ThenSucceeds(t *testing.T) {
	r := newInFlightRig(t, "Retry In Flight Pays", []int{1, 3, 3}, map[string][]string{"pm_slow_b": {"processing"}})
	inv := r.newInvoice(r.newCustomer("b", "pm_slow_b"))
	r.advance(2)
	pi := r.stripe.piOf("pm_slow_b")
	if run := r.run(inv); flowStr(run, "state") != "active" {
		t.Fatalf("run while processing: %v, want active", run["state"])
	}
	r.webhook(r.piEvent("payment_intent.succeeded", pi, inv, "succeeded", nil)).want(t, 200, "payment_intent.succeeded")
	if got := r.invoice(inv); flowStr(got, "status") != "paid" {
		t.Fatalf("invoice after the success webhook: %v, want paid", got["status"])
	}
	run := r.run(inv)
	if flowStr(run, "state") != "resolved" || flowStr(run, "resolution") != "payment_recovered" || flowInt(run, "attempt_count") != 1 {
		t.Errorf("run: %v, want resolved / payment_recovered / 1", run)
	}
	// A later advance changes nothing, and the success redelivered resolves
	// nothing twice.
	r.webhook(r.piEvent("payment_intent.succeeded", pi, inv, "succeeded", nil)).want(t, 200, "payment_intent.succeeded again")
	r.advance(10)
	if n := r.resolvedEvents(inv); n != 1 {
		t.Errorf("dunning.resolved events: %d, want 1", n)
	}
	if w := r.warnings(inv); len(w) != 0 {
		t.Errorf("warning emails: %+v, want none", w)
	}
	if cards := r.stripe.retryCards(); len(cards) != 1 {
		t.Errorf("dunning retry charges: %v, want one", cards)
	}
}

// TestE2E_DunningRetryProcessing_FailsNeedingConfirmation: a processing
// payment that fails with authentication_required asks the customer to
// confirm the payment, with no reason line (Stripe's text for it is written
// for the developer).
func TestE2E_DunningRetryProcessing_FailsNeedingConfirmation(t *testing.T) {
	r := newInFlightRig(t, "Retry In Flight Confirm", []int{1, 3, 3}, map[string][]string{"pm_slow_c": {"processing"}})
	inv := r.newInvoice(r.newCustomer("c", "pm_slow_c"))
	r.advance(2)
	pi := r.stripe.piOf("pm_slow_c")
	r.webhook(r.piEvent("payment_intent.payment_failed", pi, inv, "requires_payment_method",
		cardError("authentication_required", confirmDevText, "pm_slow_c"))).want(t, 200, "payment_intent.payment_failed")
	w := r.warnings(inv)
	if len(w) != 1 || w[0].ask != "confirm" || w[0].reason != "" {
		t.Fatalf("warning emails: %+v, want one asking the customer to confirm, with no reason line", w)
	}
}

// TestE2E_DunningFinalRetryProcessing_HeldThenEscalatedAtFailure: the last
// retry comes back processing. No final action and no escalation while it
// is processing; its failure sends no warning (the escalation is the last
// attempt's email), and the next tick escalates, dated at the failure rather
// than at the retry days before it.
func TestE2E_DunningFinalRetryProcessing_HeldThenEscalatedAtFailure(t *testing.T) {
	r := newInFlightRig(t, "Final Retry In Flight", []int{1}, map[string][]string{"pm_slow_d": {"processing"}})
	inv := r.newInvoice(r.newCustomer("d", "pm_slow_d"))
	r.advance(2)
	pi := r.stripe.piOf("pm_slow_d")
	run := r.run(inv)
	if flowStr(run, "state") != "active" || flowInt(run, "attempt_count") != 1 {
		t.Fatalf("run while the last retry is processing: %v / %v, want active / 1", run["state"], run["attempt_count"])
	}
	r.advance(4)
	if run := r.run(inv); flowStr(run, "state") != "active" {
		t.Fatalf("run two days into the processing payment: %v, want active (the final action waits)", run["state"])
	}
	if n := r.emailsOfType(inv, "dunning_escalation"); n != 0 {
		t.Fatalf("escalation emails while the last payment is processing: %d, want 0", n)
	}

	// The payment fails on day 4.
	r.webhook(r.piEvent("payment_intent.payment_failed", pi, inv, "requires_payment_method",
		cardError("insufficient_funds", "Your card has insufficient funds.", "pm_slow_d"))).want(t, 200, "payment_intent.payment_failed")
	if w := r.warnings(inv); len(w) != 0 {
		t.Errorf("warning emails after the last retry failed: %+v, want none (the escalation follows)", w)
	}
	r.advance(5)
	run = r.run(inv)
	if flowStr(run, "state") != "escalated" {
		t.Fatalf("run after the next tick: %v, want escalated", run["state"])
	}
	if got, want := r.runTime(run, "resolved_at"), r.day(4); !got.Equal(want) {
		t.Errorf("escalation dated %v, want the failure %v (not the retry on %v)", got, want, r.day(1))
	}
	if n := r.emailsOfType(inv, "dunning_escalation"); n != 1 {
		t.Errorf("escalation emails: %d, want 1", n)
	}
}
