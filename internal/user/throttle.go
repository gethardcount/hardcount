package user

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/session"
)

// The sign-in throttle (ADR-094, amended 2026-10-18). It slows repeated
// wrong passwords instead of locking the account.
//
// Every sign-in attempt is counted before the password is checked, under one
// or two subjects:
//
//   - A browser that has signed in to this email before (it sends the
//     velox_device cookie) is counted under that browser and email only.
//     Guesses from elsewhere never hold it out.
//   - Any other browser is counted twice: under its address and the email,
//     which stops one source early; and under the email alone, which caps
//     guesses spread over many addresses.
//
// Once a subject reaches its threshold, each further attempt starts a wait:
// 1 minute, then 2, 4 and 8, then 15 minutes per attempt. An attempt made
// during a wait is refused before any account lookup or bcrypt, and is not
// counted. A count whose last attempt is over 24 hours old starts again.
// Proof of the account (the right password, a completed password reset, an
// accepted invite) marks the browser and clears this address's count. Only
// the right password from a browser the email's count held clears that
// count too (SignedIn says why).
//
// Every subject is an HMAC keyed from VELOX_ENCRYPTION_KEY, and the typed
// email is used as typed (trimmed, lower-cased), so an email with no account
// is counted and answered exactly like a real one. All times come from the
// database clock, so every copy of the server shares one count.
const (
	// DeviceCookieName is the cookie that marks a browser that signed in.
	// It holds 32 random bytes as hex and is sent only to /v1/auth.
	DeviceCookieName = "velox_device"

	// pairThreshold: one address and one email. Attempts 1 to 5 reach the
	// password check; the 5th starts a 1-minute wait.
	pairThreshold = 5
	// accountThreshold: one email, from every browser that never signed in
	// to it, over all addresses. The 10th attempt starts a 1-minute wait.
	// After that about 100 guesses a day reach the check, with no cap.
	// NIST SP 800-63B-4 §3.2.2 caps 100 failures in a row and then disables
	// the password; Velox does not lock, by decision (ADR-094).
	accountThreshold = 10
	// browserThreshold: one known browser and one email.
	browserThreshold = 5

	firstWait   = time.Minute
	longestWait = 15 * time.Minute
	// countFades: a count whose last attempt is older starts from zero.
	countFades = 24 * time.Hour
	// knownBrowserFor: how long a browser stays known after its last
	// sign-in. The cookie lives as long.
	knownBrowserFor = 180 * 24 * time.Hour
)

// throttleRow is one subject's stored count.
type throttleRow struct {
	attempts     int
	lastAttempt  time.Time
	blockedUntil *time.Time
}

// throttleStep is what decide returns for one subject.
type throttleStep struct {
	refused    bool
	retryAfter time.Duration // set when refused
	next       throttleRow   // the row to store when not refused
}

// decide applies the throttle rule to one subject's row at the database
// time now. A refused attempt leaves the row as it was.
func decide(row throttleRow, threshold int, now time.Time) throttleStep {
	if row.blockedUntil != nil && now.Before(*row.blockedUntil) {
		return throttleStep{refused: true, retryAfter: row.blockedUntil.Sub(now)}
	}
	attempts := row.attempts
	if now.Sub(row.lastAttempt) >= countFades {
		attempts = 0
	}
	attempts++
	next := throttleRow{attempts: attempts, lastAttempt: now}
	if attempts >= threshold {
		wait := longestWait
		// 1, 2, 4 and 8 minutes, then the 15-minute cap. The shift is
		// bounded so a long-running count cannot overflow it.
		if shift := attempts - threshold; shift < 4 {
			wait = firstWait << shift
		}
		until := now.Add(wait)
		next.blockedUntil = &until
	}
	return throttleStep{next: next}
}

// ThrottleKey derives the throttle's HMAC key from VELOX_ENCRYPTION_KEY
// (64 hex characters) with HKDF. With no key, as in local development, it
// derives from a fixed value: the subjects then still hold no email or
// address, but anyone with the code can test a guess against them.
// Production refuses to start without a key.
func ThrottleKey(encryptionKeyHex string) ([]byte, error) {
	secret := []byte("velox local development: VELOX_ENCRYPTION_KEY is not set")
	if encryptionKeyHex != "" {
		b, err := hex.DecodeString(encryptionKeyHex)
		if err != nil || len(b) != 32 {
			return nil, errors.New("VELOX_ENCRYPTION_KEY must be 64 hex characters")
		}
		secret = b
	}
	return hkdf.Key(sha256.New, secret, nil, "velox/login-throttle/v1", 32)
}

// LoginThrottle counts sign-in attempts in the login_throttle table.
type LoginThrottle struct {
	db     *postgres.DB
	key    []byte
	cookie session.CookieConfig
}

// NewLoginThrottle builds the throttle. key comes from ThrottleKey; cookie
// gives the velox_device cookie the session cookie's Domain, Secure and
// SameSite.
func NewLoginThrottle(db *postgres.DB, key []byte, cookie session.CookieConfig) *LoginThrottle {
	return &LoginThrottle{db: db, key: key, cookie: cookie}
}

// ThrottleVerdict says whether one sign-in attempt may go on to the
// password check.
type ThrottleVerdict struct {
	Allowed    bool
	RetryAfter time.Duration // when refused: until the longest wait ends
	// Account is the first 12 hex characters of the email's subject. Logs
	// use it to group attempts on one email from any address without
	// writing the email down.
	Account string
	// countedForEmail: the attempt was counted under the email's subject,
	// because it came from a browser that never signed in to the email.
	countedForEmail bool
}

// subject is the hex HMAC of the kind and its parts, NUL-separated. Only
// the last part (the email) is free text, so no two inputs collide.
func (t *LoginThrottle) subject(kind string, parts ...string) string {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte("v1\x00" + kind))
	for _, p := range parts {
		m.Write([]byte{0})
		m.Write([]byte(p))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// normalizeEmail is the email as the throttle keys it.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// deviceToken reads the velox_device cookie. A missing or malformed value
// reads as "".
func deviceToken(r *http.Request) string {
	c, err := r.Cookie(DeviceCookieName)
	if err != nil || len(c.Value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(c.Value); err != nil || strings.ToLower(c.Value) != c.Value {
		return ""
	}
	return c.Value
}

type thresholdSubject struct {
	subject   string
	threshold int
}

// Reserve counts one sign-in attempt for the typed email from this request,
// before the password is checked. When any of the attempt's subjects is in
// a wait, nothing is counted and the verdict is a refusal.
func (t *LoginThrottle) Reserve(r *http.Request, email string) (ThrottleVerdict, error) {
	ctx := r.Context()
	email = normalizeEmail(email)
	account := t.subject("account", email)
	verdict := ThrottleVerdict{Account: account[:12]}

	tx, err := t.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return verdict, fmt.Errorf("login throttle: begin: %w", err)
	}
	defer postgres.Rollback(tx)

	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return verdict, fmt.Errorf("login throttle: read the clock: %w", err)
	}

	var subjects []thresholdSubject
	knownBrowser := false
	if tok := deviceToken(r); tok != "" {
		browser := t.subject("browser", tok, email)
		var one int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM login_throttle
			 WHERE subject = $1 AND signed_in_at > $2
			   FOR UPDATE`, browser, now.Add(-knownBrowserFor)).Scan(&one)
		switch {
		case err == nil:
			knownBrowser = true
			subjects = []thresholdSubject{{browser, browserThreshold}}
		case !errors.Is(err, sql.ErrNoRows):
			return verdict, fmt.Errorf("login throttle: read the browser: %w", err)
		}
	}
	if !knownBrowser {
		subjects = []thresholdSubject{
			{t.subject("pair", audit.ExtractClientIP(r), email), pairThreshold},
			{account, accountThreshold},
		}
	}
	// One lock order for every writer, so two attempts never deadlock.
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].subject < subjects[j].subject })

	steps := make([]throttleStep, len(subjects))
	for i, s := range subjects {
		// One statement adds the row if it is new, locks it and reads it.
		// Added and then locked in two statements, a row that a sign-in or
		// the cleanup deleted in between left nothing to lock, and the
		// attempt failed with a 503. ON CONFLICT DO UPDATE waits for such a
		// delete and then adds the row again.
		var row throttleRow
		var blocked sql.NullTime
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO login_throttle (subject) VALUES ($1)
			ON CONFLICT (subject) DO UPDATE SET attempts = login_throttle.attempts
			RETURNING attempts, last_attempt_at, blocked_until`, s.subject).
			Scan(&row.attempts, &row.lastAttempt, &blocked); err != nil {
			return verdict, fmt.Errorf("login throttle: lock a subject: %w", err)
		}
		if blocked.Valid {
			row.blockedUntil = &blocked.Time
		}
		steps[i] = decide(row, s.threshold, now)
		if steps[i].refused && steps[i].retryAfter > verdict.RetryAfter {
			verdict.RetryAfter = steps[i].retryAfter
		}
	}
	if verdict.RetryAfter > 0 {
		// Refused: the deferred rollback leaves every count as it was.
		return verdict, nil
	}

	for i, s := range subjects {
		next := steps[i].next
		if _, err := tx.ExecContext(ctx, `
			UPDATE login_throttle
			   SET attempts = $2, last_attempt_at = $3, blocked_until = $4
			 WHERE subject = $1`, s.subject, next.attempts, next.lastAttempt, next.blockedUntil); err != nil {
			return verdict, fmt.Errorf("login throttle: count an attempt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return verdict, fmt.Errorf("login throttle: commit: %w", err)
	}
	verdict.Allowed = true
	verdict.countedForEmail = !knownBrowser
	return verdict, nil
}

// SignedIn records a sign-in with the right password. v is the verdict
// Reserve gave the same request. The sign-in marks the browser, sets the
// velox_device cookie and clears this address's count for the email.
//
// It clears the email's count only when the attempt was counted under it,
// that is, from a browser that never signed in to the email. A successful
// sign-in should reset the failures it was held by (NIST SP 800-63B-4
// §3.2.2), and without that reset a person who signs in from private
// windows, or a script with no cookie jar, would pile up waits. Such a
// sign-in cannot end a running wait: during one, the attempt was refused.
// A known browser was never held by the email's count, so its sign-in leaves
// the count alone. Clearing it there would end other people's wait early,
// which shows a guesser that the email has an account and gives them a new
// allowance of guesses.
func (t *LoginThrottle) SignedIn(w http.ResponseWriter, r *http.Request, email string, v ThrottleVerdict) error {
	return t.proved(w, r, email, v.countedForEmail)
}

// InboxProved records a completed password reset or an accepted invite.
// Each proves the inbox, so it marks the browser and clears this address's
// count for the email, like a sign-in. It leaves the email's count running:
// the marked browser is no longer held by it, and clearing it would end
// other people's wait early (see SignedIn).
func (t *LoginThrottle) InboxProved(w http.ResponseWriter, r *http.Request, email string) error {
	return t.proved(w, r, email, false)
}

// proved marks the browser as known for the email, clears this address's
// count for the email and, when clearEmail is set, the email's count.
// Counts for the email at other addresses stay until they fade.
func (t *LoginThrottle) proved(w http.ResponseWriter, r *http.Request, email string, clearEmail bool) error {
	ctx := r.Context()
	email = normalizeEmail(email)
	tok := deviceToken(r)
	if tok == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("login throttle: browser token: %w", err)
		}
		tok = hex.EncodeToString(buf)
	}
	cleared := []string{t.subject("pair", audit.ExtractClientIP(r), email)}
	if clearEmail {
		cleared = append(cleared, t.subject("account", email))
	}
	sort.Strings(cleared)

	tx, err := t.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return fmt.Errorf("login throttle: begin: %w", err)
	}
	defer postgres.Rollback(tx)
	for _, s := range cleared {
		if _, err := tx.ExecContext(ctx, `DELETE FROM login_throttle WHERE subject = $1`, s); err != nil {
			return fmt.Errorf("login throttle: clear a count: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO login_throttle (subject, attempts, last_attempt_at, blocked_until, signed_in_at)
		VALUES ($1, 0, now(), NULL, now())
		ON CONFLICT (subject) DO UPDATE
		   SET attempts = 0, last_attempt_at = now(), blocked_until = NULL, signed_in_at = now()`,
		t.subject("browser", tok, email)); err != nil {
		return fmt.Errorf("login throttle: mark the browser: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("login throttle: commit: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     DeviceCookieName,
		Value:    tok,
		Path:     "/v1/auth",
		Domain:   t.cookie.Domain,
		MaxAge:   int(knownBrowserFor / time.Second),
		HttpOnly: true,
		Secure:   t.cookie.Secure,
		SameSite: t.cookie.SameSite,
	})
	return nil
}

// Cleanup deletes rows nothing reads any more: no attempt for 24 hours, no
// wait still running, and no sign-in from that browser within 180 days.
// The leader runs it with the other cleanup steps.
func (t *LoginThrottle) Cleanup(ctx context.Context) (int, error) {
	tx, err := t.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(ctx, `
		DELETE FROM login_throttle
		 WHERE last_attempt_at < now() - make_interval(secs => $1)
		   AND (blocked_until IS NULL OR blocked_until <= now())
		   AND (signed_in_at IS NULL OR signed_in_at < now() - make_interval(secs => $2))`,
		countFades.Seconds(), knownBrowserFor.Seconds())
	if err != nil {
		return 0, fmt.Errorf("login throttle cleanup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
