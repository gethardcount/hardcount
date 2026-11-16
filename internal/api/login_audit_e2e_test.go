package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestE2E_Login_AuditRowAndLastSignIn: over the real router, each sign-in
// writes one login audit row on its own transaction: actor the user, in
// test mode, with the client address and the request id of that sign-in.
// last_login_at is set. The client address round-trips because
// throttleServer trusts the loopback proxy (TRUST_PROXY=127.0.0.1/32), so
// the X-Forwarded-For entry is the client address. The rolled-back case (a failed audit row leaves no
// session and does not clear the sign-in throttle) is in internal/user,
// login_integration_test.go.
func TestE2E_Login_AuditRowAndLastSignIn(t *testing.T) {
	db, ts := throttleServer(t)
	const address = "203.0.113.70"

	var requestIDs []string
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(map[string]any{"email": throttleOwner, "password": throttlePassword})
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/auth/login", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", address)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("sign-in %d: %v", i+1, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sign-in %d: status %d, want 200", i+1, resp.StatusCode)
		}
		id := resp.Header.Get("Hardcount-Request-Id")
		if id == "" {
			t.Fatalf("sign-in %d: no Hardcount-Request-Id header", i+1)
		}
		requestIDs = append(requestIDs, id)
	}

	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)

	var userID string
	var lastLoginSet bool
	if err := tx.QueryRowContext(t.Context(), `SELECT id, last_login_at IS NOT NULL FROM users WHERE email = $1`, throttleOwner).
		Scan(&userID, &lastLoginSet); err != nil {
		t.Fatal(err)
	}
	if !lastLoginSet {
		t.Error("last_login_at is unset after two sign-ins")
	}

	rows, err := tx.QueryContext(t.Context(), `
		SELECT actor_type, actor_id, resource_type, resource_id, livemode,
		       coalesce(ip_address, ''), coalesce(request_id, ''), metadata::text
		FROM audit_log WHERE action = 'login'
		ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var actorType, actorID, resType, resID, ip, reqID, meta string
		var live bool
		if err := rows.Scan(&actorType, &actorID, &resType, &resID, &live, &ip, &reqID, &meta); err != nil {
			t.Fatal(err)
		}
		if actorType != "user" || actorID != userID || resType != "user" || resID != userID {
			t.Errorf("login row: actor %s/%s resource %s/%s, want user/%s for both", actorType, actorID, resType, resID, userID)
		}
		if live {
			t.Error("login row is in live mode, want test mode")
		}
		if ip != address {
			t.Errorf("login row ip_address = %q, want %q", ip, address)
		}
		if meta != `{"livemode": false}` {
			t.Errorf("login row metadata = %s, want {\"livemode\": false}", meta)
		}
		got = append(got, reqID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != requestIDs[0] || got[1] != requestIDs[1] {
		t.Errorf("login rows' request ids = %v, want one row per sign-in: %v", got, requestIDs)
	}
}

// TestE2E_SignIn_ModeSwitch_SignOut_AuditRows: over the real router, a
// sign-in, a switch to live mode and a sign-out each write their audit row on
// their own transaction, in that order: actor the user, resource the user, in
// test mode, with the client address and the request id of that request. The
// rolled-back cases (a failed audit row leaves the session and its mode as
// they were) are in internal/user, logout_mode_integration_test.go.
func TestE2E_SignIn_ModeSwitch_SignOut_AuditRows(t *testing.T) {
	db, ts := throttleServer(t)
	const address = "203.0.113.71"

	// post sends one request from address and returns the answer's status,
	// its request id and the session cookie it set, if any.
	post := func(path string, cookie *http.Cookie, body any) (int, string, *http.Cookie) {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			enc, _ := json.Marshal(body)
			rdr = bytes.NewReader(enc)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, rdr)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Forwarded-For", address)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		id := resp.Header.Get("Hardcount-Request-Id")
		if id == "" {
			t.Fatalf("%s: no Hardcount-Request-Id header", path)
		}
		var set *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == "hardcount_session" && c.Value != "" {
				set = c
			}
		}
		return resp.StatusCode, id, set
	}

	status, loginID, cookie := post("/v1/auth/login", nil, map[string]any{"email": throttleOwner, "password": throttlePassword})
	if status != http.StatusOK || cookie == nil {
		t.Fatalf("sign-in: status %d, cookie %v; want 200 and a session cookie", status, cookie)
	}
	status, modeID, _ := post("/v1/auth/mode", cookie, map[string]any{"livemode": true})
	if status != http.StatusOK {
		t.Fatalf("switch to live: status %d, want 200", status)
	}
	status, logoutID, _ := post("/v1/auth/logout", cookie, nil)
	if status != http.StatusNoContent {
		t.Fatalf("sign-out: status %d, want 204", status)
	}

	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var userID string
	if err := tx.QueryRowContext(t.Context(), `SELECT id FROM users WHERE email = $1`, throttleOwner).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(t.Context(), `
		SELECT action, actor_type, actor_id, resource_type, resource_id, livemode,
		       coalesce(ip_address, ''), coalesce(request_id, '')
		FROM audit_log WHERE action IN ('login', 'mode_changed', 'logout')
		ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		action, actorType, actorID, resType, resID string
		live                                       bool
		ip, requestID                              string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.action, &r.actorType, &r.actorID, &r.resType, &r.resID, &r.live, &r.ip, &r.requestID); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []row{
		{"login", "user", userID, "user", userID, false, address, loginID},
		{"mode_changed", "user", userID, "user", userID, false, address, modeID},
		{"logout", "user", userID, "user", userID, false, address, logoutID},
	}
	if len(got) != len(want) {
		t.Fatalf("audit rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("audit row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
