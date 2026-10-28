package domain

import "time"

// User is a dashboard operator account. Hardcount stores email + password
// hash; sessions are user-bound (see ADR-011). Operators are
// bootstrapped externally (`make bootstrap`) — there is no public
// signup. Multi-user invite flows are deferred until a DP requests.
type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	PasswordHash string     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// UserTenant binds a user to a tenant with a role. v1 ships only the
// `owner` role; the column shape supports member/viewer/etc when
// invite flows land.
type UserTenant struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
}

// PasswordResetToken is a single-use, 1-hour-expiry credential for
// resetting a forgotten password. The plaintext is sent to the user
// via the reset-link email; the DB stores SHA-256 of the plaintext
// so a snapshot can't be replayed.
type PasswordResetToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	TokenHash string     `json:"-"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
