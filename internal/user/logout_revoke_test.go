package user

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/session"
)

// fakeSessions lets the test do the thing the concrete *session.Service made
// impossible: fail a Revoke. The phantom-logout bug survived a full end-to-end
// audit of this subsystem precisely because no test could reach that branch.
type fakeSessions struct {
	sess      session.Session
	resolveEr error
	revokeEr  error
	revoked   bool
}

func (f *fakeSessions) Issue(context.Context, session.IssueInput) (string, session.Session, error) {
	return "", session.Session{}, nil
}
func (f *fakeSessions) Resolve(context.Context, string) (session.Session, error) {
	return f.sess, f.resolveEr
}
func (f *fakeSessions) Revoke(context.Context, string) error {
	if f.revokeEr != nil {
		return f.revokeEr
	}
	f.revoked = true
	return nil
}
func (f *fakeSessions) RevokeAllForUser(context.Context, string) error { return nil }
func (f *fakeSessions) SetLivemode(context.Context, string, bool) error {
	return nil
}

type capturedRow struct {
	action   string
	tenantID string
}

type rowRecorder struct{ rows []capturedRow }

func (r *rowRecorder) Log(_ context.Context, tenantID, action, _, _, _ string, _ map[string]any) error {
	r.rows = append(r.rows, capturedRow{action: action, tenantID: tenantID})
	return nil
}

func logoutRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "raw-token"})
	return req
}

// TestLogout_RevokeFailure_WritesNoRowAndDoesNotReport204 is the regression lock
// for the phantom logout.
//
// The old handler wrote the audit row FIRST, then revoked, then — if the revoke
// failed — logged to stderr and returned 204 anyway. The result was the worst
// possible pair:
//
//   - the append-only log permanently asserted a logout that DID NOT HAPPEN, and
//   - the user was told they were signed out while their session stayed LIVE on
//     the server, which is the exact failure they were defending against when they
//     clicked Log out on a machine they no longer trust.
//
// A row is evidence of a state change. If the state did not change, the row is a
// lie — and this one is a lie about a security boundary.
func TestLogout_RevokeFailure_WritesNoRowAndDoesNotReport204(t *testing.T) {
	sessions := &fakeSessions{
		sess:     session.Session{UserID: "hc_usr_1", TenantID: "hc_ten_1"},
		revokeEr: errors.New("postgres is down"),
	}
	rec := &rowRecorder{}

	h := NewHandler(nil, sessions, session.CookieConfig{}, nil, "", false)
	h.SetAuditLogger(rec)

	w := httptest.NewRecorder()
	h.logout(w, logoutRequest())

	if w.Code == http.StatusNoContent {
		t.Error("logout reported 204 while the revoke FAILED — the session is still live and the caller has been told it is not")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (a logout that did not happen must not report success)", w.Code)
	}
	if len(rec.rows) != 0 {
		t.Errorf("wrote %d audit row(s) for a logout that never happened: %+v — audit_log is append-only, so this row could never be retracted", len(rec.rows), rec.rows)
	}
	if sessions.revoked {
		t.Error("fixture is wrong: Revoke reported success")
	}
}

// The happy path still records exactly one row — and only on the far side of a
// revoke that actually worked.
func TestLogout_Success_RevokesThenRecordsExactlyOneRow(t *testing.T) {
	sessions := &fakeSessions{sess: session.Session{UserID: "hc_usr_1", TenantID: "hc_ten_1"}}
	rec := &rowRecorder{}

	h := NewHandler(nil, sessions, session.CookieConfig{}, nil, "", false)
	h.SetAuditLogger(rec)

	w := httptest.NewRecorder()
	h.logout(w, logoutRequest())

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if !sessions.revoked {
		t.Fatal("the session was not revoked")
	}
	if len(rec.rows) != 1 || rec.rows[0].action != "logout" {
		t.Fatalf("audit rows = %+v, want exactly one 'logout'", rec.rows)
	}
	if rec.rows[0].tenantID != "hc_ten_1" {
		t.Errorf("tenant = %q, want hc_ten_1 (identity must be resolved BEFORE the revoke — after it the token names nobody)", rec.rows[0].tenantID)
	}
}

// TestLogout_NoCookie_DoesNotClearTheCookie is the regression lock for the
// forced-logout CSRF.
//
// A cross-site POST is not a hypothetical shape for this branch — it is the
// ONLY shape that reaches it in a browser. SameSite=Lax withholds hardcount_session
// from every cross-site request, so an attacker's form POST arrives here looking
// exactly like "the user has no cookie". The old code answered it with
// ClearCookie:
//
//	Set-Cookie: hardcount_session=; Path=/; Max-Age=0
//
// which the browser honours. So any website could log any operator out of the
// dashboard — no click needed, an auto-submitting form does it on page load —
// while the session it could not see stayed LIVE server-side for the full 7-day
// TTL. Verified end-to-end in a real Chrome before this fix: one cross-site POST
// bounced the dashboard to /login, and curl with the original cookie still
// answered 200.
//
// The invariant: a request that could not be authenticated must not mutate the
// caller's cookie jar. "No cookie arrived" ≠ "the browser holds no cookie".
func TestLogout_NoCookie_DoesNotClearTheCookie(t *testing.T) {
	sessions := &fakeSessions{}
	rec := &rowRecorder{}

	h := NewHandler(nil, sessions, session.CookieConfig{}, nil, "", false)
	h.SetAuditLogger(rec)

	w := httptest.NewRecorder()
	// No AddCookie: this is the cross-site POST as the server sees it.
	h.logout(w, httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil))

	if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("responded with Set-Cookie %q to a request that carried NO cookie — "+
			"a cross-site POST lands here (SameSite=Lax hides the cookie), so this "+
			"header force-logs-out any operator who visits a hostile page", got)
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (no cookie is not an error)", w.Code)
	}
	if sessions.revoked {
		t.Error("revoked a session for a request with no cookie")
	}
	if len(rec.rows) != 0 {
		t.Errorf("wrote %d audit row(s) for a logout that revoked nothing: %+v", len(rec.rows), rec.rows)
	}
}

// The counterpart the fix must NOT break: a real sign-out still clears the
// cookie, because there the server saw the cookie, authenticated it, and
// revoked the session first.
func TestLogout_Success_StillClearsTheCookie(t *testing.T) {
	sessions := &fakeSessions{sess: session.Session{UserID: "hc_usr_1", TenantID: "hc_ten_1"}}

	h := NewHandler(nil, sessions, session.CookieConfig{}, nil, "", false)
	h.SetAuditLogger(&rowRecorder{})

	w := httptest.NewRecorder()
	h.logout(w, logoutRequest())

	if !sessions.revoked {
		t.Fatal("fixture: the session was not revoked")
	}
	got := w.Header().Get("Set-Cookie")
	if got == "" {
		t.Fatal("a real sign-out did NOT clear the cookie — the browser would keep sending a revoked token")
	}
	if !strings.Contains(got, session.CookieName+"=;") {
		t.Errorf("Set-Cookie = %q, want it to blank %s", got, session.CookieName)
	}
}

// A stale cookie revokes nothing, so it records nothing — and says so, rather
// than leaving the coverage detector to report a 2xx mutation with no row.
func TestLogout_StaleCookie_RecordsNothing(t *testing.T) {
	sessions := &fakeSessions{resolveEr: errors.New("no such session")}
	rec := &rowRecorder{}

	h := NewHandler(nil, sessions, session.CookieConfig{}, nil, "", false)
	h.SetAuditLogger(rec)

	w := httptest.NewRecorder()
	h.logout(w, logoutRequest())

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (a stale cookie is not an error)", w.Code)
	}
	if len(rec.rows) != 0 {
		t.Errorf("wrote %d row(s) for a logout that revoked no session: %+v", len(rec.rows), rec.rows)
	}
}
