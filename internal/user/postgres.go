package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// PostgresStore is the production persistence for user accounts. All
// queries run under TxBypass — there is no tenant context for users
// (a user *belongs to* tenants via user_tenants but the user row
// itself is global). Auth resolution happens before any tenant ctx is
// available.
type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

const userCols = `id, email::text, password_hash, created_at, last_login_at`

func scanUser(row *sql.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt, &u.LastLoginAt)
	return u, err
}

// ErrEmailTaken is returned when CreateInTx hits the email unique
// violation. Surfaced to the API as 409.
var ErrEmailTaken = errs.AlreadyExists("email", "an account with this email already exists")

func (s *PostgresStore) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.User{}, err
	}
	defer postgres.Rollback(tx)

	row := tx.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE email = $1`, strings.TrimSpace(email))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, errs.ErrNotFound
	}
	return u, err
}

func (s *PostgresStore) GetByID(ctx context.Context, id string) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.User{}, err
	}
	defer postgres.Rollback(tx)

	row := tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, errs.ErrNotFound
	}
	return u, err
}

// errPasswordChanged is SignIn's answer when the user's password hash is no
// longer the one the caller verified: the password changed, or the user was
// deleted, while the sign-in ran. The service answers it as a wrong
// password.
var errPasswordChanged = errors.New("user: the password changed after it was verified")

// errNoWorkspace is SignIn's answer for a user who belongs to no workspace.
var errNoWorkspace = errors.New("user: the user belongs to no workspace")

// SignIn records a sign-in in one bypass transaction. In order, it locks the
// user's row and checks that its password hash is still verifiedHash, reads
// the user's first-joined workspace (the order TenantsForUser uses) with a
// lock on that membership row, writes at as last_login_at, stamps
// app.tenant_id with the workspace and app.livemode off for an audit row,
// and calls then. then runs last, on the same transaction, with the
// workspace. It must not commit or roll back tx; an error it returns rolls
// the whole sign-in back and is returned.
//
// It returns errPasswordChanged when the hash is no longer verifiedHash or
// the user's row is gone, and errNoWorkspace when the user belongs to no
// workspace, and changes nothing in both cases.
//
// The row locks are what keep the sign-in true when it commits. A password
// reset updates the user's row, so the lock waits for a reset in flight and
// the check then sees its new hash. A member removal deletes the membership
// row and ends the member's sessions in its own transaction. Either the lock
// waits for the removal, and the read then finds the user's next membership
// (a user with one workspace has none and gets errNoWorkspace; a user with
// more signs in to the next one), or the removal waits for this commit and
// its sign-out ends the session this sign-in created.
//
// FOR UPDATE, not FOR SHARE, on the user's row: this transaction updates the
// row, and two sign-ins of one user that each held a share lock would
// deadlock on that update. FOR KEY SHARE on the membership row: it conflicts
// with the removal's delete, and not with another sign-in or an update that
// leaves the row's key alone, so sign-ins of one workspace's members do not
// queue behind each other.
func (s *PostgresStore) SignIn(ctx context.Context, userID, verifiedHash string, at time.Time, then func(tx *sql.Tx, tenantID string) error) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)

	var hash string
	err = tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errPasswordChanged
	}
	if err != nil {
		return "", fmt.Errorf("lock user: %w", err)
	}
	if hash != verifiedHash {
		return "", errPasswordChanged
	}

	var tenantID string
	err = tx.QueryRowContext(ctx, `
		SELECT tenant_id FROM user_tenants WHERE user_id = $1
		ORDER BY created_at ASC, tenant_id ASC
		LIMIT 1
		FOR KEY SHARE
	`, userID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNoWorkspace
	}
	if err != nil {
		return "", fmt.Errorf("find workspace: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE users SET last_login_at = $1 WHERE id = $2`, at, userID); err != nil {
		return "", fmt.Errorf("stamp last sign-in: %w", err)
	}

	// audit.Logger.LogInTx reads the row's tenant from app.tenant_id and its
	// mode from app.livemode. Account events are filed in test mode.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return "", fmt.Errorf("stamp tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		return "", fmt.Errorf("stamp livemode: %w", err)
	}

	if err := then(tx, tenantID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return tenantID, nil
}

// CreateInTx inserts a user and their tenant attachment inside the
// CALLER's transaction — the seam tenant.RunBootstrap needs so the
// owner user commits (or rolls back) atomically with the tenant, keys,
// and settings it belongs to (ADR-073). The invite accept uses it too,
// so a new account commits with the invitation's claim. passwordHash must
// already be a bcrypt hash (user.HashPassword validates + hashes). No
// commit here; the caller owns the tx.
func (s *PostgresStore) CreateInTx(ctx context.Context, tx *sql.Tx, email, passwordHash, tenantID, role string) (domain.User, error) {
	row := tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING `+userCols, strings.TrimSpace(email), passwordHash)
	u, err := scanUser(row)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.User{}, ErrEmailTaken
		}
		return domain.User{}, fmt.Errorf("create user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_tenants (user_id, tenant_id, role)
		VALUES ($1, $2, $3)
	`, u.ID, tenantID, role); err != nil {
		return domain.User{}, fmt.Errorf("attach tenant: %w", err)
	}
	return u, nil
}

// ErrMemberElsewhere refuses a second workspace for an account. Sign-in
// opens one workspace per address, so a second membership could never be
// reached (ADR-081). The accept page shows the message to the invited person.
var ErrMemberElsewhere = errs.AlreadyExists("", "This address already signs in to another workspace on this server, and an address can belong to one. Ask the person who invited you to use a different address.").WithCode("member_elsewhere")

// AttachTenantInTx adds (user_id, tenant_id, role) to user_tenants on the
// caller's transaction, and does nothing when the membership already exists.
// It returns ErrMemberElsewhere, and writes nothing, when the user is a
// member of another workspace. The invite accept uses it so the membership
// commits with the invitation's claim. No commit here; the caller owns the tx.
//
// tx must be a bypass transaction: row-level security hides other workspaces'
// memberships from a tenant one. The user's row is locked first, as SignIn
// locks it, so of two accepts of one account into two workspaces at once the
// second waits and is refused.
func (s *PostgresStore) AttachTenantInTx(ctx context.Context, tx *sql.Tx, userID, tenantID, role string) error {
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return fmt.Errorf("lock user: %w", err)
	}
	if err := checkJoin(ctx, tx, userID, tenantID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_tenants (user_id, tenant_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, tenant_id) DO NOTHING
	`, userID, tenantID, role); err != nil {
		return fmt.Errorf("attach tenant: %w", err)
	}
	return nil
}

// CheckJoin returns ErrMemberElsewhere when AttachTenantInTx would refuse the
// user for tenantID, and nil otherwise. It is a plain read for the accept
// page: a membership can change before the accept, which checks again under
// the user's row lock.
func (s *PostgresStore) CheckJoin(ctx context.Context, userID, tenantID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	return checkJoin(ctx, tx, userID, tenantID)
}

// checkJoin returns ErrMemberElsewhere when the user is a member of a
// workspace other than tenantID. tx must be a bypass transaction.
func checkJoin(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	var elsewhere bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM user_tenants WHERE user_id = $1 AND tenant_id <> $2)
	`, userID, tenantID).Scan(&elsewhere)
	if err != nil {
		return fmt.Errorf("look for another workspace: %w", err)
	}
	if elsewhere {
		return ErrMemberElsewhere
	}
	return nil
}

func (s *PostgresStore) TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	// Ordered by join time, so an account that holds several memberships
	// from before the one-workspace rule (ADR-081) gets its first-joined one.
	rows, err := tx.QueryContext(ctx,
		`SELECT user_id, tenant_id, role FROM user_tenants WHERE user_id = $1
		 ORDER BY created_at ASC, tenant_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []domain.UserTenant
	for rows.Next() {
		var ut domain.UserTenant
		if err := rows.Scan(&ut.UserID, &ut.TenantID, &ut.Role); err != nil {
			return nil, err
		}
		out = append(out, ut)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateResetToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) (domain.PasswordResetToken, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.PasswordResetToken{}, err
	}
	defer postgres.Rollback(tx)

	// Per-account send cap, decided inside this transaction. The user row is
	// locked FIRST so two concurrent requests for one account cannot both
	// read "2 sent" and both insert — the count and the insert are one
	// decision. Rows in password_reset_tokens are the durable record of what
	// was sent, so this cap is cluster-wide and needs no external store.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return domain.PasswordResetToken{}, fmt.Errorf("lock user for reset cap: %w", err)
	}
	var sent int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM password_reset_tokens
		WHERE user_id = $1 AND created_at > now() - $2::interval
	`, userID, ResetSendWindow.String()).Scan(&sent); err != nil {
		return domain.PasswordResetToken{}, fmt.Errorf("count recent reset tokens: %w", err)
	}
	if sent >= ResetSendsPerWindow {
		return domain.PasswordResetToken{}, ErrResetSendCapped
	}

	var t domain.PasswordResetToken
	row := tx.QueryRowContext(ctx, `
		INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, token_hash, expires_at, used_at, created_at
	`, userID, tokenHash, expiresAt)
	err = row.Scan(&t.ID, &t.UserID, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return domain.PasswordResetToken{}, fmt.Errorf("create reset token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.PasswordResetToken{}, err
	}
	return t, nil
}

// errResetLinkDead is ResetPassword's answer for a link that cannot be
// used: the token is unknown, expired or used, or its user belongs to no
// workspace. Only this error becomes the dead-link answer; any other error,
// including one from the hook, is a fault.
var errResetLinkDead = errors.New("user: reset link cannot be used")

// ResetPassword redeems a reset token and sets the user's password hash in
// one bypass transaction. In order, it spends the token, voids the user's
// other unused tokens, writes passwordHash, finds the user's first-joined
// workspace (the one login picks), stamps app.tenant_id with it and
// app.livemode off for an audit row, and calls then. then runs last, on the
// same transaction, with the token's owner. It must not commit or roll back
// tx; an error it returns rolls the whole reset back and is returned.
//
// It returns errResetLinkDead, and changes nothing, when the token is
// unknown, expired or used, or when the user belongs to no workspace: such a
// user cannot sign in, and an audit row has no tenant to go to.
func (s *PostgresStore) ResetPassword(ctx context.Context, tokenHash, passwordHash string, then func(tx *sql.Tx, userID string) error) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.User{}, err
	}
	defer postgres.Rollback(tx)

	// The conditional UPDATE is the single-use guard: of two concurrent
	// redeems, only one sees the row unused.
	var userID string
	err = tx.QueryRowContext(ctx, `
		UPDATE password_reset_tokens
		SET used_at = now()
		WHERE token_hash = $1
		  AND used_at IS NULL
		  AND expires_at > now()
		RETURNING user_id
	`, tokenHash).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, errResetLinkDead
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("spend reset token: %w", err)
	}

	// Anyone who knows the email can request a reset, so a user can hold
	// several live tokens. Once one resets the password, an earlier link
	// (from a mailbox since recovered, or forwarded) must not reset it again.
	if _, err := tx.ExecContext(ctx, `
		UPDATE password_reset_tokens
		SET used_at = now()
		WHERE user_id = $1
		  AND token_hash <> $2
		  AND used_at IS NULL
	`, userID, tokenHash); err != nil {
		return domain.User{}, fmt.Errorf("void other reset tokens: %w", err)
	}

	u, err := scanUser(tx.QueryRowContext(ctx, `
		UPDATE users SET password_hash = $1 WHERE id = $2
		RETURNING `+userCols, passwordHash, userID))
	if err != nil {
		return domain.User{}, fmt.Errorf("set password: %w", err)
	}

	// Same order as TenantsForUser, so this is the workspace login picks.
	var tenantID string
	err = tx.QueryRowContext(ctx, `
		SELECT tenant_id FROM user_tenants WHERE user_id = $1
		ORDER BY created_at ASC, tenant_id ASC
		LIMIT 1
	`, userID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, errResetLinkDead
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("find workspace: %w", err)
	}

	// audit.Logger.LogInTx reads the row's tenant from app.tenant_id and its
	// mode from app.livemode. Account events are filed in test mode.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return domain.User{}, fmt.Errorf("stamp tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		return domain.User{}, fmt.Errorf("stamp livemode: %w", err)
	}

	if err := then(tx, userID); err != nil {
		return domain.User{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.User{}, err
	}
	return u, nil
}

// LookupResetToken returns the user id of a reset link that can still be
// used: the token is unused and unexpired, and its user belongs to a
// workspace. That is the rule ResetPassword applies, without spending the
// token. Returns errs.ErrNotFound otherwise.
func (s *PostgresStore) LookupResetToken(ctx context.Context, tokenHash string) (string, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return "", err
	}
	defer postgres.Rollback(tx)
	var userID string
	row := tx.QueryRowContext(ctx, `
		SELECT t.user_id FROM password_reset_tokens t
		WHERE t.token_hash = $1
		  AND t.used_at IS NULL
		  AND t.expires_at > now()
		  AND EXISTS (SELECT 1 FROM user_tenants ut WHERE ut.user_id = t.user_id)
	`, tokenHash)
	if err := row.Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errs.ErrNotFound
		}
		return "", err
	}
	return userID, nil
}
