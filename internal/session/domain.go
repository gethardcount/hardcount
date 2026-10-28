// Package session implements dashboard sessions minted by the
// email+password login flow. The credential is the operator's
// password (verified via bcrypt); the browser-side artefact is an
// httpOnly cookie tied to the user — see ADR-011 for the full
// rationale and what changed from the previous (key-bound) shape.
//
// The package deliberately exposes only the surface the dashboard auth
// flow needs: Issue (mint a session for an authenticated user), Get
// (resolve a cookie value to its session row), Revoke (logout).
// Everything is keyed by sha256(raw_id); the raw id only ever lives
// in the cookie.
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

// DefaultTTL is the default session lifetime. 7 days matches GitHub's
// auth cookie default and the prior (ADR-008) value; long enough that
// operators don't see a daily login prompt, short enough that a
// stolen cookie has bounded lifetime even if the operator never
// realises and the cookie isn't revoked server-side.
const DefaultTTL = 7 * 24 * time.Hour

// ErrNotFound signals that a session id resolves to no row, or to a
// row that has been revoked or expired. Callers funnel all three into
// 401 to deny enumeration of session ids.
var ErrNotFound = errors.New("session: not found")

// Session is the domain row. id_hash is sha256(raw); the raw id never
// leaves the cookie. UserID identifies the operator the session was
// minted for; sessions are user-bound (ADR-011), not key-bound.
// Livemode is captured at session-issue time and stays static —
// sessions don't toggle modes; a mode flip would mint a new session.
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

// IsActive reports whether the session can authenticate a request:
// not revoked and not yet expired. The DB query already filters
// revoked rows out for the active path; this guard catches expiry
// without a clock-keyed index lookup on every request.
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

// newRawID mints a 256-bit random id encoded as 64 hex chars. The raw
// form lives only in the cookie; the DB stores HashID(raw).
func newRawID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
