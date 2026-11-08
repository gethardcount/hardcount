package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The Stripe connection routes (/v1/settings/stripe) over the real router and
// database, with each credential they take. Each mode has its own Stripe
// connection, and every caller sees and changes only the one of its own mode,
// as it does with all other data: an API key the mode it was made in, a
// dashboard session the mode it is in. Settings → Payments shows that mode's
// connection, and the Test/Live toggle switches it. Settings shared by both
// modes (PUT /v1/settings) are changed only from the dashboard.

// stripeModeRig is a bootstrapped account with its test key, its live key and
// the owner's dashboard session. A new session starts in test mode.
type stripeModeRig struct {
	t       *testing.T
	ts      *httptest.Server
	stub    *stripeStub
	testKey flowReq
	liveKey flowReq
	session flowReq
}

func newStripeModeRig(t *testing.T) *stripeModeRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "stripe-mode-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	// Connect checks the saved key with GET /v1/account. Stripe Tax refuses
	// every calculation, as it does a revoked key; only the retry journey
	// calculates tax.
	stub := installStripeStub(t)
	stub.extra = func(call string, _ url.Values) (int, string, bool) {
		switch call {
		case "GET /v1/account":
			return 200, `{"id":"acct_FAKE_mode_flow","object":"account","business_profile":{"name":"Mode Flow"}}`, true
		case "POST /v1/tax/calculations":
			return 401, `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided: sk_live_****FAKE"}}`, true
		}
		return 0, "", false
	}

	const email, password = "owner@stripe-mode.test", "stripe-mode-pw-12"
	boot := flowBootstrap(t, ts, "stripe-mode-bootstrap", email, password)
	return &stripeModeRig{
		t: t, ts: ts, stub: stub,
		testKey: flowReq{bearer: flowStr(boot, "secret_key_test")},
		liveKey: flowReq{bearer: flowStr(boot, "secret_key_live")},
		session: flowReq{cookie: login(t, ts, email, password)},
	}
}

func (r *stripeModeRig) call(as flowReq, method, path string, body any) flowResp {
	r.t.Helper()
	as.body = body
	return flowDo(r.t, r.ts, method, path, as)
}

// connectBody is a POST /v1/settings/stripe body for the mode. The keys are
// obviously fake and carry only the real prefix, which is all Hardcount
// checks before it asks Stripe.
func connectBody(live bool, tag string) map[string]any {
	mode := modeName(live)
	return map[string]any{
		"livemode":        live,
		"secret_key":      "sk_" + mode + "_FAKE_" + tag,
		"publishable_key": "pk_" + mode + "_FAKE_" + tag,
		"webhook_secret":  "whsec_FAKE_" + mode + "_" + tag,
	}
}

func modeName(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// connections lists the Stripe connections the credential sees, by mode.
func (r *stripeModeRig) connections(as flowReq) map[bool]map[string]any {
	r.t.Helper()
	out := map[bool]map[string]any{}
	for _, c := range flowList(r.call(as, "GET", "/v1/settings/stripe", nil).want(r.t, 200, "list Stripe connections").body, "data") {
		live, _ := c["livemode"].(bool)
		out[live] = c
	}
	return out
}

// connectBoth connects each mode with that mode's key.
func (r *stripeModeRig) connectBoth() {
	r.t.Helper()
	r.call(r.testKey, "POST", "/v1/settings/stripe", connectBody(false, "own")).want(r.t, 200, "the test key connects test mode")
	r.call(r.liveKey, "POST", "/v1/settings/stripe", connectBody(true, "own")).want(r.t, 200, "the live key connects live mode")
}

// both is each mode's connection, as that mode's key sees it.
func (r *stripeModeRig) both() map[bool]map[string]any {
	r.t.Helper()
	return map[bool]map[string]any{
		false: r.connections(r.testKey)[false],
		true:  r.connections(r.liveKey)[true],
	}
}

// invoice reads a live invoice with the live key.
func (r *stripeModeRig) invoice(id string) map[string]any {
	r.t.Helper()
	return flowObj(r.call(r.liveKey, "GET", "/v1/invoices/"+id, nil).want(r.t, 200, "read invoice "+id).body, "invoice")
}

// refused fails the test unless the answer is the 403 for a connection of
// another mode.
func refused(t *testing.T, res flowResp, what string) {
	t.Helper()
	if res.status != http.StatusForbidden || res.errCode() != "forbidden" {
		t.Errorf("%s: status %d, want 403 forbidden; body: %s", what, res.status, res.raw)
		return
	}
	if !strings.Contains(res.errMessage(), "Stripe connection") {
		t.Errorf("%s: the 403 says %q, want it to name the Stripe connection", what, res.errMessage())
	}
}

// TestE2E_StripeSettings_TestKeyCannotConnectLive: a test key that sends a
// connect for live mode gets 403. The live connection is unchanged, and
// Stripe is never called with the refused keys.
func TestE2E_StripeSettings_TestKeyCannotConnectLive(t *testing.T) {
	r := newStripeModeRig(t)
	r.call(r.liveKey, "POST", "/v1/settings/stripe", connectBody(true, "own")).want(t, 200, "the live key connects live mode")
	before := r.connections(r.liveKey)[true]
	checks := r.stub.count("GET /v1/account")

	refused(t, r.call(r.testKey, "POST", "/v1/settings/stripe", connectBody(true, "testkey")), "a test key connects live mode")

	if after := r.connections(r.liveKey)[true]; !reflect.DeepEqual(before, after) {
		t.Errorf("the live connection changed after a refused connect:\nbefore %v\nafter  %v", before, after)
	}
	if n := r.stub.count("GET /v1/account"); n != checks {
		t.Errorf("Stripe was asked to check the refused keys (%d checks, want %d)", n, checks)
	}
}

// TestE2E_StripeSettings_KeyCannotChangeOtherMode: neither key can replace,
// set the webhook secret of, or delete the other mode's connection. Each
// answer is 403, and the other mode's connection is unchanged.
func TestE2E_StripeSettings_KeyCannotChangeOtherMode(t *testing.T) {
	r := newStripeModeRig(t)
	r.connectBoth()
	before := r.both()

	for _, tc := range []struct {
		name   string
		key    flowReq
		target bool
	}{
		{"test key on live mode", r.testKey, true},
		{"live key on test mode", r.liveKey, false},
	} {
		mode := modeName(tc.target)
		refused(t, r.call(tc.key, "PATCH", "/v1/settings/stripe/"+mode+"/webhook", map[string]any{"webhook_secret": "whsec_FAKE_other"}), tc.name+": set the webhook secret")
		refused(t, r.call(tc.key, "POST", "/v1/settings/stripe", connectBody(tc.target, "other")), tc.name+": connect")
		refused(t, r.call(tc.key, "DELETE", "/v1/settings/stripe/"+mode, nil), tc.name+": disconnect")
	}

	after := r.both()
	for _, live := range []bool{false, true} {
		if !reflect.DeepEqual(before[live], after[live]) {
			t.Errorf("the %s connection changed after refused requests:\nbefore %v\nafter  %v", modeName(live), before[live], after[live])
		}
	}
}

// TestE2E_StripeSettings_KeyListsOwnModeOnly: with both modes connected, a
// key's list holds only its own mode's connection.
func TestE2E_StripeSettings_KeyListsOwnModeOnly(t *testing.T) {
	r := newStripeModeRig(t)
	r.connectBoth()
	for _, tc := range []struct {
		name string
		key  flowReq
		live bool
	}{
		{"test key", r.testKey, false},
		{"live key", r.liveKey, true},
	} {
		got := r.connections(tc.key)
		if len(got) != 1 || got[tc.live] == nil {
			t.Errorf("%s lists %v, want only the %s connection", tc.name, got, modeName(tc.live))
		}
	}
}

// TestE2E_StripeSettings_SessionManagesItsOwnMode: a dashboard session
// manages the connection of the mode it is in, the one Settings → Payments
// shows. In test mode it connects test mode and is refused live mode.
// Switched to live mode, as the Test/Live toggle does, it connects live mode
// and is refused test mode. Its list holds only the mode it is in.
func TestE2E_StripeSettings_SessionManagesItsOwnMode(t *testing.T) {
	r := newStripeModeRig(t)
	r.call(r.session, "POST", "/v1/settings/stripe", connectBody(false, "session")).want(t, 200, "a test-mode session connects test mode")
	r.call(r.session, "PATCH", "/v1/settings/stripe/test/webhook", map[string]any{"webhook_secret": "whsec_FAKE_test_new"}).want(t, 200, "a test-mode session sets the test webhook secret")
	refused(t, r.call(r.session, "POST", "/v1/settings/stripe", connectBody(true, "session")), "a test-mode session connects live mode")
	if got := r.connections(r.session); len(got) != 1 || got[false] == nil {
		t.Errorf("a test-mode session lists %v, want only test mode", got)
	}

	r.call(r.session, "POST", "/v1/auth/mode", map[string]any{"livemode": true}).want(t, 200, "switch the session to live mode")
	r.call(r.session, "POST", "/v1/settings/stripe", connectBody(true, "session")).want(t, 200, "a live-mode session connects live mode")
	r.call(r.session, "PATCH", "/v1/settings/stripe/live/webhook", map[string]any{"webhook_secret": "whsec_FAKE_live_new"}).want(t, 200, "a live-mode session sets the live webhook secret")
	before := r.connections(r.testKey)[false]
	refused(t, r.call(r.session, "PATCH", "/v1/settings/stripe/test/webhook", map[string]any{"webhook_secret": "whsec_FAKE_test_other"}), "a live-mode session sets the test webhook secret")
	refused(t, r.call(r.session, "DELETE", "/v1/settings/stripe/test", nil), "a live-mode session disconnects test mode")
	if after := r.connections(r.testKey)[false]; after == nil || !reflect.DeepEqual(before, after) {
		t.Errorf("the test connection changed after refused requests:\nbefore %v\nafter  %v", before, after)
	}
	if got := r.connections(r.session); len(got) != 1 || got[true] == nil {
		t.Errorf("a live-mode session lists %v, want only live mode", got)
	}
	r.call(r.session, "DELETE", "/v1/settings/stripe/live", nil).want(t, 204, "a live-mode session disconnects live mode")
}

// TestE2E_StripeSettings_ConnectRetriesStuckDrafts: connecting a mode's
// Stripe keys retries the tax of that mode's drafts stuck on the old keys
// (ADR-019), and retries_queued counts them. A live draft is stuck on
// provider_auth, because Stripe Tax refused the live key. Connecting test
// mode counts nothing. The dashboard, switched to live mode, replaces the
// live key: retries_queued is 1, and the draft's tax is calculated again.
func TestE2E_StripeSettings_ConnectRetriesStuckDrafts(t *testing.T) {
	r := newStripeModeRig(t)
	r.call(r.session, "PUT", "/v1/settings", map[string]any{"tax_provider": "stripe_tax"}).want(t, 200, "use Stripe Tax")
	r.connectBoth()

	cus := flowStr(r.call(r.liveKey, "POST", "/v1/customers", map[string]any{
		"external_id": "stuck-live", "display_name": "Stuck Live",
	}).want(t, 201, "create a live customer").body, "id")
	r.call(r.liveKey, "PUT", "/v1/customers/"+cus+"/billing-profile", map[string]any{
		"legal_name": "Stuck Live", "address_line1": "1 Market St", "city": "San Francisco",
		"state": "CA", "postal_code": "94105", "country": "US",
	}).want(t, 200, "set the customer's address")
	inv := flowStr(r.call(r.liveKey, "POST", "/v1/invoices", map[string]any{
		"customer_id": cus,
		"line_items":  []map[string]any{{"description": "Seats", "quantity": 1, "unit_amount_cents": 5000}},
	}).want(t, 201, "create a live draft").body, "id")
	if res := r.call(r.liveKey, "POST", "/v1/invoices/"+inv+"/finalize", nil); res.status == http.StatusOK {
		t.Fatalf("finalize with a refused Stripe Tax key: status 200, want it blocked; body: %s", res.raw)
	}
	draft := r.invoice(inv)
	if flowStr(draft, "tax_status") != "pending" || flowStr(draft, "tax_error_code") != "provider_auth" {
		t.Fatalf("the draft's tax is %q/%q, want pending/provider_auth", flowStr(draft, "tax_status"), flowStr(draft, "tax_error_code"))
	}
	tries := flowInt(draft, "tax_retry_count")
	calcs := r.stub.count("POST /v1/tax/calculations")

	if n := flowInt(r.call(r.testKey, "POST", "/v1/settings/stripe", connectBody(false, "rotated")).want(t, 200, "the test key replaces the test key").body, "retries_queued"); n != 0 {
		t.Errorf("connecting test mode queued %d retries, want 0: the stuck draft is live", n)
	}

	r.call(r.session, "POST", "/v1/auth/mode", map[string]any{"livemode": true}).want(t, 200, "switch the session to live mode")
	if n := flowInt(r.call(r.session, "POST", "/v1/settings/stripe", connectBody(true, "rotated")).want(t, 200, "the live-mode session replaces the live key").body, "retries_queued"); n != 1 {
		t.Fatalf("connecting live mode queued %d retries, want 1 for the stuck live draft", n)
	}
	// The retry runs after the answer. Wait for it, so it ends inside the test.
	deadline := time.Now().Add(15 * time.Second)
	for flowInt(r.invoice(inv), "tax_retry_count") <= tries {
		if time.Now().After(deadline) {
			t.Fatalf("the stuck draft's tax was not retried within 15s (tax_retry_count %d)", tries)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := r.stub.count("POST /v1/tax/calculations"); n != calcs+1 {
		t.Errorf("Stripe Tax was asked %d more times, want 1 retry", n-calcs)
	}
}

// TestE2E_StripeSettings_KeyManagesOwnMode: each key still manages its own
// mode's connection, as docs/guides/connect-stripe.md shows with a test key:
// connect, set the webhook secret, list, disconnect.
func TestE2E_StripeSettings_KeyManagesOwnMode(t *testing.T) {
	r := newStripeModeRig(t)
	for _, tc := range []struct {
		name string
		key  flowReq
		live bool
	}{
		{"test key", r.testKey, false},
		{"live key", r.liveKey, true},
	} {
		mode := modeName(tc.live)
		body := connectBody(tc.live, "own")
		delete(body, "webhook_secret")
		creds := r.call(tc.key, "POST", "/v1/settings/stripe", body).want(t, 200, tc.name+" connects "+mode+" mode").body
		if creds["livemode"] != tc.live || flowStr(creds, "stripe_account_name") != "Mode Flow" || creds["verified_at"] == nil {
			t.Errorf("%s connect answered %v, want a verified %s connection to Mode Flow", tc.name, creds, mode)
		}
		r.call(tc.key, "PATCH", "/v1/settings/stripe/"+mode+"/webhook", map[string]any{"webhook_secret": "whsec_FAKE_own"}).want(t, 200, tc.name+" sets the webhook secret")
		if got := r.connections(tc.key); len(got) != 1 || got[tc.live]["has_webhook_secret"] != true {
			t.Errorf("%s lists %v, want its %s connection with a webhook secret", tc.name, got, mode)
		}
		r.call(tc.key, "DELETE", "/v1/settings/stripe/"+mode, nil).want(t, 204, tc.name+" disconnects")
		if got := r.connections(tc.key); len(got) != 0 {
			t.Errorf("%s lists %v after its disconnect, want nothing", tc.name, got)
		}
	}
}

// TestE2E_Settings_ChangeTakesDashboardSession: the account settings are
// shared by both modes, so a key, which belongs to one mode, reads them but
// gets 403 on a change. The dashboard changes them.
func TestE2E_Settings_ChangeTakesDashboardSession(t *testing.T) {
	r := newStripeModeRig(t)
	terms := func(as flowReq) int64 {
		t.Helper()
		return flowInt(r.call(as, "GET", "/v1/settings", nil).want(t, 200, "read settings").body, "net_payment_terms")
	}
	start := terms(r.testKey)

	for _, tc := range []struct {
		name string
		key  flowReq
	}{{"test key", r.testKey}, {"live key", r.liveKey}} {
		res := r.call(tc.key, "PUT", "/v1/settings", map[string]any{"net_payment_terms": start + 7})
		if res.status != http.StatusForbidden || !strings.Contains(res.errMessage(), "requires a dashboard session") {
			t.Errorf("%s changes settings: status %d, want 403 asking for a dashboard session; body: %s", tc.name, res.status, res.raw)
		}
	}
	if got := terms(r.liveKey); got != start {
		t.Errorf("net_payment_terms is %d after refused changes, want %d", got, start)
	}

	r.call(r.session, "PUT", "/v1/settings", map[string]any{"net_payment_terms": start + 7}).want(t, 200, "the dashboard changes settings")
	if got := terms(r.testKey); got != start+7 {
		t.Errorf("net_payment_terms is %d after the dashboard's change, want %d", got, start+7)
	}
}
