package user

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The sign-in's transaction on real Postgres: the session, the end of the
// session the browser held, last_login_at and the login audit row commit
// together, and the transaction checks again that the verified password
// still holds. The removal of a member during a sign-in is in
// login_removal_integration_test.go; the route over the real router is in
// internal/api (TestE2E_Login_AuditRowAndLastSignIn).

const (
	loginITEmail       = "owner@login-it.test"
	loginITPassword    = "login-it-pw-123456"
	loginITNewPassword = "login-it-new-pw-34"
	loginITAddress     = "198.51.100.20"
)

type loginIT struct {
	db       *postgres.DB
	tenantID string
	userID   string
	sessSvc  *session.Service
}

func newLoginIT(t *testing.T) loginIT {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Login IT")
	u := CreateTestUser(t, db, loginITEmail, loginITPassword, tenantID, "owner")
	return loginIT{db: db, tenantID: tenantID, userID: u.ID, sessSvc: session.NewService(session.NewPostgresStore(db))}
}

// handler is the dashboard auth handler on store, the real sessions and
// auditor. throttle may be nil.
func (it loginIT) handler(store Store, auditor AuditTxWriter, throttle *LoginThrottle) http.Handler {
	svc := NewService(store, clock.Real(), it.sessSvc, auditor)
	h := NewHandler(svc, it.sessSvc, session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false)
	if throttle != nil {
		h.SetLoginThrottle(throttle)
	}
	return h.Routes()
}

// signIn posts the sign-in form from loginITAddress, with the session
// cookie when cookie is not "".
func signIn(h http.Handler, password, cookie string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(LoginReq{Email: loginITEmail, Password: password})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	req.RemoteAddr = loginITAddress + ":40000"
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: cookie})
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// sessionCookie is the session cookie value rr set, or "".
func sessionCookie(rr *httptest.ResponseRecorder) string {
	for _, c := range rr.Result().Cookies() {
		if c.Name == session.CookieName {
			return c.Value
		}
	}
	return ""
}

// errMessage is the error envelope's message.
func errMessage(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the error: %v; body %s", err, rr.Body.String())
	}
	return body.Error.Message
}

// sessionRows counts every session row of the user, ended or not, read
// outside row-level security.
func (it loginIT) sessionRows(t *testing.T) int {
	t.Helper()
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(t.Context(), `SELECT count(*) FROM dashboard_sessions WHERE user_id = $1`, it.userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (it loginIT) lastLoginAt(t *testing.T) *time.Time {
	t.Helper()
	u, err := NewPostgresStore(it.db).GetByID(t.Context(), it.userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.LastLoginAt
}

// afterLookup is the real store, running then once after GetByEmail
// returns: after the sign-in read the account and before its transaction.
type afterLookup struct {
	Store
	then func()
}

func (a *afterLookup) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := a.Store.GetByEmail(ctx, email)
	if a.then != nil {
		then := a.then
		a.then = nil
		then()
	}
	return u, err
}

// TestLogin_AFailedAuditRowLeavesNoSession: when the audit row cannot be
// written, the sign-in answers 500, sets no session cookie and leaves no
// session row, and last_login_at stays unset.
func TestLogin_AFailedAuditRowLeavesNoSession(t *testing.T) {
	it := newLoginIT(t)
	rr := signIn(it.handler(NewPostgresStore(it.db), failingAuditTx{}, nil), loginITPassword, "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("sign-in with a failing audit row: status %d, want 500; body %s", rr.Code, rr.Body.String())
	}
	if c := sessionCookie(rr); c != "" {
		t.Errorf("session cookie = %q, want none", c)
	}
	if n := it.sessionRows(t); n != 0 {
		t.Errorf("session rows = %d, want 0", n)
	}
	if at := it.lastLoginAt(t); at != nil {
		t.Errorf("last_login_at = %v, want unset", *at)
	}
}

// TestLogin_APasswordChangedWhileItRanIsRefused: a password reset commits
// after the sign-in read the account and before its transaction. The
// sign-in checked the old password against the old hash, and its
// transaction finds the new hash, so it gets the wrong-password 401 and no
// session.
func TestLogin_APasswordChangedWhileItRanIsRefused(t *testing.T) {
	it := newLoginIT(t)
	resetSvc := NewService(NewPostgresStore(it.db), clock.Real(), it.sessSvc, audit.NewLogger(it.db))
	token, _, _, err := resetSvc.IssueResetToken(t.Context(), loginITEmail)
	if err != nil || token == "" {
		t.Fatalf("issue reset token: token %q, err %v", token, err)
	}
	store := &afterLookup{Store: NewPostgresStore(it.db), then: func() {
		if _, err := resetSvc.ConfirmReset(t.Context(), token, loginITNewPassword); err != nil {
			t.Errorf("reset the password during the sign-in: %v", err)
		}
	}}

	badBefore := promtest.ToFloat64(loginAttempts.WithLabelValues("bad_credentials"))
	rr := signIn(it.handler(store, audit.NewLogger(it.db), nil), loginITPassword, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in whose password changed: status %d, want 401; body %s", rr.Code, rr.Body.String())
	}
	if msg := errMessage(t, rr); msg != "invalid email or password" {
		t.Errorf("message = %q, want %q", msg, "invalid email or password")
	}
	if d := promtest.ToFloat64(loginAttempts.WithLabelValues("bad_credentials")) - badBefore; d != 1 {
		t.Errorf("outcome bad_credentials rose by %v, want 1", d)
	}
	if n := it.sessionRows(t); n != 0 {
		t.Errorf("session rows = %d, want 0", n)
	}
	if at := it.lastLoginAt(t); at != nil {
		t.Errorf("last_login_at = %v, want unset", *at)
	}
}

// TestLogin_EndsTheSessionTheBrowserHeld: a sign-in that carries a live
// session cookie ends that session, and the new cookie works. An unknown
// cookie changes nothing and the sign-in goes ahead.
func TestLogin_EndsTheSessionTheBrowserHeld(t *testing.T) {
	it := newLoginIT(t)
	h := it.handler(NewPostgresStore(it.db), audit.NewLogger(it.db), nil)

	old, _, err := it.sessSvc.Issue(t.Context(), session.IssueInput{UserID: it.userID, TenantID: it.tenantID})
	if err != nil {
		t.Fatalf("issue the browser's session: %v", err)
	}
	rr := signIn(h, loginITPassword, old)
	if rr.Code != http.StatusOK {
		t.Fatalf("sign-in with a live cookie: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	if _, err := it.sessSvc.Resolve(t.Context(), old); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("resolve the session the browser held: err = %v, want session.ErrNotFound", err)
	}
	newer := sessionCookie(rr)
	if newer == "" || newer == old {
		t.Fatalf("new session cookie = %q, want a new value", newer)
	}
	if _, err := it.sessSvc.Resolve(t.Context(), newer); err != nil {
		t.Errorf("resolve the new session: %v, want it live", err)
	}

	rr = signIn(h, loginITPassword, "not-a-session-id")
	if rr.Code != http.StatusOK {
		t.Fatalf("sign-in with an unknown cookie: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	if _, err := it.sessSvc.Resolve(t.Context(), sessionCookie(rr)); err != nil {
		t.Errorf("resolve the session from the sign-in with an unknown cookie: %v, want it live", err)
	}
	if _, err := it.sessSvc.Resolve(t.Context(), newer); err != nil {
		t.Errorf("resolve the session of the earlier sign-in: %v, want it still live", err)
	}
}

// barrierSignIn is the real store whose SignIn waits until n calls have
// arrived, so their transactions start together.
type barrierSignIn struct {
	Store
	arrived *sync.WaitGroup
}

func (b barrierSignIn) SignIn(ctx context.Context, userID, verifiedHash string, at time.Time, then func(*sql.Tx, string) error) (string, error) {
	b.arrived.Done()
	b.arrived.Wait()
	return b.Store.SignIn(ctx, userID, verifiedHash, at, then)
}

// TestLogin_TwoAtOnceBothSignIn: two sign-ins of one user whose
// transactions start together both answer 200, with two sessions. The lock
// on the user's row makes the second wait for the first; it must not
// deadlock.
func TestLogin_TwoAtOnceBothSignIn(t *testing.T) {
	it := newLoginIT(t)
	var arrived sync.WaitGroup
	arrived.Add(2)
	h := it.handler(barrierSignIn{Store: NewPostgresStore(it.db), arrived: &arrived}, audit.NewLogger(it.db), nil)

	var results [2]*httptest.ResponseRecorder
	var done sync.WaitGroup
	for i := range results {
		done.Add(1)
		go func() {
			defer done.Done()
			results[i] = signIn(h, loginITPassword, "")
		}()
	}
	done.Wait()

	for i, rr := range results {
		if rr.Code != http.StatusOK {
			t.Fatalf("sign-in %d: status %d, want 200; body %s", i, rr.Code, rr.Body.String())
		}
	}
	a, b := sessionCookie(results[0]), sessionCookie(results[1])
	if a == "" || b == "" || a == b {
		t.Fatalf("session cookies = %q and %q, want two different values", a, b)
	}
	for _, raw := range []string{a, b} {
		if _, err := it.sessSvc.Resolve(t.Context(), raw); err != nil {
			t.Errorf("resolve a session: %v, want it live", err)
		}
	}
	if n := it.sessionRows(t); n != 2 {
		t.Errorf("session rows = %d, want 2", n)
	}
}

// TestLogin_ARolledBackSignInLeavesTheThrottleCounts: the throttle's counts
// are cleared only by a sign-in that committed. Four wrong passwords, then
// the right one whose audit row fails: that 500 is the 5th counted attempt
// and starts the 1-minute wait, so the next attempt is refused.
func TestLogin_ARolledBackSignInLeavesTheThrottleCounts(t *testing.T) {
	it := newLoginIT(t)
	throttle := itThrottle(t, it.db)
	failing := it.handler(NewPostgresStore(it.db), failingAuditTx{}, throttle)
	working := it.handler(NewPostgresStore(it.db), audit.NewLogger(it.db), throttle)

	for i := 1; i <= 4; i++ {
		if rr := signIn(working, "wrong-password-123", ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: status %d, want 401; body %s", i, rr.Code, rr.Body.String())
		}
	}
	if rr := signIn(failing, loginITPassword, ""); rr.Code != http.StatusInternalServerError {
		t.Fatalf("right password, failing audit row: status %d, want 500; body %s", rr.Code, rr.Body.String())
	}
	if rr := signIn(working, loginITPassword, ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("right password after the rolled-back sign-in: status %d, want 429; body %s", rr.Code, rr.Body.String())
	}
	if n := it.sessionRows(t); n != 0 {
		t.Errorf("session rows = %d, want 0", n)
	}
}
