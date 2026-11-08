package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The Stripe connection routes (/v1/settings/stripe) over the real router and
// database, with each credential they take. An API key sees and changes only
// the connection of its own mode, as it does with all other data. A dashboard
// session manages both modes, because Settings → Payments shows and edits both
// side by side. Settings shared by both modes (PUT /v1/settings) are changed
// only from the dashboard.

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
	// Connect checks the saved key with GET /v1/account.
	stub := installStripeStub(t)
	stub.extra = func(call string, _ url.Values) (int, string, bool) {
		if call != "GET /v1/account" {
			return 0, "", false
		}
		return 200, `{"id":"acct_FAKE_mode_flow","object":"account","business_profile":{"name":"Mode Flow"}}`, true
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
	r.call(r.session, "POST", "/v1/settings/stripe", connectBody(true, "session")).want(t, 200, "the dashboard connects live mode")
	before := r.connections(r.session)[true]
	checks := r.stub.count("GET /v1/account")

	refused(t, r.call(r.testKey, "POST", "/v1/settings/stripe", connectBody(true, "testkey")), "a test key connects live mode")

	if after := r.connections(r.session)[true]; !reflect.DeepEqual(before, after) {
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
	for _, live := range []bool{false, true} {
		r.call(r.session, "POST", "/v1/settings/stripe", connectBody(live, "session")).want(t, 200, "the dashboard connects "+modeName(live)+" mode")
	}
	before := r.connections(r.session)

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

	after := r.connections(r.session)
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
	for _, live := range []bool{false, true} {
		r.call(r.session, "POST", "/v1/settings/stripe", connectBody(live, "session")).want(t, 200, "the dashboard connects "+modeName(live)+" mode")
	}
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

// TestE2E_StripeSettings_SessionManagesBothModes: the dashboard manages both
// connections from either mode. A session in test mode connects live mode
// and sets its webhook secret. Switched to live mode, it disconnects test
// mode. It lists both modes throughout.
func TestE2E_StripeSettings_SessionManagesBothModes(t *testing.T) {
	r := newStripeModeRig(t)
	for _, live := range []bool{false, true} {
		r.call(r.session, "POST", "/v1/settings/stripe", connectBody(live, "session")).want(t, 200, "a test-mode session connects "+modeName(live)+" mode")
	}
	r.call(r.session, "PATCH", "/v1/settings/stripe/live/webhook", map[string]any{"webhook_secret": "whsec_FAKE_live_new"}).want(t, 200, "a test-mode session sets the live webhook secret")
	if got := r.connections(r.session); len(got) != 2 {
		t.Fatalf("a test-mode session lists %v, want both modes", got)
	}

	r.call(r.session, "POST", "/v1/auth/mode", map[string]any{"livemode": true}).want(t, 200, "switch the session to live mode")
	if got := r.connections(r.session); len(got) != 2 {
		t.Fatalf("a live-mode session lists %v, want both modes", got)
	}
	r.call(r.session, "DELETE", "/v1/settings/stripe/test", nil).want(t, 204, "a live-mode session disconnects test mode")
	if got := r.connections(r.session); len(got) != 1 || got[true] == nil {
		t.Errorf("after the test disconnect the session lists %v, want only live mode", got)
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
