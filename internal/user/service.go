package user

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/user/breachedpw"
)

// Service is the auth-side surface for user accounts. ADR-011.
//
// The recipe is intentionally boring: bcrypt cost 12 for hashing,
// crypto/rand for tokens, sha256 for token storage. The sign-in throttle
// (throttle.go, ADR-094) runs in the handler before Authenticate. All of this
// is well-trodden Go stdlib + x/crypto territory.
type Service struct {
	store    Store
	clock    clock.Clock
	sessions SessionEnder
	audit    AuditTxWriter
}

// SessionEnder ends every session of a user on the caller's transaction.
// *session.Service satisfies it.
type SessionEnder interface {
	EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error
}

// AuditTxWriter writes an audit row on the caller's transaction.
// *audit.Logger satisfies it.
type AuditTxWriter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// NewService returns a Service. sessions and auditor join the password
// reset's transaction, so the sign-out and its audit row commit with the
// new password.
func NewService(store Store, clk clock.Clock, sessions SessionEnder, auditor AuditTxWriter) *Service {
	if clk == nil {
		clk = clock.Real()
	}
	return &Service{store: store, clock: clk, sessions: sessions, audit: auditor}
}

const (
	// bcryptCost = 12 is the OWASP-recommended floor for password
	// hashing as of 2026. Higher slows login measurably; lower is
	// brute-forceable. Don't tune without a benchmark.
	bcryptCost = 12

	// MinPasswordLength is the only complexity rule we enforce. NIST
	// SP 800-63B recommends against required-character classes (they
	// reduce real-world security by encouraging predictable
	// substitutions). 12 chars is the post-2024 floor for low-stakes
	// systems; we'd raise to 16 for a financial-services tenant if a
	// DP requested.
	MinPasswordLength = 12

	// PasswordResetTokenTTL is how long a reset token remains valid.
	// 1 hour matches the GitHub / AWS Cognito / Auth0 default — long
	// enough for the recipient to find the email and click, short
	// enough that a leaked token isn't useful days later.
	PasswordResetTokenTTL = 1 * time.Hour
)

// HashPassword applies bcrypt to a candidate plaintext. Validates
// length and the breached-password list before hashing. Returns the hash
// suitable for `users.password_hash`.
func HashPassword(plaintext string) (string, error) {
	if err := ValidatePassword(plaintext); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt: %w", err)
	}
	return string(hash), nil
}

// ValidatePassword runs the password rules without hashing. HashPassword
// calls it.
func ValidatePassword(plaintext string) error {
	if len(plaintext) < MinPasswordLength {
		return errs.Invalid("password",
			fmt.Sprintf("must be at least %d characters", MinPasswordLength))
	}
	// bcrypt has a 72-byte input cap; passwords longer than that are
	// silently truncated. Reject so the caller picks a different one
	// rather than getting a deceptively-strong-looking password
	// that's effectively the same as its 72-byte prefix.
	if len(plaintext) > 72 {
		// Shown after the word "Password" on the sign-in pages. bcrypt counts
		// bytes: an accented letter is 2 in UTF-8, most other scripts 3.
		return errs.Invalid("password", "is too long. Use at most 72 characters; accented and non-Latin letters count as two or more")
	}
	// The embedded breached-password list (ADR-095 Phase 0(a)): about
	// 30,000 passwords of 12 or more characters from the top million of a
	// breach corpus, matched in any letter case. Checked only when a
	// password is set (bootstrap, reset, invite accept), never at login.
	if breachedpw.Contains(plaintext) {
		return errs.Invalid("password",
			"is one of the most common passwords. Pick a different one")
	}
	return nil
}

// VerifyPassword does a constant-time bcrypt compare. Returns nil on
// match, a generic ErrBadCredentials on mismatch (no leaking of
// "user exists vs password wrong"), or a wrapped error on
// implementation failures.
func VerifyPassword(hash, plaintext string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return ErrBadCredentials
		}
		return fmt.Errorf("bcrypt compare: %w", err)
	}
	return nil
}

// ErrBadCredentials covers both "no such user" and "wrong password".
// Same error for both so an attacker can't enumerate registered
// emails by timing the response. Handler maps to 401.
var ErrBadCredentials = errs.New("bad_credentials", "invalid email or password")

// wrongPasswordError is ErrBadCredentials for an email that has an account.
// It carries the user id only for the failed-login log line, which must not
// hold the typed email. The response stays the same generic 401.
type wrongPasswordError struct{ userID string }

func (e *wrongPasswordError) Error() string { return ErrBadCredentials.Error() }
func (e *wrongPasswordError) Unwrap() error { return ErrBadCredentials }

// wrongPasswordUserID returns the account's user id when err is a wrong
// password for an existing account, and "" otherwise.
func wrongPasswordUserID(err error) string {
	var wp *wrongPasswordError
	if errors.As(err, &wp) {
		return wp.userID
	}
	return ""
}

// GetByID looks up a user by id. Used by /v1/whoami to project email
// onto the cookie-path response without forcing the session row to
// duplicate it.
func (s *Service) GetByID(ctx context.Context, id string) (domain.User, error) {
	return s.store.GetByID(ctx, id)
}

// Authenticate verifies email + password and returns the user + their tenant
// memberships. Returns ErrBadCredentials for a missing user OR a wrong password —
// the same error, with identical timing on both paths (the not-found path runs a
// dummy bcrypt), so response timing can't be used to enumerate registered emails.
// On success: stamps last_login_at.
//
// There is no account-lock check, and there never is: the sign-in throttle
// (throttle.go) slows guesses before this runs and does not lock an account
// (ADR-094). An account under active attack is handled by password reset /
// session revocation, not by refusing login.
func (s *Service) Authenticate(ctx context.Context, email, plaintext string) (domain.User, []domain.UserTenant, error) {
	u, err := s.store.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			// Run a dummy bcrypt compare so the not-found timing
			// matches the wrong-password timing. Defends against
			// email-enumeration-by-timing attacks.
			_ = bcrypt.CompareHashAndPassword(
				[]byte("$2a$12$AAAAAAAAAAAAAAAAAAAAAOgmlgqkZ7T3cZ6h7xQpqQ6VUCRZx0Aa"),
				[]byte(plaintext))
			return domain.User{}, nil, ErrBadCredentials
		}
		return domain.User{}, nil, err
	}

	if err := VerifyPassword(u.PasswordHash, plaintext); err != nil {
		if errors.Is(err, ErrBadCredentials) {
			return domain.User{}, nil, &wrongPasswordError{userID: u.ID}
		}
		return domain.User{}, nil, err
	}

	tenants, err := s.store.TenantsForUser(ctx, u.ID)
	if err != nil {
		return domain.User{}, nil, fmt.Errorf("load tenants: %w", err)
	}
	if len(tenants) == 0 {
		// User exists but has no tenant — operator data corruption.
		// Refuse login rather than mint a session into nothing.
		return domain.User{}, nil, errs.InvalidState(
			"user has no tenant membership; contact your administrator")
	}

	now := s.clock.Now(ctx)
	_ = s.store.TouchLastLogin(ctx, u.ID, now)
	u.LastLoginAt = &now
	return u, tenants, nil
}

// IssueResetToken generates a fresh reset token for the user with the
// given email, persists the hash, and returns the plaintext (caller
// emails it to the user via the reset link) along with the user's
// tenant_id (caller threads it into the email-outbox row so the
// password-reset email lands on the right tenant for operator
// visibility). Returns ("", "", nil) if the email doesn't match — the
// caller should always render the same "if your email is on file,
// you'll get a link" response so we don't leak account existence.
// IssueResetToken also returns the matched user's ID so the caller can record an
// audit row that POINTS AT the account instead of STORING its email. audit_log is
// append-only (0150 revoked DELETE), so an address written into a row is an
// erasure dead end; a user id is not.
func (s *Service) IssueResetToken(ctx context.Context, email string) (plaintext, tenantID, userID string, err error) {
	u, err := s.store.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return "", "", "", nil
		}
		return "", "", "", err
	}

	tenants, err := s.store.TenantsForUser(ctx, u.ID)
	if err != nil {
		return "", "", "", fmt.Errorf("load tenants for reset: %w", err)
	}
	if len(tenants) == 0 {
		// User exists but isn't attached to any tenant — same posture
		// as Authenticate's "no tenant memberships" check. Refuse to
		// mint a token rather than enqueue an email with empty
		// tenant_id (which the outbox FK would reject anyway).
		return "", "", "", nil
	}

	rawToken, hash, err := generateResetToken()
	if err != nil {
		return "", "", "", err
	}
	expiresAt := s.clock.Now(ctx).Add(PasswordResetTokenTTL)
	if _, err := s.store.CreateResetToken(ctx, u.ID, hash, expiresAt); err != nil {
		return "", "", "", err
	}
	return rawToken, tenants[0].TenantID, u.ID, nil
}

// CheckResetToken returns nil when the reset link can still be used: the
// token is unused and unexpired, and its user belongs to a workspace, the
// same rule ConfirmReset applies. Otherwise it returns the token error. It
// spends nothing; the reset-password page calls it before it shows the
// form, and ConfirmReset calls it before the slow hash.
func (s *Service) CheckResetToken(ctx context.Context, plaintext string) error {
	if plaintext == "" {
		return errs.Invalid("token", "reset token is invalid, expired, or already used")
	}
	tokenHash := hashResetToken(plaintext)
	if _, err := s.store.LookupResetToken(ctx, tokenHash); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return errs.Invalid("token", "reset token is invalid, expired, or already used")
		}
		return err
	}
	return nil
}

// ConfirmReset sets newPassword for the user whose reset token is
// plaintext, and returns that user. It checks the link first and answers a
// dead link with the token error before any bcrypt work, so a dead link sent
// with a bad password gets the token error. It then checks and hashes the
// password outside any transaction: a refused password spends nothing, and
// bcrypt holds no row lock. One transaction then spends the token, voids the
// user's other tokens, sets the hash, ends every session of the user and
// writes the password_reset_completed audit row, so either all of it happens
// or none of it does. A link used up between the check and the transaction
// still gets the token error.
func (s *Service) ConfirmReset(ctx context.Context, plaintext, newPassword string) (domain.User, error) {
	if err := s.CheckResetToken(ctx, plaintext); err != nil {
		return domain.User{}, err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return domain.User{}, err
	}
	// A step's error is formatted with %v, not wrapped: whatever it carries,
	// such as a not-found, it is a fault and answers 500.
	u, err := s.store.ResetPassword(ctx, hashResetToken(plaintext), hash, func(tx *sql.Tx, userID string) error {
		if err := s.sessions.EndAllForUserTx(ctx, tx, userID); err != nil {
			return fmt.Errorf("end sessions: %v", err)
		}
		// The account owner held the token, so they are the actor. The client
		// address and the request id are already on ctx (the root
		// audit-coverage and request-id middlewares put them there).
		if err := s.audit.LogInTx(auth.WithUserID(ctx, userID), tx, audit.Entry{
			Action:       "password_reset_completed",
			ResourceType: "user",
			ResourceID:   userID,
		}); err != nil {
			return fmt.Errorf("write the audit row: %v", err)
		}
		return nil
	})
	if errors.Is(err, errResetLinkDead) {
		return domain.User{}, errs.Invalid("token", "reset token is invalid, expired, or already used")
	}
	if err != nil {
		return domain.User{}, err
	}
	return u, nil
}

// generateResetToken returns (plaintext, hash). Plaintext is 32
// random bytes hex-encoded (64 chars); hash is SHA-256 of the
// plaintext, hex-encoded. Plaintext goes in the email link; hash
// goes in the DB.
func generateResetToken() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("crypto/rand: %w", err)
	}
	plaintext := hex.EncodeToString(buf)
	return plaintext, hashResetToken(plaintext), nil
}

func hashResetToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
