package api

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_DashboardSession_ModeAndPasswordReset walks a signed-in operator's
// session over the real router and database: GET /v1/whoami on both
// credential paths, the test/live switch (lists follow it, and a key made
// in live mode is a live key), and a password reset from the emailed link
// to a new sign-in. GET /v1/whoami, POST /v1/auth/mode and GET
// /v1/auth/password-reset/check had no test at any level.
func TestE2E_DashboardSession_ModeAndPasswordReset(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", "session-flow-bootstrap")
	// Reset links are built only from this setting, never from the request.
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.session-flow.test")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@session-flow.test", "session-flow-pw-12"
	boot := flowBootstrap(t, ts, "session-flow-bootstrap", email, password)
	tenantID := flowStr(boot, "tenant", "id")

	// One customer in each mode, each made with that mode's key.
	newCustomer := func(key, mode string) string {
		t.Helper()
		return flowStr(flowDo(t, ts, "POST", "/v1/customers", flowReq{bearer: key, body: map[string]any{
			"external_id": "session-flow-" + mode, "display_name": "Session Flow " + mode, "email": mode + "@session-flow.test",
		}}).want(t, 201, "create "+mode+" customer").body, "id")
	}
	testCustomer := newCustomer(flowStr(boot, "secret_key_test"), "test")
	liveCustomer := newCustomer(flowStr(boot, "secret_key_live"), "live")

	cookie := login(t, ts, email, password)
	whoami := func() map[string]any {
		t.Helper()
		return flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: cookie}).want(t, 200, "whoami").body
	}
	customerList := func() string {
		t.Helper()
		res := flowDo(t, ts, "GET", "/v1/customers", flowReq{cookie: cookie}).want(t, 200, "customer list")
		var ids []string
		for _, c := range flowList(res.body, "data") {
			ids = append(ids, flowStr(c, "id"))
		}
		return strings.Join(ids, ",")
	}
	setMode := func(live bool) {
		t.Helper()
		res := flowDo(t, ts, "POST", "/v1/auth/mode", flowReq{cookie: cookie, body: map[string]any{"livemode": live}}).want(t, 200, "switch mode")
		if res.body["livemode"] != live {
			t.Fatalf("mode switch answered %s, want livemode %v", res.raw, live)
		}
	}

	// whoami on the session names the person and the account. The dashboard
	// signs the operator out when user_id is missing.
	who := whoami()
	if flowStr(who, "user_id") == "" || flowStr(who, "email") != email || flowStr(who, "tenant_id") != tenantID ||
		who["livemode"] != false || flowStr(who, "key_id") != "" {
		t.Errorf("session whoami = %v, want user_id, %s, account %s, test mode, no key_id", who, email, tenantID)
	}
	// On a bearer key it names the key instead.
	whoKey := flowDo(t, ts, "GET", "/v1/whoami", flowReq{bearer: flowStr(boot, "secret_key_live")}).want(t, 200, "whoami with a key").body
	if flowStr(whoKey, "key_type") != "secret" || flowStr(whoKey, "key_id") == "" || flowStr(whoKey, "user_id") != "" || whoKey["livemode"] != true {
		t.Errorf("live key whoami = %v, want a live secret key and no user_id", whoKey)
	}

	// A new session starts in test mode and lists test data only.
	if got := customerList(); got != testCustomer {
		t.Errorf("test mode customer list = %q, want only %q", got, testCustomer)
	}

	// Switch to live: whoami, the lists and new keys follow the session.
	setMode(true)
	if whoami()["livemode"] != true {
		t.Error("whoami after switching to live still reports test mode")
	}
	if got := customerList(); got != liveCustomer {
		t.Errorf("live mode customer list = %q, want only %q", got, liveCustomer)
	}
	liveKey := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: cookie, body: map[string]any{
		"name": "Live server", "key_type": "secret",
	}}).want(t, 201, "create a key in live mode").body
	if raw := flowStr(liveKey, "raw_key"); !strings.HasPrefix(raw, "vlx_secret_live_") || flowObj(liveKey, "key")["livemode"] != true {
		t.Errorf("key made in live mode: raw %q, key %v; want a vlx_secret_live_ key", raw, flowObj(liveKey, "key"))
	}

	// And back.
	setMode(false)
	if whoami()["livemode"] != false {
		t.Error("whoami after switching back still reports live mode")
	}
	if got := customerList(); got != testCustomer {
		t.Errorf("customer list after switching back = %q, want only %q", got, testCustomer)
	}
	flowDo(t, ts, "POST", "/v1/auth/mode", flowReq{body: map[string]any{"livemode": true}}).want(t, 401, "switch mode with no session")
	assertAuditAction(t, db, "mode_changed")

	// Password reset: request it, open the emailed link, set a new password.
	flowDo(t, ts, "POST", "/v1/auth/password-reset/request", flowReq{body: map[string]any{"email": email}}).want(t, 200, "request a reset")
	token := flowEmailLinkToken(t, db, "password_reset", "password_reset_url")
	check := func(tok string) flowResp {
		t.Helper()
		return flowDo(t, ts, "GET", "/v1/auth/password-reset/check?token="+url.QueryEscape(tok), flowReq{})
	}
	if res := check(token).want(t, 200, "check the emailed link"); res.body["valid"] != true {
		t.Errorf("check of a fresh link = %s, want valid true", res.raw)
	}
	// Checking does not use the link up; the page checks on every load.
	check(token).want(t, 200, "check the link a second time")
	if res := check("not-a-reset-token").want(t, 422, "check a made-up link"); res.body["valid"] != false || res.body["reason"] != "invalid_or_expired" {
		t.Errorf("check of a made-up link = %s, want valid false, reason invalid_or_expired", res.raw)
	}
	flowDo(t, ts, "GET", "/v1/auth/password-reset/check", flowReq{}).want(t, 422, "check with no token")

	const newPassword = "session-flow-new-pw-34"
	flowDo(t, ts, "POST", "/v1/auth/password-reset/confirm", flowReq{body: map[string]any{
		"token": token, "password": newPassword,
	}}).want(t, 200, "set the new password")

	// The reset signs out every session and spends the link.
	flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: cookie}).want(t, 401, "whoami on a session from before the reset")
	check(token).want(t, 422, "check a used link")
	flowDo(t, ts, "POST", "/v1/auth/password-reset/confirm", flowReq{body: map[string]any{
		"token": token, "password": "session-flow-third-pw-56",
	}}).want(t, 422, "use the link twice")

	// Only the new password signs in.
	flowDo(t, ts, "POST", "/v1/auth/login", flowReq{body: map[string]any{"email": email, "password": password}}).want(t, 401, "sign in with the old password")
	after := login(t, ts, email, newPassword)
	if got := flowStr(flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: after}).want(t, 200, "whoami after the reset").body, "email"); got != email {
		t.Errorf("whoami after the reset names %q, want %q", got, email)
	}
}
