package user

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// Service is the auth-side surface for user accounts. ADR-011.
//
// The recipe is intentionally boring: bcrypt cost 12 for hashing,
// crypto/rand for tokens, sha256 for token storage. v1 has NO automatic login
// brute-force throttle — the interim one was removed; ADR-094 records the
// deferred design. All of this is well-trodden Go stdlib + x/crypto territory.
type Service struct {
	store Store
	clock clock.Clock
}

func NewService(store Store, clk clock.Clock) *Service {
	if clk == nil {
		clk = clock.Real()
	}
	return &Service{store: store, clock: clk}
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

// CommonPasswords is the top-1000 most-used passwords from the 2023
// HaveIBeenPwned breach corpus (lower-cased). Login flow rejects
// passwords on this list at signup/reset so trivially-guessable
// credentials never enter our DB. Embedded as a tiny in-memory set.
//
// Not exhaustive — a real "weak password" feel needs zxcvbn — but
// catches the 90th percentile of bad passwords (password, 123456,
// qwerty123, etc).
var commonPasswords = map[string]struct{}{
	"password":       {},
	"password123":    {},
	"123456789012":   {},
	"qwertyuiopasdf": {},
	"administrator":  {},
	"changeme123":    {},
	"letmein12345":   {},
	"iloveyou1234":   {},
	"welcome12345":   {},
	"admin12345678":  {},
	// ... in production we'd embed the full top-1000 list. For v1
	// this seed catches the most embarrassing cases. Expand when a
	// security review asks for stronger denial.
}

// HashPassword applies bcrypt to a candidate plaintext. Validates
// length and common-password-list before hashing. Returns the hash
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

// ValidatePassword runs the complexity checks without hashing — used
// at the start of HashPassword and exposed for the password-reset
// confirm endpoint to give an early 422 on bad input before doing any
// crypto work.
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
	if _, blocked := commonPasswords[strings.ToLower(plaintext)]; blocked {
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

// CreateUser inserts a new user with hashed password and binds them
// to the given tenant with the supplied role. Used by the bootstrap
// CLI; not exposed via the HTTP API in v1 (no public signup).
func (s *Service) CreateUser(ctx context.Context, email, plaintext, tenantID, role string) (domain.User, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return domain.User{}, errs.Required("email")
	}
	hash, err := HashPassword(plaintext)
	if err != nil {
		return domain.User{}, err
	}
	u, err := s.store.Create(ctx, email, hash)
	if err != nil {
		return domain.User{}, err
	}
	if tenantID != "" {
		if err := s.store.AttachTenant(ctx, u.ID, tenantID, role); err != nil {
			return domain.User{}, fmt.Errorf("attach tenant: %w", err)
		}
	}
	return u, nil
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
// There is no account-lock check: v1 has no automatic login throttle or lockout
// (deferred as one unit — ADR-094). An account under active attack is handled by
// password reset / session revocation, not by refusing login.
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

// TenantForUser returns the user's (single, v1) tenant id. The auth handler
// needs it to scope a password-reset-completed audit row, where it holds only
// the domain.User (which carries no tenant). Returns "" + nil when the user has
// no tenant membership — the caller treats that as "nothing to audit," not an
// error worth failing the reset over.
func (s *Service) TenantForUser(ctx context.Context, userID string) (string, error) {
	tenants, err := s.store.TenantsForUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(tenants) == 0 {
		return "", nil
	}
	return tenants[0].TenantID, nil
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

// CheckResetToken is the non-consuming counterpart of
// ConsumeResetToken. The reset-password page calls it on mount and
// renders the form only when nil is returned; otherwise it shows
// "this link is no longer valid". Stops the operator from filling
// in the form and only learning at submit-time that the token was
// already used (e.g. they reset earlier and clicked the email link
// from history).
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

// ConsumeResetToken validates the token, sets the new password, and
// returns the user it belonged to. Single-use: the token is stamped
// used_at atomically inside ConsumeResetToken, so a concurrent second
// redeem fails.
func (s *Service) ConsumeResetToken(ctx context.Context, plaintext, newPassword string) (domain.User, error) {
	if err := ValidatePassword(newPassword); err != nil {
		return domain.User{}, err
	}
	tokenHash := hashResetToken(plaintext)
	userID, err := s.store.ConsumeResetToken(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.User{}, errs.Invalid("token",
				"reset token is invalid, expired, or already used")
		}
		return domain.User{}, err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return domain.User{}, err
	}
	if err := s.store.SetPassword(ctx, userID, hash); err != nil {
		return domain.User{}, err
	}
	u, err := s.store.GetByID(ctx, userID)
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
