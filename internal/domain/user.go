package domain

import "time"

// User is a dashboard operator account. Hardcount stores the email and a
// password hash, and sessions are bound to the user (see ADR-011). Operators
// are set up outside the app (`make bootstrap`), and there is no public
// sign-up. Invite flows for more users wait until a design partner (DP) asks
// for them.
type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	PasswordHash string     `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// UserTenant binds a user to a tenant with a role. v1 has only the `owner`
// role. The column shape can hold other roles, such as member or viewer,
// once invite flows exist.
type UserTenant struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
}

// PasswordResetToken is a single-use credential, valid for 1 hour, for
// resetting a forgotten password. The plaintext goes to the user in the
// reset-link email. The database stores the SHA-256 of the plaintext, so a
// database snapshot cannot be replayed.
type PasswordResetToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	TokenHash string     `json:"-"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
