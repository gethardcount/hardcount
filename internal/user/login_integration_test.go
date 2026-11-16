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
	return it.handlerWith(store, it.sessSvc, auditor, throttle)
}

// handlerWith is handler with sessions for the steps inside the sign-in's
// transaction.
func (it loginIT) handlerWith(store Store, sessions SessionWriter, auditor AuditTxWriter, throttle *LoginThrottle) http.Handler {
	svc := NewService(store, clock.Real(), sessions, auditor)
	h := NewHandler(svc, session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false)
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

// liveSession issues a session for the user: the one the browser holds.
func (it loginIT) liveSession(t *testing.T) string {
	t.Helper()
	return issueTestSession(t, it.db, it.sessSvc, session.IssueInput{UserID: it.userID, TenantID: it.tenantID})
}

// loginMetadata is the metadata of each login audit row, oldest first.
func (it loginIT) loginMetadata(t *testing.T) []string {
	t.Helper()
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(t.Context(), `
		SELECT metadata::text FROM audit_log
		WHERE action = 'login' AND resource_id = $1
		ORDER BY created_at, id`, it.userID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
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
// written, the sign-in answers 500, sets no session cookie and creates no
// session, the session the browser held still works, and last_login_at
// stays unset.
func TestLogin_AFailedAuditRowLeavesNoSession(t *testing.T) {
	it := newLoginIT(t)
	old := it.liveSession(t)
	rr := signIn(it.handler(NewPostgresStore(it.db), failingAuditTx{}, nil), loginITPassword, old)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("sign-in with a failing audit row: status %d, want 500; body %s", rr.Code, rr.Body.String())
	}
	it.assertFailedSignIn(t, rr, old)
	if at := it.lastLoginAt(t); at != nil {
		t.Errorf("last_login_at = %v, want unset", *at)
	}
}

// assertFailedSignIn checks what a refused or failed sign-in leaves: no
// session cookie, no new session row, and the browser's session old still
// live.
func (it loginIT) assertFailedSignIn(t *testing.T, rr *httptest.ResponseRecorder, old string) {
	t.Helper()
	if c := sessionCookie(rr); c != "" {
		t.Errorf("session cookie = %q, want none", c)
	}
	if n := it.sessionRows(t); n != 1 {
		t.Errorf("session rows = %d, want 1 (the browser's own)", n)
	}
	if _, err := it.sessSvc.Resolve(t.Context(), old); err != nil {
		t.Errorf("resolve the session the browser held: %v, want it still live", err)
	}
}

// endThenFailTx ends the browser's session on the transaction, then fails.
type endThenFailTx struct{ *session.Service }

func (e endThenFailTx) EndTx(ctx context.Context, tx *sql.Tx, rawID string) (bool, error) {
	if _, err := e.Service.EndTx(ctx, tx, rawID); err != nil {
		return false, err
	}
	return false, errors.New("ending the session failed")
}

// issueThenFailTx inserts the new session on the transaction, then fails.
type issueThenFailTx struct{ *session.Service }

func (i issueThenFailTx) IssueTx(ctx context.Context, tx *sql.Tx, in session.IssueInput) (string, session.Session, error) {
	if _, _, err := i.Service.IssueTx(ctx, tx, in); err != nil {
		return "", session.Session{}, err
	}
	return "", session.Session{}, errors.New("creating the session failed")
}

// TestLogin_AFailedSignInKeepsTheBrowsersSession: a sign-in that is refused
// or fails leaves the session the browser held as it was. A wrong password
// is refused before the transaction; a failure inside it rolls back the end
// of the old session and the insert of the new one.
func TestLogin_AFailedSignInKeepsTheBrowsersSession(t *testing.T) {
	cases := []struct {
		name     string
		password string
		sessions func(*session.Service) SessionWriter
		status   int
	}{
		{"wrong password", "wrong-password-123", func(s *session.Service) SessionWriter { return s }, http.StatusUnauthorized},
		{"ending the old session fails", loginITPassword, func(s *session.Service) SessionWriter { return endThenFailTx{s} }, http.StatusInternalServerError},
		{"creating the new session fails", loginITPassword, func(s *session.Service) SessionWriter { return issueThenFailTx{s} }, http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := newLoginIT(t)
			old := it.liveSession(t)
			h := it.handlerWith(NewPostgresStore(it.db), c.sessions(it.sessSvc), audit.NewLogger(it.db), nil)
			rr := signIn(h, c.password, old)
			if rr.Code != c.status {
				t.Fatalf("status %d, want %d; body %s", rr.Code, c.status, rr.Body.String())
			}
			it.assertFailedSignIn(t, rr, old)
		})
	}
}

// holdEndAll is the real session service whose EndAllForUserTx, inside a
// password reset's transaction (after the new hash is written), waits at
// gate before it ends the sessions.
type holdEndAll struct {
	*session.Service
	gate *Gate
}

func (h holdEndAll) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	h.gate.Hold()
	return h.Service.EndAllForUserTx(ctx, tx, userID)
}

// TestLogin_AResetInFlightIsWaitedFor: a password reset has written the new
// hash and not yet committed when a sign-in with the old password starts its
// transaction. The sign-in's read of the hash waits on the user's row; once
// the reset commits it sees the new hash and answers the wrong-password 401,
// with no session.
func TestLogin_AResetInFlightIsWaitedFor(t *testing.T) {
	it := newLoginIT(t)
	gate := NewGate(t)
	resetSvc := NewService(NewPostgresStore(it.db), clock.Real(), holdEndAll{it.sessSvc, gate}, audit.NewLogger(it.db))
	token, _, _, err := resetSvc.IssueResetToken(t.Context(), loginITEmail)
	if err != nil || token == "" {
		t.Fatalf("issue reset token: token %q, err %v", token, err)
	}

	resetDone := make(chan error, 1)
	go func() {
		_, err := resetSvc.ConfirmReset(context.Background(), token, loginITNewPassword)
		resetDone <- err
	}()
	gate.WaitReached(t)

	h := it.handler(NewPostgresStore(it.db), audit.NewLogger(it.db), nil)
	signInDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { signInDone <- signIn(h, loginITPassword, "") }()

	WaitForLockWait(t, it.db, "%users%")
	gate.Open()
	if err := Receive(t, resetDone, "the reset"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	rr := Receive(t, signInDone, "the sign-in")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("sign-in with the old password during a reset: status %d, want 401; body %s", rr.Code, rr.Body.String())
	}
	if msg := errMessage(t, rr); msg != "invalid email or password" {
		t.Errorf("message = %q, want %q", msg, "invalid email or password")
	}
	if n := it.sessionRows(t); n != 0 {
		t.Errorf("session rows = %d, want 0", n)
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
// cookie changes nothing and the sign-in goes ahead. The login audit row
// carries previous_session_ended only for the sign-in that ended one.
func TestLogin_EndsTheSessionTheBrowserHeld(t *testing.T) {
	it := newLoginIT(t)
	h := it.handler(NewPostgresStore(it.db), audit.NewLogger(it.db), nil)

	old := it.liveSession(t)
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

	if rr = signIn(h, loginITPassword, ""); rr.Code != http.StatusOK {
		t.Fatalf("sign-in with no cookie: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	// Only the sign-in that ended a session says so in its audit row.
	want := []string{
		`{"livemode": false, "previous_session_ended": true}`,
		`{"livemode": false}`,
		`{"livemode": false}`,
	}
	if got := it.loginMetadata(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("login rows' metadata = %q, want %q", got, want)
	}
}

// barrierSignIn is the real store whose SignIn waits until every call
// counted in arrived has arrived, so their transactions start together. It
// gives up after testWait, so one failed request cannot block the other.
type barrierSignIn struct {
	Store
	arrived *sync.WaitGroup
	all     chan struct{} // closed once every call has arrived
}

func newBarrierSignIn(store Store, n int) barrierSignIn {
	b := barrierSignIn{Store: store, arrived: &sync.WaitGroup{}, all: make(chan struct{})}
	b.arrived.Add(n)
	go func() {
		b.arrived.Wait()
		close(b.all)
	}()
	return b
}

func (b barrierSignIn) SignIn(ctx context.Context, userID, verifiedHash string, at time.Time, then func(*sql.Tx, string) error) (string, error) {
	b.arrived.Done()
	select {
	case <-b.all:
	case <-time.After(testWait):
		return "", errors.New("barrierSignIn: the other sign-in never arrived")
	}
	return b.Store.SignIn(ctx, userID, verifiedHash, at, then)
}

// TestLogin_TwoAtOnceBothSignIn: two sign-ins of one user whose
// transactions start together both answer 200, with two sessions. The lock
// on the user's row makes the second wait for the first; it must not
// deadlock.
func TestLogin_TwoAtOnceBothSignIn(t *testing.T) {
	it := newLoginIT(t)
	h := it.handler(newBarrierSignIn(NewPostgresStore(it.db), 2), audit.NewLogger(it.db), nil)

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
