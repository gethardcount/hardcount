package user

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/session"
)

// TestDecide walks the throttle rule one attempt at a time: when a wait
// starts, how long it is, that a refused attempt changes nothing, and that
// a day-old count starts over.
func TestDecide(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }

	cases := []struct {
		name      string
		row       throttleRow
		threshold int
		now       time.Time
		refused   bool
		retry     time.Duration
		attempts  int
		wait      time.Duration // 0 = no wait starts
	}{
		{"first attempt", throttleRow{0, t0, nil}, 5, t0, false, 0, 1, 0},
		{"4th attempt, below the threshold", throttleRow{3, t0, nil}, 5, t0, false, 0, 4, 0},
		{"5th attempt goes on and starts 1 minute", throttleRow{4, t0, nil}, 5, t0, false, 0, 5, time.Minute},
		{"6th attempt starts 2 minutes", throttleRow{5, t0, nil}, 5, t0.Add(time.Minute), false, 0, 6, 2 * time.Minute},
		{"7th starts 4 minutes", throttleRow{6, t0, nil}, 5, t0, false, 0, 7, 4 * time.Minute},
		{"8th starts 8 minutes", throttleRow{7, t0, nil}, 5, t0, false, 0, 8, 8 * time.Minute},
		{"9th is capped at 15 minutes", throttleRow{8, t0, nil}, 5, t0, false, 0, 9, 15 * time.Minute},
		{"the 1000th is still 15 minutes", throttleRow{999, t0, nil}, 5, t0, false, 0, 1000, 15 * time.Minute},
		{"the email threshold: the 9th attempt starts nothing", throttleRow{8, t0, nil}, 10, t0, false, 0, 9, 0},
		{"the email threshold: the 10th starts 1 minute", throttleRow{9, t0, nil}, 10, t0, false, 0, 10, time.Minute},
		{"during a wait: refused, with the time left", throttleRow{5, t0, at(time.Minute)}, 5, t0.Add(20 * time.Second), true, 40 * time.Second, 0, 0},
		{"the moment a wait ends: counted", throttleRow{5, t0, at(time.Minute)}, 5, t0.Add(time.Minute), false, 0, 6, 2 * time.Minute},
		{"a count 24 hours old starts over", throttleRow{50, t0, nil}, 5, t0.Add(24 * time.Hour), false, 0, 1, 0},
		{"a count just under 24 hours old does not", throttleRow{50, t0, nil}, 5, t0.Add(24*time.Hour - time.Second), false, 0, 51, 15 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decide(c.row, c.threshold, c.now)
			if got.refused != c.refused {
				t.Fatalf("refused = %v, want %v", got.refused, c.refused)
			}
			if c.refused {
				if got.retryAfter != c.retry {
					t.Errorf("retryAfter = %v, want %v", got.retryAfter, c.retry)
				}
				return
			}
			if got.next.attempts != c.attempts || !got.next.lastAttempt.Equal(c.now) {
				t.Errorf("next = %d attempts at %v, want %d at %v", got.next.attempts, got.next.lastAttempt, c.attempts, c.now)
			}
			switch {
			case c.wait == 0 && got.next.blockedUntil != nil:
				t.Errorf("a wait started until %v, want none", got.next.blockedUntil)
			case c.wait != 0 && (got.next.blockedUntil == nil || !got.next.blockedUntil.Equal(c.now.Add(c.wait))):
				t.Errorf("wait until %v, want %v", got.next.blockedUntil, c.now.Add(c.wait))
			}
		})
	}
}

// TestThrottleSubjects pins what each count is keyed on. A subject is 64 hex
// characters and holds no email or address. The address-and-email count
// moves with either one; the email count ignores the address; letter case
// and spaces around the typed email do not matter; a browser's count is its
// own.
func TestThrottleSubjects(t *testing.T) {
	key, err := ThrottleKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	th := NewLoginThrottle(nil, key, session.CookieConfig{})
	email := normalizeEmail("  Owner@Example.COM ")
	if email != "owner@example.com" {
		t.Fatalf("normalizeEmail = %q", email)
	}

	pair := th.subject("pair", "198.51.100.7", email)
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, s := range []string{pair, th.subject("account", email), th.subject("browser", strings.Repeat("0", 64), email)} {
		if !hex64.MatchString(s) || strings.Contains(s, "owner") {
			t.Errorf("subject %q is not plain hex", s)
		}
	}
	distinct := map[string]string{
		"pair":                th.subject("pair", "198.51.100.7", email),
		"pair, other address": th.subject("pair", "198.51.100.8", email),
		"pair, other email":   th.subject("pair", "198.51.100.7", "other@example.com"),
		"account":             th.subject("account", email),
		"account, other":      th.subject("account", "other@example.com"),
		"browser":             th.subject("browser", strings.Repeat("0", 64), email),
		"browser, other":      th.subject("browser", strings.Repeat("1", 64), email),
	}
	seen := map[string]string{}
	for name, s := range distinct {
		if prev, dup := seen[s]; dup {
			t.Errorf("%s and %s share a subject", name, prev)
		}
		seen[s] = name
	}
	if th.subject("pair", "198.51.100.7", email) != pair {
		t.Error("the same address and email gave two subjects")
	}

	// Another key gives other subjects, so a changed key starts every count over.
	other, _ := ThrottleKey(strings.Repeat("cd", 32))
	if NewLoginThrottle(nil, other, session.CookieConfig{}).subject("pair", "198.51.100.7", email) == pair {
		t.Error("two keys gave the same subject")
	}
}

func TestThrottleKey(t *testing.T) {
	dev, err := ThrottleKey("")
	if err != nil || len(dev) != 32 {
		t.Fatalf("no key: %d bytes, %v; want the 32-byte development key", len(dev), err)
	}
	real, err := ThrottleKey(strings.Repeat("ab", 32))
	if err != nil || len(real) != 32 {
		t.Fatalf("a valid key: %d bytes, %v", len(real), err)
	}
	if string(real) == string(dev) {
		t.Error("a configured key gave the development key")
	}
	if real[0] == 0xab && real[1] == 0xab {
		t.Error("the throttle key is the encryption key itself, not derived from it")
	}
	for _, bad := range []string{"abc", strings.Repeat("zz", 32), strings.Repeat("ab", 31)} {
		if _, err := ThrottleKey(bad); err == nil {
			t.Errorf("ThrottleKey(%q) accepted a malformed key", bad)
		}
	}
}

func TestDeviceToken(t *testing.T) {
	good := strings.Repeat("0a", 32)
	for _, c := range []struct {
		value, want string
	}{
		{good, good},
		{"", ""},
		{strings.Repeat("0A", 32), ""}, // upper case: not a value Velox sets
		{strings.Repeat("0g", 32), ""},
		{good + "00", ""},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		if c.value != "" {
			r.AddCookie(&http.Cookie{Name: DeviceCookieName, Value: c.value})
		}
		if got := deviceToken(r); got != c.want {
			t.Errorf("deviceToken(%q) = %q, want %q", c.value, got, c.want)
		}
	}
}

func TestWriteLoginThrottled(t *testing.T) {
	for _, c := range []struct {
		wait    time.Duration
		retry   string
		message string
	}{
		{60 * time.Second, "60", "Too many sign-in attempts. Try again in 1 minute."},
		{59*time.Second + time.Millisecond, "60", "Too many sign-in attempts. Try again in 1 minute."},
		{61 * time.Second, "61", "Too many sign-in attempts. Try again in 2 minutes."},
		{15 * time.Minute, "900", "Too many sign-in attempts. Try again in 15 minutes."},
		{time.Millisecond, "1", "Too many sign-in attempts. Try again in 1 minute."},
	} {
		w := httptest.NewRecorder()
		writeLoginThrottled(w, httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil), c.wait)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != c.retry {
			t.Errorf("wait %v: status %d, Retry-After %q; want 429, %s", c.wait, w.Code, w.Header().Get("Retry-After"), c.retry)
		}
		if body := w.Body.String(); !strings.Contains(body, `"code":"login_throttled"`) || !strings.Contains(body, c.message) {
			t.Errorf("wait %v: body %s, want code login_throttled and %q", c.wait, body, c.message)
		}
	}
}

// TestLogin_ThrottleFailsClosed: when the throttle cannot reach Postgres the
// attempt is refused with 503 and the password is not checked. Sign-in needs
// the database anyway, so failing open would only skip the limit.
func TestLogin_ThrottleFailsClosed(t *testing.T) {
	pool, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	key, _ := ThrottleKey("")
	th := NewLoginThrottle(postgres.NewDB(pool, time.Second), key, session.CookieConfig{Path: "/"})

	hash, err := bcrypt.GenerateFromPassword([]byte("a-good-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	u := domain.User{ID: "usr_1", Email: "op@acme.com", PasswordHash: string(hash)}
	store := &lookupCounter{Store: &fakeUserStore{loginUser: &u, tenants: []domain.UserTenant{{UserID: "usr_1", TenantID: "ten_acme"}}}}

	code := itSignIn(itHandler(store, th), "198.51.100.4", "op@acme.com", "a-good-password-123")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("throttle unavailable: status %d, want 503", code)
	}
	if n := store.lookups.Load(); n != 0 {
		t.Fatalf("throttle unavailable: %d account lookups, want 0", n)
	}
	// Control: the same handler without the throttle signs in.
	if code := itSignIn(itHandler(store, nil), "198.51.100.4", "op@acme.com", "a-good-password-123"); code != http.StatusOK {
		t.Fatalf("control without the throttle: status %d, want 200", code)
	}
}
