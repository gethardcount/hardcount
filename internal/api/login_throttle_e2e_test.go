package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The sign-in throttle (ADR-094, amended 2026-10-18), driven over the real
// router and a real Postgres. Repeated wrong passwords slow down, per
// (address × email) and per email across every address; a browser that
// signed in before is not slowed by other people's guesses; a refusal looks
// the same for an email with no account; and the count lives in Postgres,
// so two copies of the server share it.
//
// Numbers the tests rely on:
//   - one address and one email: attempts 1 to 5 reach the password check,
//     and the 5th starts a 1-minute wait;
//   - one email from browsers that never signed in to it, over all
//     addresses: attempts 1 to 10 reach the check, and the 10th starts a
//     1-minute wait;
//   - a browser that signed in to the email before carries the hardcount_device
//     cookie, and its attempts count only against that browser.

const (
	throttleBootToken = "throttle-e2e-bootstrap"
	throttleOwner     = "owner@throttle-e2e.test"
	throttlePassword  = "throttle-e2e-pw-12"
	throttleWrong     = "not-the-password-12"
)

// throttleServer starts a server with one account. The test client is the
// trusted proxy, so X-Forwarded-For sets the client address of each request.
func throttleServer(t *testing.T) (*postgres.DB, *httptest.Server) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", throttleBootToken)
	t.Setenv("TRUST_PROXY", "127.0.0.1/32")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	flowBootstrap(t, ts, throttleBootToken, throttleOwner, throttlePassword)
	return db, ts
}

// signInTry is one sign-in answer, reduced to what the throttle decides.
type signInTry struct {
	status     int
	errType    string
	code       string
	message    string
	retryAfter int    // seconds; 0 when the header is absent
	body       string // the JSON body without request_id, to compare two answers
	browser    *http.Cookie
	session    *http.Cookie
}

// authPost posts to a /v1/auth path from client address ip, with the
// hardcount_device cookie when browser is not nil.
func authPost(t *testing.T, ts *httptest.Server, path, ip string, browser *http.Cookie, body map[string]any) signInTry {
	t.Helper()
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	if browser != nil {
		req.AddCookie(browser)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	out := signInTry{status: resp.StatusCode}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if e, ok := parsed["error"].(map[string]any); ok {
		out.errType, _ = e["type"].(string)
		out.code, _ = e["code"].(string)
		out.message, _ = e["message"].(string)
		delete(e, "request_id")
	}
	norm, _ := json.Marshal(parsed)
	out.body = string(norm)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		out.retryAfter, _ = strconv.Atoi(ra)
	}
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "hardcount_device":
			if c.Value != "" {
				out.browser = c
			}
		case "hardcount_session":
			if c.Value != "" {
				out.session = c
			}
		}
	}
	return out
}

func trySignIn(t *testing.T, ts *httptest.Server, ip, email, password string, browser *http.Cookie) signInTry {
	t.Helper()
	return authPost(t, ts, "/v1/auth/login", ip, browser, map[string]any{"email": email, "password": password})
}

func (s signInTry) want(t *testing.T, status int, what string) signInTry {
	t.Helper()
	if s.status != status {
		t.Fatalf("%s: status %d, want %d; body %s", what, s.status, status, s.body)
	}
	return s
}

// wantWrongPassword checks the answer every wrong password gets: today's 401.
func (s signInTry) wantWrongPassword(t *testing.T, what string) signInTry {
	t.Helper()
	s.want(t, http.StatusUnauthorized, what)
	if s.message != "invalid email or password" || s.retryAfter != 0 {
		t.Fatalf("%s: got message %q, Retry-After %d; want the generic wrong-password answer", what, s.message, s.retryAfter)
	}
	return s
}

// wantThrottled checks the answer a refused attempt gets.
func (s signInTry) wantThrottled(t *testing.T, what string) signInTry {
	t.Helper()
	s.want(t, http.StatusTooManyRequests, what)
	if s.errType != "rate_limit_error" || s.code != "login_throttled" {
		t.Fatalf("%s: type %q code %q, want rate_limit_error login_throttled; body %s", what, s.errType, s.code, s.body)
	}
	if s.retryAfter < 1 || s.retryAfter > 15*60 {
		t.Fatalf("%s: Retry-After %d, want 1 to 900 seconds", what, s.retryAfter)
	}
	if !strings.HasPrefix(s.message, "Too many sign-in attempts. Try again in ") {
		t.Fatalf("%s: message %q", what, s.message)
	}
	return s
}

func wrongFrom(t *testing.T, ts *httptest.Server, ip, email string, n int, what string) {
	t.Helper()
	for i := 1; i <= n; i++ {
		trySignIn(t, ts, ip, email, throttleWrong, nil).wantWrongPassword(t, fmt.Sprintf("%s: wrong password %d", what, i))
	}
}

// tripAccount makes ten wrong attempts on email, one from each of ten new
// addresses, so no single address is slowed but the email is.
func tripAccount(t *testing.T, ts *httptest.Server, email, prefix string) {
	t.Helper()
	for i := 1; i <= 10; i++ {
		ip := fmt.Sprintf("%s.%d", prefix, i)
		trySignIn(t, ts, ip, email, throttleWrong, nil).wantWrongPassword(t, "wrong password from "+ip)
	}
	trySignIn(t, ts, prefix+".11", email, throttleWrong, nil).wantThrottled(t, "11th wrong password, from an 11th address")
}

// TestE2E_LoginThrottle_SlowsRepeatedWrongPasswords: five wrong passwords
// from one address, then the sixth waits about a minute. While the wait
// lasts even the right password is refused, and the password is not checked.
// The wait belongs to that address and email only.
func TestE2E_LoginThrottle_SlowsRepeatedWrongPasswords(t *testing.T) {
	db, ts := throttleServer(t)
	const attacker, owner = "198.51.100.7", "203.0.113.20"

	wrongFrom(t, ts, attacker, throttleOwner, 5, "one address")
	sixth := trySignIn(t, ts, attacker, throttleOwner, throttleWrong, nil).wantThrottled(t, "6th wrong password")
	if sixth.retryAfter < 55 || sixth.retryAfter > 60 {
		t.Errorf("6th wrong password: Retry-After %d, want about 60", sixth.retryAfter)
	}
	if sixth.message != "Too many sign-in attempts. Try again in 1 minute." {
		t.Errorf("6th wrong password: message %q", sixth.message)
	}
	trySignIn(t, ts, attacker, throttleOwner, throttlePassword, nil).wantThrottled(t, "right password while the address waits")

	// Never a lockout: the owner, at another address, signs in.
	ok := trySignIn(t, ts, owner, throttleOwner, throttlePassword, nil).want(t, http.StatusOK, "owner from another address")
	if ok.session == nil {
		t.Fatal("owner from another address got no session cookie")
	}
	// The same address with another email is not slowed.
	wrongFrom(t, ts, attacker, "someone-else@throttle-e2e.test", 3, "same address, another email")

	// The table holds only HMAC hex: no email, no address.
	rows, err := db.Pool.QueryContext(t.Context(), `SELECT subject FROM login_throttle`)
	if err != nil {
		t.Fatalf("read login_throttle: %v", err)
	}
	defer func() { _ = rows.Close() }()
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	n := 0
	for rows.Next() {
		var subject string
		if err := rows.Scan(&subject); err != nil {
			t.Fatal(err)
		}
		n++
		if !hex64.MatchString(subject) {
			t.Errorf("login_throttle subject %q is not 64 hex characters", subject)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("login_throttle has no rows after a throttled sign-in")
	}
}

// TestE2E_LoginThrottle_NoAccountLooksTheSame: an email with no account gets
// the same answers, in the same order, as a real one. The counter is keyed
// on the typed email and checked before any account lookup.
func TestE2E_LoginThrottle_NoAccountLooksTheSame(t *testing.T) {
	_, ts := throttleServer(t)
	const nobody = "nobody@throttle-e2e.test"

	var real, fake []signInTry
	for i := 0; i < 7; i++ {
		real = append(real, trySignIn(t, ts, "198.51.100.30", throttleOwner, throttleWrong, nil))
		fake = append(fake, trySignIn(t, ts, "198.51.100.31", nobody, throttleWrong, nil))
	}
	for i := range real {
		r, f := real[i], fake[i]
		if r.status != f.status || r.body != f.body || (r.retryAfter == 0) != (f.retryAfter == 0) {
			t.Errorf("attempt %d: account %d %s (Retry-After %d) vs no account %d %s (Retry-After %d)",
				i+1, r.status, r.body, r.retryAfter, f.status, f.body, f.retryAfter)
		}
	}
	real[4].wantWrongPassword(t, "5th attempt on the account")
	real[5].wantThrottled(t, "6th attempt on the account")
	fake[5].wantThrottled(t, "6th attempt on an email with no account")
}

// TestE2E_LoginThrottle_SuccessResetsTheCount: a right password clears the
// counts it was held by. Negative control: four wrong passwords then the
// right one signs in, so the throttle refuses nothing below its threshold.
func TestE2E_LoginThrottle_SuccessResetsTheCount(t *testing.T) {
	_, ts := throttleServer(t)
	const ip = "198.51.100.40"

	wrongFrom(t, ts, ip, throttleOwner, 4, "before the sign-in")
	trySignIn(t, ts, ip, throttleOwner, throttlePassword, nil).want(t, http.StatusOK, "right password as the 5th attempt")
	// Counted from zero again: five more wrong passwords reach the check.
	wrongFrom(t, ts, ip, throttleOwner, 5, "after the sign-in")
	trySignIn(t, ts, ip, throttleOwner, throttleWrong, nil).wantThrottled(t, "6th wrong password after the sign-in")
}

// TestE2E_LoginThrottle_NewBrowserSignInClearsTheEmailCount: a sign-in from
// a browser with no hardcount_device cookie was counted under the email, so it
// clears the email's count. Without that, a script with no cookie jar, or a
// person in a private window, would pile up waits with every sign-in.
func TestE2E_LoginThrottle_NewBrowserSignInClearsTheEmailCount(t *testing.T) {
	_, ts := throttleServer(t)

	// Nine wrong passwords from nine addresses: one short of the email's wait.
	for i := 1; i <= 9; i++ {
		ip := fmt.Sprintf("192.0.2.%d", i)
		trySignIn(t, ts, ip, throttleOwner, throttleWrong, nil).wantWrongPassword(t, "wrong password from "+ip)
	}
	// The 10th attempt is the right password from a new browser. Counted, it
	// starts the email's 1-minute wait; signed in, it clears the count.
	trySignIn(t, ts, "203.0.113.40", throttleOwner, throttlePassword, nil).want(t, http.StatusOK, "right password from a new browser")
	trySignIn(t, ts, "192.0.2.10", throttleOwner, throttleWrong, nil).wantWrongPassword(t, "wrong password after the sign-in")

	// Eleven sign-ins in a row from cookie-less clients never wait.
	for i := 1; i <= 11; i++ {
		trySignIn(t, ts, fmt.Sprintf("203.0.113.%d", 40+i), throttleOwner, throttlePassword, nil).
			want(t, http.StatusOK, fmt.Sprintf("cookie-less sign-in %d", i))
	}
}

// TestE2E_LoginThrottle_CapsOneEmailAcrossAddresses: guesses spread over many
// addresses slow down too. A browser that signed in before keeps signing in.
func TestE2E_LoginThrottle_CapsOneEmailAcrossAddresses(t *testing.T) {
	_, ts := throttleServer(t)

	home := trySignIn(t, ts, "203.0.113.50", throttleOwner, throttlePassword, nil).want(t, http.StatusOK, "owner signs in at home")
	if home.browser == nil {
		t.Fatal("a sign-in set no hardcount_device cookie")
	}
	if !home.browser.HttpOnly || home.browser.Path != "/v1/auth" {
		t.Errorf("hardcount_device cookie: HttpOnly %v, Path %q; want HttpOnly, /v1/auth", home.browser.HttpOnly, home.browser.Path)
	}

	tripAccount(t, ts, throttleOwner, "192.0.2")

	// A browser that never signed in to this email waits, at any address.
	trySignIn(t, ts, "203.0.113.51", throttleOwner, throttlePassword, nil).wantThrottled(t, "right password from a new browser")
	// A hardcount_device cookie the server never set is a new browser. Held out,
	// it shows the known-browser check reads the stored row, not just the
	// cookie's shape. (A cookie set for another email: user package,
	// TestLoginThrottle_BrowserIsKnownPerEmail.)
	trySignIn(t, ts, "203.0.113.53", throttleOwner, throttlePassword,
		&http.Cookie{Name: "hardcount_device", Value: strings.Repeat("7c", 32)}).
		wantThrottled(t, "right password with a made-up hardcount_device cookie")

	// The owner's browser is not slowed, even from a new address.
	trySignIn(t, ts, "203.0.113.52", throttleOwner, throttlePassword, home.browser).want(t, http.StatusOK, "owner's browser from a new address")
	// That sign-in leaves the email's wait running. Ending it early would
	// tell a guesser the email has an account, and give them new guesses.
	trySignIn(t, ts, "192.0.2.99", throttleOwner, throttleWrong, nil).wantThrottled(t, "wrong password after the owner signed in")
}

// TestE2E_LoginThrottle_TwoServersShareTheCount: two copies of the server on
// one database count the same attempts, so a round-robin load balancer
// does not double the allowance.
func TestE2E_LoginThrottle_TwoServersShareTheCount(t *testing.T) {
	db, one := throttleServer(t)
	two := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer two.Close()
	const ip = "198.51.100.60"

	for i := 1; i <= 5; i++ {
		srv := one
		if i%2 == 0 {
			srv = two
		}
		trySignIn(t, srv, ip, throttleOwner, throttleWrong, nil).wantWrongPassword(t, fmt.Sprintf("wrong password %d", i))
	}
	trySignIn(t, two, ip, throttleOwner, throttleWrong, nil).wantThrottled(t, "6th wrong password, on the second server")
	trySignIn(t, one, ip, throttleOwner, throttleWrong, nil).wantThrottled(t, "7th wrong password, on the first server")
}

// TestE2E_LoginThrottle_CountFadesAfterADay: a count whose last attempt is
// more than 24 hours old starts again from zero. The test moves the stored
// times back instead of waiting.
func TestE2E_LoginThrottle_CountFadesAfterADay(t *testing.T) {
	db, ts := throttleServer(t)
	const ip = "198.51.100.70"

	wrongFrom(t, ts, ip, throttleOwner, 5, "day one")
	trySignIn(t, ts, ip, throttleOwner, throttleWrong, nil).wantThrottled(t, "6th wrong password on day one")

	if _, err := db.Pool.ExecContext(t.Context(), `
		UPDATE login_throttle
		   SET last_attempt_at = last_attempt_at - interval '25 hours',
		       blocked_until = blocked_until - interval '25 hours'`); err != nil {
		t.Fatalf("move the stored times back: %v", err)
	}
	// Five more reach the check. Without the fade the 7th attempt overall
	// would start a 2-minute wait and the next one would be refused.
	wrongFrom(t, ts, ip, throttleOwner, 5, "a day later")
	trySignIn(t, ts, ip, throttleOwner, throttleWrong, nil).wantThrottled(t, "6th wrong password a day later")
}

// TestE2E_LoginThrottle_PasswordResetLetsTheOwnerBackIn: when guesses from
// many addresses hold out a new browser, a completed password reset marks
// the browser that completed it, so the owner signs in from it at once. The
// reset leaves the email's wait running for every other new browser.
func TestE2E_LoginThrottle_PasswordResetLetsTheOwnerBackIn(t *testing.T) {
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.throttle-e2e.test")
	db, ts := throttleServer(t)
	const laptop = "203.0.113.80"
	const newPassword = "throttle-e2e-new-pw-34"

	tripAccount(t, ts, throttleOwner, "192.0.2")
	trySignIn(t, ts, laptop, throttleOwner, throttlePassword, nil).wantThrottled(t, "owner on a new laptop")

	authPost(t, ts, "/v1/auth/password-reset/request", laptop, nil, map[string]any{"email": throttleOwner}).
		want(t, http.StatusOK, "request a reset")
	token := flowEmailLinkToken(t, db, "password_reset", "password_reset_url")
	reset := authPost(t, ts, "/v1/auth/password-reset/confirm", laptop, nil, map[string]any{
		"token": token, "password": newPassword,
	}).want(t, http.StatusOK, "confirm the reset")
	if reset.browser == nil {
		t.Fatal("a completed reset set no hardcount_device cookie")
	}

	// The laptop is now a known browser: the wait does not hold it out.
	trySignIn(t, ts, laptop, throttleOwner, newPassword, reset.browser).want(t, http.StatusOK, "owner's laptop after the reset")
	// Any other new browser still waits: a reset does not end the wait early.
	trySignIn(t, ts, "203.0.113.81", throttleOwner, newPassword, nil).wantThrottled(t, "new password from another new browser")
}

// TestE2E_LoginThrottle_InviteAcceptMarksTheBrowser: a new member who
// accepts an invite proved their inbox and set a password. The browser that
// accepted becomes known, so guesses made at the email before do not hold it
// out. The email's wait keeps running for other new browsers.
func TestE2E_LoginThrottle_InviteAcceptMarksTheBrowser(t *testing.T) {
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.throttle-e2e.test")
	// Invites are refused with no SMTP. No dispatcher runs here.
	t.Setenv("SMTP_HOST", "smtp.throttle-e2e.invalid")
	db, ts := throttleServer(t)
	const member, memberPassword, desk = "teammate@throttle-e2e.test", "teammate-pw-1234", "203.0.113.90"

	owner := login(t, ts, throttleOwner, throttlePassword)
	inv := postWithCookie(t, ts, "/v1/members/invite", owner, map[string]any{"email": member})
	assertStatus(t, inv, http.StatusCreated)
	_ = inv.Body.Close()
	token := inviteTokenFromOutbox(t, db)

	// Someone guesses at the address before the account exists.
	tripAccount(t, ts, member, "192.0.2")

	accept := authPost(t, ts, "/v1/auth/accept-invite", desk, nil, map[string]any{
		"token": token, "password": memberPassword,
	}).want(t, http.StatusOK, "accept the invite")
	if accept.session == nil || accept.browser == nil {
		t.Fatalf("accept set session %v, hardcount_device %v; want both", accept.session != nil, accept.browser != nil)
	}
	// The browser that accepted is known: the wait does not hold it out.
	trySignIn(t, ts, desk, member, memberPassword, accept.browser).want(t, http.StatusOK, "member at the desk that accepted")
	// Another new browser still waits: accepting does not end the wait early.
	trySignIn(t, ts, "203.0.113.91", member, memberPassword, nil).wantThrottled(t, "member from another new browser")
}
