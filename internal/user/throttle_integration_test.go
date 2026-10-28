package user

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/session"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Real-Postgres tests of the sign-in throttle inside the handler. The e2e
// journeys in internal/api cover the behaviour over HTTP; these count what
// the e2e tests cannot see: how many attempts reached the account lookup,
// and with it bcrypt (the lookup comes first, and the bcrypt for a real or
// an unknown email only after it).

const (
	itEmail    = "owner@throttle-it.test"
	itPassword = "throttle-it-pw-1234"
)

// lookupCounter is the real store, counting account lookups.
type lookupCounter struct {
	Store
	lookups atomic.Int64
}

func (c *lookupCounter) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	c.lookups.Add(1)
	return c.Store.GetByEmail(ctx, email)
}

func throttleIT(t *testing.T) (*postgres.DB, *lookupCounter) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Throttle IT")
	store := NewPostgresStore(db)
	if _, err := NewService(store, clock.Real()).CreateUser(context.Background(), itEmail, itPassword, tenantID, "owner"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return db, &lookupCounter{Store: store}
}

// itHandler is the dashboard auth handler on the real store. throttle may be
// nil: the handler then checks every password, as it did before.
func itHandler(store Store, throttle *LoginThrottle) http.Handler {
	h := NewHandler(NewService(store, clock.Real()), session.NewService(&recordingSessionStore{}),
		session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false)
	if throttle != nil {
		h.SetLoginThrottle(throttle)
	}
	return h.Routes()
}

func itThrottle(t *testing.T, db *postgres.DB) *LoginThrottle {
	t.Helper()
	key, err := ThrottleKey(strings.Repeat("5a", 32))
	if err != nil {
		t.Fatal(err)
	}
	return NewLoginThrottle(db, key, session.CookieConfig{Path: "/"})
}

func itSignIn(h http.Handler, ip, email, password string) int {
	return itSignInWith(h, ip, email, password, nil).Code
}

// itSignInWith signs in from ip, sending the hardcount_device cookie when
// browser is not nil.
func itSignInWith(h http.Handler, ip, email, password string, browser *http.Cookie) *httptest.ResponseRecorder {
	body, _ := json.Marshal(LoginReq{Email: email, Password: password})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	req.RemoteAddr = ip + ":40000"
	if browser != nil {
		req.AddCookie(browser)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// deviceCookie is the hardcount_device cookie a sign-in set.
func deviceCookie(t *testing.T, rr *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("sign-in: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == DeviceCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("the sign-in set no hardcount_device cookie")
	return nil
}

// TestLoginThrottle_RefusalSkipsThePasswordCheck is control against
// treatment on the same database. Without the throttle, six wrong passwords
// make six lookups and six 401s. With it, the 6th attempt is refused before
// the lookup; so is the right password during the wait, and so is an email
// with no account, which therefore costs no dummy bcrypt either. That is
// why a refusal has the same timing for real and unknown emails.
func TestLoginThrottle_RefusalSkipsThePasswordCheck(t *testing.T) {
	db, store := throttleIT(t)

	// Control: no throttle.
	control := itHandler(store, nil)
	for i := 1; i <= 6; i++ {
		if code := itSignIn(control, "198.51.100.1", itEmail, "wrong-password-123"); code != http.StatusUnauthorized {
			t.Fatalf("control: attempt %d = %d, want 401", i, code)
		}
	}
	if n := store.lookups.Load(); n != 6 {
		t.Fatalf("control: %d lookups for 6 attempts, want 6", n)
	}

	// Treatment: the same attempts with the throttle wired.
	store.lookups.Store(0)
	h := itHandler(store, itThrottle(t, db))
	const ip = "198.51.100.2"
	for i := 1; i <= 5; i++ {
		if code := itSignIn(h, ip, itEmail, "wrong-password-123"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	if code := itSignIn(h, ip, itEmail, "wrong-password-123"); code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt = %d, want 429", code)
	}
	if code := itSignIn(h, ip, itEmail, itPassword); code != http.StatusTooManyRequests {
		t.Fatalf("right password during the wait = %d, want 429", code)
	}
	if n := store.lookups.Load(); n != 5 {
		t.Fatalf("%d lookups, want 5: a refused attempt reached the account lookup and bcrypt", n)
	}

	// An email with no account: five lookups (each with a dummy bcrypt),
	// then refusals that make none.
	store.lookups.Store(0)
	for i := 1; i <= 5; i++ {
		itSignIn(h, ip, "nobody@throttle-it.test", "wrong-password-123")
	}
	for i := 0; i < 3; i++ {
		if code := itSignIn(h, ip, "nobody@throttle-it.test", "wrong-password-123"); code != http.StatusTooManyRequests {
			t.Fatalf("unknown email, refused attempt = %d, want 429", code)
		}
	}
	if n := store.lookups.Load(); n != 5 {
		t.Fatalf("unknown email: %d lookups, want 5", n)
	}
}

// TestLoginThrottle_ParallelAttemptsAreCountedExactly: twenty wrong
// passwords at once for one address and email. The count is taken under a
// row lock before the password check, so exactly five reach it; a
// check-then-count throttle lets a burst through.
func TestLoginThrottle_ParallelAttemptsAreCountedExactly(t *testing.T) {
	db, store := throttleIT(t)
	h := itHandler(store, itThrottle(t, db))

	var wg sync.WaitGroup
	var unauthorized, throttled, other atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch itSignIn(h, "198.51.100.3", itEmail, "wrong-password-123") {
			case http.StatusUnauthorized:
				unauthorized.Add(1)
			case http.StatusTooManyRequests:
				throttled.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	wg.Wait()
	if unauthorized.Load() != 5 || throttled.Load() != 15 || other.Load() != 0 || store.lookups.Load() != 5 {
		t.Fatalf("20 parallel attempts: %d checked, %d refused, %d other, %d lookups; want 5, 15, 0, 5",
			unauthorized.Load(), throttled.Load(), other.Load(), store.lookups.Load())
	}
}

// TestLoginThrottle_Cleanup keeps what is still read and deletes the rest.
func TestLoginThrottle_Cleanup(t *testing.T) {
	db, _ := throttleIT(t)
	th := itThrottle(t, db)
	ctx := context.Background()

	rows := []struct {
		name, sql string
		kept      bool
	}{
		{"recent attempt", `now() - interval '1 hour', NULL, NULL`, true},
		{"a day old", `now() - interval '25 hours', NULL, NULL`, false},
		{"a day old, wait over", `now() - interval '25 hours', now() - interval '24 hours', NULL`, false},
		{"a day old, wait still running", `now() - interval '25 hours', now() + interval '5 minutes', NULL`, true},
		{"browser signed in 100 days ago", `now() - interval '100 days', NULL, now() - interval '100 days'`, true},
		{"browser signed in 200 days ago", `now() - interval '200 days', NULL, now() - interval '200 days'`, false},
	}
	for i, r := range rows {
		if _, err := db.Pool.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO login_throttle (subject, attempts, last_attempt_at, blocked_until, signed_in_at)
			VALUES ($1, 3, %s)`, r.sql), fmt.Sprintf("%064x", i+1)); err != nil {
			t.Fatalf("insert %s: %v", r.name, err)
		}
	}
	n, err := th.Cleanup(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 3 {
		t.Errorf("cleanup deleted %d rows, want 3", n)
	}
	for i, r := range rows {
		var exists bool
		if err := db.Pool.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM login_throttle WHERE subject = $1)`,
			fmt.Sprintf("%064x", i+1)).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != r.kept {
			t.Errorf("%s: kept = %v, want %v", r.name, exists, r.kept)
		}
	}
}

// TestLoginThrottle_BrowserIsKnownPerEmail: a hardcount_device cookie makes a
// browser known only for the email it signed in to. A guesser who signs in
// to an account of their own and keeps that cookie is a new browser for
// every other email, so the email's wait holds them. Control: the owner's
// own cookie gets through the same wait.
func TestLoginThrottle_BrowserIsKnownPerEmail(t *testing.T) {
	db, store := throttleIT(t)
	const guesser, guesserPassword = "guesser@throttle-it.test", "guesser-pw-5678"
	tenantID := testutil.CreateTestTenant(t, db, "Throttle IT guesser")
	if _, err := NewService(store, clock.Real()).CreateUser(context.Background(), guesser, guesserPassword, tenantID, "owner"); err != nil {
		t.Fatalf("create the guesser's user: %v", err)
	}
	h := itHandler(store, itThrottle(t, db))

	owner := deviceCookie(t, itSignInWith(h, "203.0.113.1", itEmail, itPassword, nil))
	theirs := deviceCookie(t, itSignInWith(h, "198.51.100.50", guesser, guesserPassword, nil))

	// Ten wrong passwords for the owner's email, from ten addresses, start
	// the email's wait.
	for i := 1; i <= 10; i++ {
		if code := itSignIn(h, fmt.Sprintf("192.0.2.%d", i), itEmail, "wrong-password-123"); code != http.StatusUnauthorized {
			t.Fatalf("wrong password %d = %d, want 401", i, code)
		}
	}
	if rr := itSignInWith(h, "198.51.100.50", itEmail, "wrong-password-123", theirs); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a guess on the owner's email with the cookie from the guesser's own sign-in = %d, want 429", rr.Code)
	}
	if rr := itSignInWith(h, "203.0.113.2", itEmail, itPassword, owner); rr.Code != http.StatusOK {
		t.Fatalf("control: the owner's browser during the wait = %d, want 200", rr.Code)
	}
}

// TestLoginThrottle_RowDeletedDuringAnAttempt: an attempt waits on a
// count's row lock while another transaction deletes the row, as a sign-in
// clearing the count or the cleanup does. The attempt adds the row again
// and is counted. When the row was added and then locked in two statements,
// the lock found no row and the attempt failed, which sign-in answers 503.
func TestLoginThrottle_RowDeletedDuringAnAttempt(t *testing.T) {
	db, _ := throttleIT(t)
	th := itThrottle(t, db)
	ctx := context.Background()
	attempt := func() (ThrottleVerdict, error) {
		r := httptest.NewRequest(http.MethodPost, "/login", nil)
		r.RemoteAddr = "198.51.100.90:40000"
		return th.Reserve(r, itEmail)
	}
	if v, err := attempt(); err != nil || !v.Allowed {
		t.Fatalf("first attempt: allowed %v, error %v", v.Allowed, err)
	}

	// Another transaction locks every row, then deletes them.
	tx, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT subject FROM login_throttle FOR UPDATE`); err != nil {
		t.Fatalf("lock the rows: %v", err)
	}

	type result struct {
		v   ThrottleVerdict
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := attempt()
		done <- result{v, err}
	}()

	// Delete only once the attempt is waiting on the lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.Pool.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second attempt never waited on the row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_throttle`); err != nil {
		t.Fatalf("delete the rows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("an attempt whose row was deleted while it waited failed: %v; want it counted", res.err)
		}
		if !res.v.Allowed {
			t.Fatal("an attempt whose row was deleted while it waited was refused; want it counted as a first attempt")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second attempt did not finish after the delete")
	}
}
