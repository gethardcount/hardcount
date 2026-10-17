package api

import (
	"net/http"
	"testing"
)

// TestE2E_SessionWhoamiModeAndPasswordReset walks what the dashboard does
// with its session cookie: ask who is signed in (GET /whoami), switch between
// test and live data (POST /auth/mode), and reset a forgotten password through
// the emailed link (GET /auth/password-reset/check before and after the
// reset). None of these three endpoints had a test before.
func TestE2E_SessionWhoamiModeAndPasswordReset(t *testing.T) {
	f := newFlow(t, map[string]string{
		// Reset emails need SMTP and the dashboard address for the link. No
		// dispatcher runs here, so nothing connects to this host.
		"SMTP_HOST":          "smtp.flow-e2e.invalid",
		"DASHBOARD_BASE_URL": "http://dashboard.flow-e2e.test",
	})
	session := f.login(f.email, f.password)

	// whoami on the cookie names the signed-in user, in test mode.
	who := f.must(http.StatusOK, http.MethodGet, "/v1/whoami", nil, withCookie(session))
	userID := str(who["user_id"])
	if userID == "" || who["email"] != f.email || who["tenant_id"] != f.tenantID || who["livemode"] != false || who["smtp_configured"] != true {
		t.Errorf("whoami on the session = %v; want user %s, tenant %s, test mode, SMTP configured", who, f.email, f.tenantID)
	}
	if _, ok := who["key_id"]; ok {
		t.Errorf("whoami on a session carries key_id: %v", who)
	}
	f.must(http.StatusUnauthorized, http.MethodGet, "/v1/whoami", nil)

	// A test-mode customer, made through the session.
	testCust := f.must(http.StatusCreated, http.MethodPost, "/v1/customers", map[string]any{
		"external_id": "flow_test_" + uniq(), "display_name": "Flow test-mode customer",
	}, withCookie(session))
	testCustID := str(testCust["id"])

	// Switch to live: the session now reads live data only.
	f.must(http.StatusUnauthorized, http.MethodPost, "/v1/auth/mode", map[string]any{"livemode": true})
	if mode := f.must(http.StatusOK, http.MethodPost, "/v1/auth/mode", map[string]any{"livemode": true}, withCookie(session)); mode["livemode"] != true {
		t.Fatalf("switch to live: %v", mode)
	}
	if who := f.must(http.StatusOK, http.MethodGet, "/v1/whoami", nil, withCookie(session)); who["livemode"] != true || who["user_id"] != userID {
		t.Errorf("whoami after switching to live = %v; want the same user in live mode", who)
	}
	liveList := f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, withCookie(session))
	if findByID(liveList["data"], testCustID) != nil {
		t.Error("in live mode, the customer list shows a test-mode customer")
	}
	f.must(http.StatusNotFound, http.MethodGet, "/v1/customers/"+testCustID, nil, withCookie(session))
	liveCust := f.must(http.StatusCreated, http.MethodPost, "/v1/customers", map[string]any{
		"external_id": "flow_live_" + uniq(), "display_name": "Flow live-mode customer",
	}, withCookie(session))
	liveCustID := str(liveCust["id"])
	// The live secret key sees the customer the live session made.
	if findByID(f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, bearer(f.liveKey))["data"], liveCustID) == nil {
		t.Error("the live key does not see the customer made in the live session")
	}

	// Switch back to test.
	if mode := f.must(http.StatusOK, http.MethodPost, "/v1/auth/mode", map[string]any{"livemode": false}, withCookie(session)); mode["livemode"] != false {
		t.Fatalf("switch back to test: %v", mode)
	}
	testList := f.must(http.StatusOK, http.MethodGet, "/v1/customers", nil, withCookie(session))
	if findByID(testList["data"], testCustID) == nil || findByID(testList["data"], liveCustID) != nil {
		t.Errorf("back in test mode, the customer list = %v; want the test customer and not the live one", testList["data"])
	}
	// One audit row per switch.
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE tenant_id = $1 AND action = 'mode_changed'`, f.tenantID); n != 2 {
		t.Errorf("mode_changed audit rows: %d, want 2", n)
	}

	// Forgot password: the reset link is emailed.
	req := f.must(http.StatusOK, http.MethodPost, "/v1/auth/password-reset/request", map[string]any{"email": f.email})
	if req["email_delivery"] != "ok" {
		t.Errorf("reset request email_delivery = %v, want ok", req["email_delivery"])
	}
	token := f.emailedToken("password_reset", "password_reset_url")

	// The reset page checks the link before showing the form. Checking does
	// not use it up.
	for range 2 {
		if check := f.must(http.StatusOK, http.MethodGet, "/v1/auth/password-reset/check?token="+token, nil); check["valid"] != true {
			t.Errorf("check a fresh reset link = %v, want valid", check)
		}
	}
	if check := f.must(http.StatusUnprocessableEntity, http.MethodGet, "/v1/auth/password-reset/check?token=not-a-reset-token", nil); check["valid"] != false || check["reason"] != "invalid_or_expired" {
		t.Errorf("check a made-up reset link = %v, want invalid_or_expired", check)
	}

	// Reset: the link is spent, every session is signed out, and only the
	// new password signs in.
	newPassword := "flow-e2e-new-pw-" + uniq()
	f.must(http.StatusOK, http.MethodPost, "/v1/auth/password-reset/confirm", map[string]any{"token": token, "password": newPassword})
	if check := f.must(http.StatusUnprocessableEntity, http.MethodGet, "/v1/auth/password-reset/check?token="+token, nil); check["valid"] != false {
		t.Errorf("check a used reset link = %v, want invalid", check)
	}
	f.must(http.StatusUnauthorized, http.MethodGet, "/v1/whoami", nil, withCookie(session))
	f.must(http.StatusUnauthorized, http.MethodPost, "/v1/auth/login", map[string]any{"email": f.email, "password": f.password})
	fresh := f.login(f.email, newPassword)
	if who := f.must(http.StatusOK, http.MethodGet, "/v1/whoami", nil, withCookie(fresh)); who["user_id"] != userID {
		t.Errorf("whoami after the reset = %v, want user %s", who, userID)
	}
}
