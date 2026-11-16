package user

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Store is the persistence boundary for user accounts, the user↔tenant
// join, and password reset tokens. ADR-011.
// ErrResetSendCapped means this account has already been sent
// ResetSendsPerWindow reset links inside ResetSendWindow, so no token was
// issued. It is not an error the caller surfaces: the endpoint's response is
// fixed either way (the fixed body is the account-enumeration defence).
var ErrResetSendCapped = errors.New("user: password-reset send cap reached for this account")

const (
	// ResetSendsPerWindow / ResetSendWindow bound how many reset EMAILS one
	// account can be sent. The per-IP limiter on /v1/auth slows credential
	// stuffing but does nothing against one caller pointing many requests at
	// a single victim's address, each inside the IP budget — flooding their
	// inbox and burning SMTP quota.
	//
	// The budget is derived from password_reset_tokens itself: the rows ARE
	// the record of what was sent, so the cap is cluster-wide by
	// construction, survives restarts, and needs no Redis. It replaced a
	// Redis bucket (ha-14 PR-B) that silently vanished wherever Redis was
	// unreachable — verified 2026-08-31: 8 consecutive sends, 0 refusals —
	// and before that an in-process map that degraded to 3xN per hour at N
	// replicas. Keyed on the ACCOUNT rather than the typed string, so it
	// counts what was actually delivered.
	ResetSendsPerWindow = 3
	ResetSendWindow     = time.Hour
)

type Store interface {
	// GetByEmail loads the user with the given email (case-insensitive
	// via citext). Returns errs.ErrNotFound when no row matches.
	GetByEmail(ctx context.Context, email string) (domain.User, error)

	// GetByID loads the user by id.
	GetByID(ctx context.Context, id string) (domain.User, error)

	// TouchLastLogin updates last_login_at on successful login.
	TouchLastLogin(ctx context.Context, id string, at time.Time) error

	// TenantsForUser returns the tenant memberships for a user. v1 has
	// 1:1 user:tenant; the shape supports growth.
	TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error)

	// CreateResetToken enforces the per-account send cap and inserts a row
	// whose token_hash matches the caller-provided hash. Plaintext token
	// never enters the DB. Over the cap it returns ErrResetSendCapped and
	// writes nothing; the caller sends no email and answers with the same
	// fixed generic body.
	CreateResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (domain.PasswordResetToken, error)

	// ResetPassword spends the token whose hash is tokenHash, voids the
	// user's other tokens, sets passwordHash and stamps the transaction for
	// an audit row in the user's first-joined workspace. then runs last, on
	// the same transaction, with the token's owner. It must not commit or
	// roll back tx; an error it returns rolls the whole reset back and is
	// returned. Returns errResetLinkDead, and changes nothing, for a token
	// that is unknown, expired or used, or a user with no workspace.
	ResetPassword(ctx context.Context, tokenHash, passwordHash string, then func(tx *sql.Tx, userID string) error) (domain.User, error)

	// LookupResetToken returns the owning user_id when ResetPassword would
	// accept the token: unused, unexpired, and its user belongs to a
	// workspace. It changes nothing. Returns errs.ErrNotFound otherwise.
	LookupResetToken(ctx context.Context, tokenHash string) (string, error)
}
