// Package session implements dashboard sessions, issued by the email and
// password login (ADR-011). The browser holds an httpOnly cookie with a random
// id; the database stores only its hash (HashID). Service.Issue mints a
// session, Resolve turns a cookie into its session, Revoke and
// RevokeAllForUser end sessions, and SetLivemode switches a session's mode.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// CookieName is the cookie that carries the raw session id.
const CookieName = "hardcount_session"

// DefaultTTL is a session's lifetime: long enough that an operator does not
// sign in every day, short enough that a stolen cookie stops working on its
// own.
const DefaultTTL = 7 * 24 * time.Hour

// ErrNotFound signals that a session id resolves to no row, or to a
// row that has been revoked or expired. Callers funnel all three into
// 401 to deny enumeration of session ids.
var ErrNotFound = errors.New("session: not found")

// Session is a dashboard_sessions row. Its id is stored hashed (HashID), and
// the raw id never leaves the cookie. UserID is the operator it was issued
// for: a session is bound to a user, not to a key (ADR-011). Livemode is the
// mode it acts in, which SetLivemode switches.
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
// not revoked and not yet expired.
func (s Session) IsActive(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	return now.Before(s.ExpiresAt)
}

// HashID returns the storage form of a raw session id.
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
