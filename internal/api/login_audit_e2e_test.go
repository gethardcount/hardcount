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
// last_login_at is set. The rolled-back case (a failed audit row leaves no
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
