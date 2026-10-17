package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Helpers for the *_flow_e2e_test.go tests. Each of those walks one kind of
// flow through the real router and a real Postgres, from the HTTP request to
// what the API answers afterwards. Each starts from POST /v1/bootstrap, so the
// account, owner, keys and seeded dunning policies are the ones a real install
// has, and each makes its own uniquely named data.

// flow is one bootstrapped account and an HTTP client for it.
type flow struct {
	t        *testing.T
	db       *postgres.DB
	ts       *httptest.Server
	tenantID string
	testKey  string // vlx_secret_test_…, from the bootstrap response
	liveKey  string
	email    string // the owner's sign-in
	password string
}

const flowBootstrapToken = "flow-e2e-bootstrap-token"

// newFlow sets env before NewServer reads it, then bootstraps an account.
func newFlow(t *testing.T, env map[string]string) *flow {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", flowBootstrapToken)
	for k, v := range env {
		t.Setenv(k, v)
	}
	ts := httptest.NewServer(NewServer(db, nil, nil))
	t.Cleanup(ts.Close)

	f := &flow{t: t, db: db, ts: ts,
		email:    "owner-" + uniq() + "@flow-e2e.test",
		password: "flow-e2e-pw-" + uniq(),
	}
	boot := f.must(http.StatusCreated, http.MethodPost, "/v1/bootstrap", map[string]any{
		"token":          flowBootstrapToken,
		"tenant_name":    "Flow E2E " + uniq(),
		"owner_email":    f.email,
		"owner_password": f.password,
	})
	f.testKey = str(boot["secret_key_test"])
	f.liveKey = str(boot["secret_key_live"])
	f.tenantID = str(obj(boot["tenant"])["id"])
	if f.testKey == "" || f.liveKey == "" || f.tenantID == "" {
		t.Fatalf("bootstrap response is missing keys or tenant: %v", boot)
	}
	return f
}

// uniq is a short random suffix for names, so data from one run never
// matches data from another.
func uniq() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// reply is one HTTP answer, with its body read to the end. Reading to the end
// matters: it returns only after the handler and the middleware after it (the
// idempotency write) have finished.
type reply struct {
	status  int
	header  http.Header
	cookies []*http.Cookie
	raw     []byte
	body    map[string]any
}

type reqOpt func(*http.Request)

func bearer(key string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
}

func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) { r.AddCookie(c) }
}

func withHeader(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

// send makes one request. body is nil (no body), []byte (sent as is) or a
// value sent as JSON.
func (f *flow) send(method, path string, body any, opts ...reqOpt) reply {
	f.t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			f.t.Fatalf("encode %s %s body: %v", method, path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rdr)
	if err != nil {
		f.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatalf("read %s %s: %v", method, path, err)
	}
	out := reply{status: res.StatusCode, header: res.Header, cookies: res.Cookies(), raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// must is send that fails the test unless the status is want.
func (f *flow) must(want int, method, path string, body any, opts ...reqOpt) map[string]any {
	f.t.Helper()
	r := f.send(method, path, body, opts...)
	if r.status != want {
		f.t.Fatalf("%s %s: status %d, want %d; body: %s", method, path, r.status, want, r.raw)
	}
	return r.body
}

// login signs in through POST /v1/auth/login and returns the session cookie.
func (f *flow) login(email, password string) *http.Cookie {
	f.t.Helper()
	r := f.send(http.MethodPost, "/v1/auth/login", map[string]any{"email": email, "password": password})
	if r.status != http.StatusOK {
		f.t.Fatalf("login %s: status %d; body: %s", email, r.status, r.raw)
	}
	for _, c := range r.cookies {
		if c.Name == "velox_session" && c.Value != "" {
			return c
		}
	}
	f.t.Fatal("login set no velox_session cookie")
	return nil
}

// customer creates a customer with a billing email.
func (f *flow) customer() map[string]any {
	f.t.Helper()
	u := uniq()
	return f.must(http.StatusCreated, http.MethodPost, "/v1/customers", map[string]any{
		"external_id":  "flow_" + u,
		"display_name": "Flow Customer " + u,
		"email":        "billing-" + u + "@flow-e2e.test",
	}, bearer(f.testKey))
}

// finalizedInvoice composes a one-line USD invoice for the customer and
// finalizes it, as the invoice composer does. The response is the invoice
// after finalize has tried to collect it.
func (f *flow) finalizedInvoice(customerID string, cents int64) map[string]any {
	f.t.Helper()
	draft := f.must(http.StatusCreated, http.MethodPost, "/v1/invoices", map[string]any{
		"customer_id": customerID,
		"currency":    "USD",
		"line_items": []map[string]any{{
			"description": "Flow E2E service " + uniq(), "line_type": "add_on",
			"quantity": 1, "unit_amount_cents": cents,
		}},
	}, bearer(f.testKey))
	return f.must(http.StatusOK, http.MethodPost, "/v1/invoices/"+str(draft["id"])+"/finalize", nil, bearer(f.testKey))
}

// invoice reads an invoice the way its page does.
func (f *flow) invoice(id string) map[string]any {
	f.t.Helper()
	return obj(f.must(http.StatusOK, http.MethodGet, "/v1/invoices/"+id, nil, bearer(f.testKey))["invoice"])
}

// timeline returns the invoice's payment timeline rows with this event_type.
func (f *flow) timeline(invoiceID, eventType string) []map[string]any {
	f.t.Helper()
	tl := f.must(http.StatusOK, http.MethodGet, "/v1/invoices/"+invoiceID+"/payment-timeline", nil, bearer(f.testKey))
	if d := arr(tl["degraded"]); len(d) > 0 {
		f.t.Errorf("payment timeline for %s degraded: %v", invoiceID, d)
	}
	var out []map[string]any
	for _, e := range arr(tl["events"]) {
		if ev := obj(e); ev["event_type"] == eventType {
			out = append(out, ev)
		}
	}
	return out
}

// sentEmails returns the customer's Sent emails rows of this type.
func (f *flow) sentEmails(customerID, emailType string) []map[string]any {
	f.t.Helper()
	body := f.must(http.StatusOK, http.MethodGet, "/v1/customers/"+customerID+"/sent-emails", nil, bearer(f.testKey))
	var out []map[string]any
	for _, e := range arr(body["sent_emails"]) {
		if row := obj(e); row["email_type"] == emailType {
			out = append(out, row)
		}
	}
	return out
}

// dunningRuns lists the invoice's dunning runs.
func (f *flow) dunningRuns(invoiceID string) []map[string]any {
	f.t.Helper()
	body := f.must(http.StatusOK, http.MethodGet, "/v1/dunning/runs?invoice_id="+invoiceID, nil, bearer(f.testKey))
	var out []map[string]any
	for _, r := range arr(body["data"]) {
		out = append(out, obj(r))
	}
	return out
}

// count runs a COUNT query past row-level security. It is for "happened
// exactly once" facts no endpoint reports, such as how many outbound events
// were queued.
func (f *flow) count(query string, args ...any) int {
	f.t.Helper()
	ctx := f.t.Context()
	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		f.t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// queuedEvents counts the outbound webhook events of this type queued for
// the invoice.
func (f *flow) queuedEvents(eventType, invoiceID string) int {
	f.t.Helper()
	return f.count(`SELECT COUNT(*) FROM webhook_outbox
		WHERE tenant_id = $1 AND event_type = $2 AND payload->>'invoice_id' = $3`,
		f.tenantID, eventType, invoiceID)
}

// emailedToken returns the token in the link of the newest queued email of
// this type. In production the email dispatcher sends this row; none runs
// in these tests, so the queued row is what the recipient would get.
func (f *flow) emailedToken(emailType, linkField string) string {
	f.t.Helper()
	ctx := f.t.Context()
	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		f.t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var raw []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT payload FROM email_outbox
		WHERE tenant_id = $1 AND email_type = $2
		ORDER BY created_at DESC LIMIT 1`, f.tenantID, emailType).Scan(&raw); err != nil {
		f.t.Fatalf("read queued %s email: %v", emailType, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		f.t.Fatalf("decode queued %s email: %v", emailType, err)
	}
	link := str(payload[linkField])
	_, token, ok := strings.Cut(link, "token=")
	if !ok || token == "" {
		f.t.Fatalf("queued %s email has no token in %s: %q", emailType, linkField, link)
	}
	token, _, _ = strings.Cut(token, "&")
	return token
}

// connectStripe connects test-mode Stripe the way Settings → Payments does
// and returns the webhook endpoint id and its signing secret. Needs a
// fakeStripe, which answers the key check.
func (f *flow) connectStripe() (endpointID, secret string) {
	f.t.Helper()
	secret = "whsec_flow_" + uniq()
	creds := f.must(http.StatusOK, http.MethodPost, "/v1/settings/stripe", map[string]any{
		"livemode":        false,
		"secret_key":      "sk_test_flow_" + uniq(),
		"publishable_key": "pk_test_flow_" + uniq(),
		"webhook_secret":  secret,
	}, bearer(f.testKey))
	if creds["verified_at"] == nil || creds["has_webhook_secret"] != true || str(creds["id"]) == "" {
		f.t.Fatalf("Stripe connect did not verify the key and store the webhook secret: %v", creds)
	}
	return str(creds["id"]), secret
}

// deliverStripeEvent posts an event to the Stripe webhook endpoint, signed
// with secret the way Stripe signs it.
func (f *flow) deliverStripeEvent(endpointID, secret string, event []byte) reply {
	f.t.Helper()
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: event, Secret: secret})
	return f.send(http.MethodPost, "/v1/webhooks/stripe/"+endpointID, event, withHeader("Stripe-Signature", signed.Header))
}

// paymentIntentEvent is a payment_intent event for a charge Velox made: its
// metadata names the invoice and the account, as chargeInvoice sets it.
func paymentIntentEvent(eventID, eventType, piID string, inv map[string]any, tenantID, declineMessage string) []byte {
	pi := map[string]any{
		"id":       piID,
		"object":   "payment_intent",
		"amount":   num(inv["amount_due_cents"]),
		"currency": strings.ToLower(str(inv["currency"])),
		"customer": "cus_flow_charge",
		"metadata": map[string]string{
			"velox_invoice_id":     str(inv["id"]),
			"velox_invoice_number": str(inv["invoice_number"]),
			"velox_tenant_id":      tenantID,
			"velox_customer_id":    str(inv["customer_id"]),
		},
	}
	if eventType == "payment_intent.succeeded" {
		pi["status"] = "succeeded"
		pi["amount_received"] = pi["amount"]
	} else {
		pi["status"] = "requires_payment_method"
		pi["last_payment_error"] = map[string]any{"message": declineMessage}
	}
	b, _ := json.Marshal(map[string]any{
		"id":       eventID,
		"object":   "event",
		"type":     eventType,
		"created":  time.Now().Unix(),
		"livemode": false,
		"data":     map[string]any{"object": pi},
	})
	return b
}

// fakeStripe stands in for api.stripe.com. It answers the calls these flows
// make the way Stripe does and counts them, so no test reaches the network.
// Install it after newFlow: NewServer copies http.DefaultTransport and needs
// it to be the standard *http.Transport.
type fakeStripe struct {
	mu    sync.Mutex
	calls map[string]int // "METHOD /path"
	seq   int
}

func newFakeStripe(t *testing.T) *fakeStripe {
	t.Helper()
	s := &fakeStripe{calls: map[string]int{}}
	real := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.stripe.com" {
			return real.RoundTrip(req)
		}
		status, body := s.answer(req)
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = real })
	return s
}

func (s *fakeStripe) answer(req *http.Request) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[req.Method+" "+req.URL.Path]++
	s.seq++
	path := req.URL.Path
	switch {
	case req.Method == http.MethodGet && path == "/v1/account":
		return http.StatusOK, `{"id":"acct_flow_e2e","object":"account","business_profile":{"name":"Flow E2E"}}`
	case req.Method == http.MethodPost && path == "/v1/customers":
		return http.StatusOK, fmt.Sprintf(`{"id":"cus_flow_%d","object":"customer"}`, s.seq)
	case req.Method == http.MethodPost && path == "/v1/checkout/sessions":
		return http.StatusOK, fmt.Sprintf(`{"id":"cs_flow_%d","object":"checkout.session","mode":"setup","url":"https://checkout.stripe.com/c/pay/cs_flow_%d"}`, s.seq, s.seq)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/v1/payment_intents/"):
		id := strings.TrimPrefix(path, "/v1/payment_intents/")
		return http.StatusOK, fmt.Sprintf(`{"id":%q,"object":"payment_intent","status":"succeeded",`+
			`"payment_method":{"id":"pm_flow","object":"payment_method","type":"card",`+
			`"card":{"brand":"visa","display_brand":"visa","last4":"4242","exp_month":12,"exp_year":2030}}}`, id)
	}
	return http.StatusNotFound, `{"error":{"type":"invalid_request_error","message":"no such route in the flow-test Stripe fake"}}`
}

// count reports how many times call ("METHOD /path") was made.
func (s *fakeStripe) count(call string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[call]
}

// JSON readers: a missing or mistyped field reads as the zero value, and the
// assertion that uses it fails with the value it got.

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

// errCode is the code in a Stripe-style error body.
func errCode(body map[string]any) string {
	return str(obj(body["error"])["code"])
}
