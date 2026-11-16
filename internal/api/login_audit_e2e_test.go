package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestE2E_SignIn_ModeSwitch_SignOut_AuditRows: over the real router, a
// sign-in, a switch to live mode and a sign-out each write their audit row on
// their own transaction, in that order: actor the user, resource the user, in
// test mode, with the client address and the request id of that request. The
// session row records the same client address. The client address
// round-trips because throttleServer trusts the loopback proxy, so the
// X-Forwarded-For entry is the client address. The rolled-back cases are in
// internal/user.
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
	var sessionIP string
	if err := tx.QueryRowContext(t.Context(), `SELECT coalesce(ip, '') FROM dashboard_sessions WHERE user_id = $1`, userID).Scan(&sessionIP); err != nil {
		t.Fatal(err)
	}
	if sessionIP != address {
		t.Errorf("session row ip = %q, want the client address %q", sessionIP, address)
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
