package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestConfirmReset_EndsEverySession: over the real router, a password reset
// ends every session of the user in both modes, writes one
// password_reset_completed row with the user as actor, swaps the password,
// and spends the link.
func TestConfirmReset_EndsEverySession(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "confirm-reset-bootstrap")
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.confirm-reset.test")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, oldPassword, newPassword = "owner@confirm-reset.test", "confirm-reset-old-pw-12", "confirm-reset-new-pw-34"
	tenantID := flowStr(flowBootstrap(t, ts, "confirm-reset-bootstrap", email, oldPassword), "tenant", "id")

	testCookie := login(t, ts, email, oldPassword)
	liveCookie := login(t, ts, email, oldPassword)
	flowDo(t, ts, "POST", "/v1/auth/mode", flowReq{cookie: liveCookie, body: map[string]any{"livemode": true}}).want(t, 200, "switch the second session to live")
	userID := flowStr(flowDo(t, ts, "GET", "/v1/whoami", flowReq{cookie: testCookie}).want(t, 200, "whoami").body, "user_id")

	flowDo(t, ts, "POST", "/v1/auth/password-reset/request", flowReq{body: map[string]any{"email": email}}).want(t, 200, "request a reset")
	token := flowEmailLinkToken(t, db, "password_reset", "password_reset_url")
	flowDo(t, ts, "POST", "/v1/auth/password-reset/confirm", flowReq{body: map[string]any{
		"token": token, "password": newPassword,
	}}).want(t, 200, "set the new password")

	sessions := session.NewService(session.NewPostgresStore(db))
	for _, c := range []struct {
		mode  string
		value string
	}{{"test", testCookie.Value}, {"live", liveCookie.Value}} {
		if _, err := sessions.Resolve(context.Background(), c.value); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("resolve the %s-mode session after the reset: err = %v, want session.ErrNotFound", c.mode, err)
		}
	}

	type row struct {
		actorType, actorID, tenantID string
		livemode                     bool
		ip, requestID                sql.NullString
	}
	var rows []row
	func() {
		tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
		if err != nil {
			t.Fatal(err)
		}
		defer postgres.Rollback(tx)
		rs, err := tx.QueryContext(t.Context(), `
			SELECT actor_type, actor_id, tenant_id, livemode, ip_address::text, request_id
			FROM audit_log
			WHERE action = 'password_reset_completed' AND resource_type = 'user' AND resource_id = $1`, userID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.actorType, &r.actorID, &r.tenantID, &r.livemode, &r.ip, &r.requestID); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, r)
		}
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
	}()
	if len(rows) != 1 {
		t.Fatalf("password_reset_completed rows for %s = %d, want 1: %+v", userID, len(rows), rows)
	}
	if r := rows[0]; r.actorType != "user" || r.actorID != userID || r.tenantID != tenantID || r.livemode ||
		!r.ip.Valid || r.ip.String == "" || !r.requestID.Valid || r.requestID.String == "" {
		t.Errorf("audit row = %+v, want actor user/%s, tenant %s, test mode, the client IP and the request id", r, userID, tenantID)
	}

	flowDo(t, ts, "POST", "/v1/auth/login", flowReq{body: map[string]any{"email": email, "password": oldPassword}}).want(t, 401, "sign in with the old password")
	login(t, ts, email, newPassword)

	again := flowDo(t, ts, "POST", "/v1/auth/password-reset/confirm", flowReq{body: map[string]any{
		"token": token, "password": "confirm-reset-third-pw-56",
	}}).want(t, 422, "use the link twice")
	if got := flowStr(again.body, "error", "param"); got != "token" {
		t.Errorf("second confirm error param = %q, want token; body %s", got, again.raw)
	}
}
