// Package session implements dashboard sessions, issued by the email and
// password login (ADR-011). The browser holds an httpOnly cookie with a random
// id; the database stores only its sha256 (HashID), so a copy of the table
// cannot be replayed. A session is live while it is not revoked and not
// expired: Resolve, End, EndTx and SetLivemode act only on a live one.
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

// ErrNotFound signals that a session id resolves to no row, or to a row that
// has been revoked or expired.
var ErrNotFound = errors.New("session: not found")

// Session is a dashboard_sessions row. It is bound to the user it was issued
// for, not to a key (ADR-011). Livemode is the mode it acts in, which
// SetLivemode switches.
type Session struct {
	IDHash    string
	UserID    string
	TenantID  string
	Livemode  bool
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
	UserAgent string
	IP        string
}

// HashID returns the storage form of a raw session id.
func HashID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// wellFormed reports whether raw has the shape newRawID mints: 64 lowercase
// hex characters. A value of any other shape cannot be a session, so it is
// treated as no cookie, with no database read.
func wellFormed(raw string) bool {
	if len(raw) != 64 {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// newRawID mints a 256-bit random id encoded as 64 hex chars.
func newRawID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
