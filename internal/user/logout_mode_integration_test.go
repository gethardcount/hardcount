package user

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
)

// The sign-out's and the mode switch's transactions on real Postgres: the
// session change and its audit row commit together, and a session that is
// unknown, ended or expired changes nothing. The routes over the real router
// are in internal/api (TestE2E_SignIn_ModeSwitch_SignOut_AuditRows).

// expire moves the session's expiry into the past.
func (it loginIT) expire(t *testing.T, raw string) {
	t.Helper()
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(t.Context(),
		`UPDATE dashboard_sessions SET expires_at = now() - interval '1 minute' WHERE id_hash = $1`, session.HashID(raw))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("expire: %d rows, want 1", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// authRow is one audit row of the user, in the fields these tests check.
type authRow struct {
	tenantID, actorType, actorID string
	livemode                     bool
	metadata                     string
}

// authRows is every audit row with action whose resource is the user,
// oldest first.
func (it loginIT) authRows(t *testing.T, action string) []authRow {
	t.Helper()
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(t.Context(), `
		SELECT tenant_id, actor_type, actor_id, livemode, metadata::text FROM audit_log
		WHERE action = $1 AND resource_type = 'user' AND resource_id = $2
		ORDER BY created_at, id`, action, it.userID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []authRow
	for rows.Next() {
		var r authRow
		if err := rows.Scan(&r.tenantID, &r.actorType, &r.actorID, &r.livemode, &r.metadata); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// postAuth posts body to path with the session cookie when cookie is not "".
func postAuth(h http.Handler, path, cookie, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: cookie})
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// clearsCookie reports whether rr clears the session cookie.
func clearsCookie(rr *httptest.ResponseRecorder) bool {
	for _, c := range rr.Result().Cookies() {
		if c.Name == session.CookieName && c.Value == "" && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// TestLogout_AFailedAuditRowLeavesTheSessionSignedIn: when the logout audit
// row cannot be written, the sign-out answers 500, does not clear the cookie,
// and the session still works.
func TestLogout_AFailedAuditRowLeavesTheSessionSignedIn(t *testing.T) {
	it := newLoginIT(t)
	raw := it.sessionIn(t, false)

	rr := postAuth(it.handler(NewPostgresStore(it.db), failingAuditTx{}, nil), "/logout", raw, "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("sign-out with a failing audit row: status %d, want 500; body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("Set-Cookie = %q, want none: the session still works", got)
	}
	if _, err := it.sessSvc.Resolve(t.Context(), raw); err != nil {
		t.Errorf("resolve the session: %v, want it still live", err)
	}
	if rows := it.authRows(t, "logout"); len(rows) != 0 {
		t.Errorf("logout rows = %+v, want none", rows)
	}
}

// TestLogout_EndsTheSessionAndWritesOneRow: a sign-out ends the session,
// clears the cookie and writes one logout row with the user as actor, in
// test mode even for a session in live mode. Signing out again with the same
// cookie clears it again and writes no second row.
func TestLogout_EndsTheSessionAndWritesOneRow(t *testing.T) {
	it := newLoginIT(t)
	raw := it.sessionIn(t, true)
	h := it.handler(NewPostgresStore(it.db), audit.NewLogger(it.db), nil)

	rr := postAuth(h, "/logout", raw, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("sign-out: status %d, want 204; body %s", rr.Code, rr.Body.String())
	}
	if !clearsCookie(rr) {
		t.Errorf("Set-Cookie = %q, want the session cookie cleared", rr.Header().Values("Set-Cookie"))
	}
	if _, err := it.sessSvc.Resolve(t.Context(), raw); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("resolve the session after the sign-out: err = %v, want session.ErrNotFound", err)
	}
	want := authRow{tenantID: it.tenantID, actorType: "user", actorID: it.userID, livemode: false, metadata: "{}"}
	if got := it.authRows(t, "logout"); len(got) != 1 || got[0] != want {
		t.Fatalf("logout rows = %+v, want [%+v]", got, want)
	}

	rr = postAuth(h, "/logout", raw, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second sign-out: status %d, want 204; body %s", rr.Code, rr.Body.String())
	}
	if !clearsCookie(rr) {
		t.Errorf("second sign-out: Set-Cookie = %q, want the session cookie cleared", rr.Header().Values("Set-Cookie"))
	}
	if got := it.authRows(t, "logout"); len(got) != 1 {
		t.Errorf("logout rows after the second sign-out = %+v, want the one row", got)
	}
}

// TestLogout_WithNoLiveSessionWritesNothing: a cookie for an expired or
// unknown session gets 204, the cookie cleared and no row. A request with no
// cookie gets 204 and no Set-Cookie at all, so a cross-site request cannot
// clear the browser's cookie.
func TestLogout_WithNoLiveSessionWritesNothing(t *testing.T) {
	it := newLoginIT(t)
	expired := it.sessionIn(t, false)
	it.expire(t, expired)
	h := it.handler(NewPostgresStore(it.db), audit.NewLogger(it.db), nil)

	for _, c := range []struct{ name, cookie string }{
		{"expired session", expired},
		{"unknown session", "not-a-session-id"},
	} {
		rr := postAuth(h, "/logout", c.cookie, "")
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s: status %d, want 204; body %s", c.name, rr.Code, rr.Body.String())
		}
		if !clearsCookie(rr) {
			t.Errorf("%s: Set-Cookie = %q, want the session cookie cleared", c.name, rr.Header().Values("Set-Cookie"))
		}
	}

	rr := postAuth(h, "/logout", "", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("no cookie: status %d, want 204; body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("no cookie: Set-Cookie = %q, want none", got)
	}
	if rows := it.authRows(t, "logout"); len(rows) != 0 {
		t.Errorf("logout rows = %+v, want none", rows)
	}
}

// TestSetMode_AFailedAuditRowLeavesTheMode: when the mode_changed audit row
// cannot be written, the switch answers 500 and the session keeps its mode.
func TestSetMode_AFailedAuditRowLeavesTheMode(t *testing.T) {
	it := newLoginIT(t)
	raw := it.sessionIn(t, false)

	rr := postAuth(it.handler(NewPostgresStore(it.db), failingAuditTx{}, nil), "/mode", raw, `{"livemode": true}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("switch with a failing audit row: status %d, want 500; body %s", rr.Code, rr.Body.String())
	}
	sess, err := it.sessSvc.Resolve(t.Context(), raw)
	if err != nil {
		t.Fatalf("resolve the session: %v, want it live", err)
	}
	if sess.Livemode {
		t.Error("session is in live mode, want test mode")
	}
	if rows := it.authRows(t, "mode_changed"); len(rows) != 0 {
		t.Errorf("mode_changed rows = %+v, want none", rows)
	}
}

// TestSetMode: a body without livemode is a 422 on param livemode and leaves
// the mode. {"livemode": true} switches the session to live mode and writes
// one mode_changed row, filed in test mode, with the new mode in its
// metadata. A switch to the mode the session is in answers 200 and writes no
// row. An expired, signed-out or unknown session gets 401 and no row.
func TestSetMode(t *testing.T) {
	it := newLoginIT(t)
	h := it.handler(NewPostgresStore(it.db), audit.NewLogger(it.db), nil)

	live := it.sessionIn(t, true)
	rr := postAuth(h, "/mode", live, `{}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("switch with {}: status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	if p := errParam(t, rr); p != "livemode" {
		t.Errorf("param = %q, want livemode", p)
	}
	if sess, err := it.sessSvc.Resolve(t.Context(), live); err != nil || !sess.Livemode {
		t.Errorf("session after {}: livemode %v, err %v; want live mode, no error", sess.Livemode, err)
	}

	raw := it.sessionIn(t, false)
	rr = postAuth(h, "/mode", raw, `{"livemode": true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("switch to live: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"livemode":true}` {
		t.Errorf("body = %s, want {\"livemode\":true}", got)
	}
	if sess, err := it.sessSvc.Resolve(t.Context(), raw); err != nil || !sess.Livemode {
		t.Errorf("session after the switch: livemode %v, err %v; want live mode, no error", sess.Livemode, err)
	}
	want := authRow{tenantID: it.tenantID, actorType: "user", actorID: it.userID, livemode: false, metadata: `{"livemode": true}`}
	if got := it.authRows(t, "mode_changed"); len(got) != 1 || got[0] != want {
		t.Fatalf("mode_changed rows = %+v, want [%+v]", got, want)
	}

	// The session is in live mode now: the same switch again changes nothing.
	rr = postAuth(h, "/mode", raw, `{"livemode": true}`)
	if got := strings.TrimSpace(rr.Body.String()); rr.Code != http.StatusOK || got != `{"livemode":true}` {
		t.Fatalf("switch to the mode it is in: status %d, body %s; want 200 {\"livemode\":true}", rr.Code, got)
	}
	if got := it.authRows(t, "mode_changed"); len(got) != 1 {
		t.Fatalf("mode_changed rows after a switch that changed nothing = %+v, want the one row", got)
	}

	expired := it.sessionIn(t, false)
	it.expire(t, expired)
	signedOut := it.sessionIn(t, false)
	if rr := postAuth(h, "/logout", signedOut, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("sign-out: status %d, want 204", rr.Code)
	}
	for _, c := range []struct{ name, cookie string }{
		{"expired session", expired},
		{"signed-out session", signedOut},
		{"unknown session", "not-a-session-id"},
	} {
		rr := postAuth(h, "/mode", c.cookie, `{"livemode": true}`)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401; body %s", c.name, rr.Code, rr.Body.String())
		}
	}
	if got := it.authRows(t, "mode_changed"); len(got) != 1 {
		t.Errorf("mode_changed rows after the refused switches = %+v, want the one row", got)
	}
}
