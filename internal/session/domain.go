// Package session implements dashboard sessions, minted by the email and
// password login flow. The credential is the operator's password, verified
// with bcrypt. The browser holds an httpOnly cookie tied to the user. See
// ADR-011 for the full reasoning.
//
// The package exposes, on purpose, only what the dashboard auth flow needs:
// Issue (mint a session for an authenticated user), Get (resolve a cookie
// value to its session row) and Revoke (logout). Everything is keyed by
// sha256(raw_id), and the raw id lives only in the cookie.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// CookieName is the HTTP cookie that carries the raw session id. Kept
// constant so middleware, login, and logout all reference the same
// name without risk of drift.
const CookieName = "hardcount_session"

// DefaultTTL is the default session lifetime. 7 days matches GitHub's auth
// cookie default and ADR-008's value. It is long enough that operators do not
// see a daily login prompt. It is short enough that a stolen cookie has a
// bounded lifetime, even if the operator never notices and the cookie is not
// revoked on the server.
const DefaultTTL = 7 * 24 * time.Hour

// ErrNotFound signals that a session id resolves to no row, or to a
// row that has been revoked or expired. Callers funnel all three into
// 401 to deny enumeration of session ids.
var ErrNotFound = errors.New("session: not found")

// Session is the domain row. id_hash is sha256(raw), and the raw id never
// leaves the cookie. UserID identifies the operator the session was minted
// for: sessions are bound to a user (ADR-011), not to a key. Livemode is set
// when the session is issued and stays fixed. Sessions do not switch modes,
// and a mode switch would mint a new session.
type Session struct {
	IDHash     string
	UserID     string
	TenantID   string
	Livemode   bool
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	UserAgent  string
	IP         string
}

// IsActive reports whether the session can authenticate a request: it is
// not revoked and not yet expired. The database query already filters out
// revoked rows on the active path. This check catches expiry without an
// index lookup keyed on the clock for every request.
func (s Session) IsActive(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	return now.Before(s.ExpiresAt)
}

// HashID returns the storage form of a raw session id. Exported so
// the logout handler can hash the cookie value before calling Revoke.
func HashID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newRawID mints a 256-bit random id encoded as 64 hex chars. The raw form
// lives only in the cookie, and the database stores HashID(raw).
func newRawID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
