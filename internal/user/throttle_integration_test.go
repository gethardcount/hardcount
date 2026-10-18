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
	body, _ := json.Marshal(loginReq{Email: email, Password: password})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	req.RemoteAddr = ip + ":40000"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
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
