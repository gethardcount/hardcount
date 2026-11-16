// Package dashmembers is the minimal team-membership surface: invite by
// email, tokenized accept, list, revoke, remove. There is no RBAC (ADR-081):
// every member has the owner's permissions, and role is recorded, not
// enforced. member_invitations stores sha256 token hashes, as
// password_reset_tokens does, with at most one open invitation per (tenant,
// email).
package dashmembers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// PostgresStore persists invitations and reads memberships. Most queries run
// under TxBypass, like the other auth tables (users, sessions,
// password_reset_tokens), because the accept runs before any tenant context
// exists; each query filters by tenant_id itself. CreateInvitation runs under
// TxTenant: the invite email's outbox row and the audit row take the mode
// from its transaction.
//
// A method that takes a hook (then, join) calls it last, on its transaction,
// after its own writes. The hook must not commit or roll back; an error it
// returns rolls everything back and is returned.
type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Invitation is the storage row.
type Invitation struct {
	ID              string
	TenantID        string
	Email           string
	TokenHash       string
	InvitedByUserID string
	Role            string
	ExpiresAt       time.Time
	AcceptedAt      *time.Time
	RevokedAt       *time.Time
	CreatedAt       time.Time
	// InvitedByEmail is the inviter's email, filled by the joins.
	InvitedByEmail string
}

// Member is one (user, tenant) membership row for the members list.
type Member struct {
	UserID   string
	Email    string
	Role     string
	JoinedAt time.Time
}

const invCols = `mi.id, mi.tenant_id, mi.email::text, mi.token_hash, mi.invited_by_user_id,
	mi.role, mi.expires_at, mi.accepted_at, mi.revoked_at, mi.created_at, COALESCE(u.email::text, '')`

func scanInvitation(scan func(...any) error) (Invitation, error) {
	var inv Invitation
	err := scan(&inv.ID, &inv.TenantID, &inv.Email, &inv.TokenHash, &inv.InvitedByUserID,
		&inv.Role, &inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt, &inv.InvitedByEmail)
	return inv, err
}

// ErrPendingInviteExists surfaces the partial-unique conflict (one
// invitation per tenant+email that is not accepted or revoked) as a clean
// 409. CreateInvitation revokes an expired one first, so only an invitation
// that can still be accepted causes it.
var ErrPendingInviteExists = errs.AlreadyExists("email", "a pending invitation for this email already exists — revoke it first to re-send")

// CreateInvitation creates a pending invitation in one tenant transaction. It
// refuses with a 403 when the inviter has no membership row. An invitation to
// the same address that expired (expires_at <= at, the accept's boundary) is
// revoked first, so only one that can still be accepted answers
// ErrPendingInviteExists. then receives the stored row and the id of the
// replaced invitation ("" for none). ctx must carry the mode
// (postgres.WithLivemode): the email and the audit row take theirs from the
// transaction.
func (s *PostgresStore) CreateInvitation(ctx context.Context, inv Invitation, at time.Time, then func(tx *sql.Tx, inv Invitation, replacedID string) error) (Invitation, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, inv.TenantID)
	if err != nil {
		return Invitation{}, err
	}
	defer postgres.Rollback(tx)

	// An invitation carries its sender's authority, so it is created only while
	// the sender is a member. The row lock is held to commit: a removal of the
	// sender waits for this invite and then revokes it, or this invite waits
	// for the removal and then finds no row.
	var member int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM user_tenants WHERE tenant_id = $1 AND user_id = $2 FOR KEY SHARE`,
		inv.TenantID, inv.InvitedByUserID).Scan(&member)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, errs.Forbidden("you are no longer a member of this workspace")
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("lock the inviter's membership: %w", err)
	}

	var replacedID string
	err = tx.QueryRowContext(ctx, `
		UPDATE member_invitations SET revoked_at = $3
		WHERE tenant_id = $1 AND email = $2
		  AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= $3
		RETURNING id`,
		inv.TenantID, inv.Email, at).Scan(&replacedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, fmt.Errorf("revoke the expired invitation: %w", err)
	}

	id := postgres.NewID("hc_minv")
	err = tx.QueryRowContext(ctx, `
		WITH ins AS (
			INSERT INTO member_invitations (id, tenant_id, email, token_hash, invited_by_user_id, role, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING *
		)
		SELECT `+invCols+` FROM ins mi LEFT JOIN users u ON u.id = mi.invited_by_user_id`,
		id, inv.TenantID, inv.Email, inv.TokenHash, inv.InvitedByUserID, inv.Role, inv.ExpiresAt,
	).Scan(&inv.ID, &inv.TenantID, &inv.Email, &inv.TokenHash, &inv.InvitedByUserID,
		&inv.Role, &inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt, &inv.InvitedByEmail)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return Invitation{}, ErrPendingInviteExists
		}
		return Invitation{}, fmt.Errorf("create invitation: %w", err)
	}
	if err := then(tx, inv, replacedID); err != nil {
		return Invitation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Invitation{}, err
	}
	return inv, nil
}

func (s *PostgresStore) ListInvitations(ctx context.Context, tenantID string) ([]Invitation, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT `+invCols+`
		FROM member_invitations mi
		LEFT JOIN users u ON u.id = mi.invited_by_user_id
		WHERE mi.tenant_id = $1
		ORDER BY mi.created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Invitation
	for rows.Next() {
		inv, err := scanInvitation(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// GetInvitationByTokenHash resolves an accept-link token. Returns the row
// regardless of state — the service derives pending/accepted/revoked/
// expired so the accept page can render an honest message.
func (s *PostgresStore) GetInvitationByTokenHash(ctx context.Context, tokenHash string) (Invitation, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return Invitation{}, err
	}
	defer postgres.Rollback(tx)

	inv, err := scanInvitation(tx.QueryRowContext(ctx, `
		SELECT `+invCols+`
		FROM member_invitations mi
		LEFT JOIN users u ON u.id = mi.invited_by_user_id
		WHERE mi.token_hash = $1`, tokenHash).Scan)
	if err == sql.ErrNoRows {
		return Invitation{}, errs.ErrNotFound
	}
	if err != nil {
		return Invitation{}, err
	}
	return inv, nil
}

// RevokeInvitation revokes an invitation that is not yet accepted or revoked,
// in one bypass transaction, stamps it for an audit row (stampForAudit) and
// calls then. An accepted or revoked invitation, or one of another workspace,
// is errs.ErrNotFound; of a revoke and an accept racing, exactly one wins.
func (s *PostgresStore) RevokeInvitation(ctx context.Context, tenantID, id string, at time.Time, then func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE member_invitations SET revoked_at = $1
		WHERE id = $2 AND tenant_id = $3 AND accepted_at IS NULL AND revoked_at IS NULL`,
		at, id, tenantID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errs.ErrNotFound
	}

	if err := stampForAudit(ctx, tx, tenantID); err != nil {
		return err
	}

	if err := then(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AcceptInvitation claims a pending, unexpired invite with a CAS on
// accepted_at in one bypass transaction, stamps it for an audit row in the
// invitation's workspace (stampForAudit), and calls join. The CAS is the
// exactly-once gate: a replayed link, or a concurrent second accept, gets
// errs.ErrNotFound and join does not run. When join fails, nothing is
// written, so the invitation stays pending and the invitee can try again.
func (s *PostgresStore) AcceptInvitation(ctx context.Context, id string, at time.Time, join func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	var tenantID string
	err = tx.QueryRowContext(ctx, `
		UPDATE member_invitations SET accepted_at = $1
		WHERE id = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $1
		RETURNING tenant_id`,
		at, id).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return errs.ErrNotFound
	}
	if err != nil {
		return err
	}

	if err := stampForAudit(ctx, tx, tenantID); err != nil {
		return err
	}

	if err := join(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMembers returns every user attached to the tenant, oldest account first.
func (s *PostgresStore) ListMembers(ctx context.Context, tenantID string) ([]Member, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT ut.user_id, u.email::text, ut.role, u.created_at
		FROM user_tenants ut
		JOIN users u ON u.id = ut.user_id
		WHERE ut.tenant_id = $1
		ORDER BY u.created_at ASC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrLastMember is RemoveMember's refusal to remove a workspace's only
// member. Nobody could sign in to the workspace again, and only psql could
// add a member back.
var ErrLastMember = errs.InvalidState("cannot remove the last member of the workspace")

// RemoveMember removes a member from the workspace in one bypass transaction.
// In order, it deletes the (user, tenant) membership unless it is the
// workspace's last one, revokes every invitation the user sent in this
// workspace that is not yet accepted or revoked, expired or not, stamps the
// transaction for an audit row (stampForAudit), and calls then with the ids
// of the invitations it revoked.
//
// at is the time written as revoked_at on the invitations; the service
// passes its clock's now. The member's sessions, which then ends, are
// stamped by the session store's own clock.
//
// It returns ErrLastMember for the last member, and errs.ErrNotFound when
// userID is not a member, and changes nothing in both cases. The service
// refuses self-removal before calling.
func (s *PostgresStore) RemoveMember(ctx context.Context, tenantID, userID string, at time.Time, then func(tx *sql.Tx, revokedInvitationIDs []string) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// One removal at a time per tenant, with the count inside the DELETE.
	// Otherwise two members removing each other at once each count two
	// members in their own snapshot, each delete the other, and leave none.
	// Behind the lock, the second DELETE starts after the first commits, so
	// its count sees one member and it deletes nothing. The lock is held to
	// commit.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"user_tenants:"+tenantID); err != nil {
		return fmt.Errorf("acquire membership lock: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM user_tenants
		WHERE tenant_id = $1 AND user_id = $2
		  AND (SELECT count(*) FROM user_tenants WHERE tenant_id = $1) > 1`,
		tenantID, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var member bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM user_tenants WHERE tenant_id = $1 AND user_id = $2)`,
			tenantID, userID).Scan(&member); err != nil {
			return err
		}
		if member {
			return ErrLastMember
		}
		return errs.ErrNotFound
	}

	// An invitation carries its sender's authority: accepting it joins the
	// workspace. Once the sender is no longer a member, every invitation they
	// sent that is not yet accepted ends with them. Expired ones are revoked
	// too, so the rule needs no clock.
	rows, err := tx.QueryContext(ctx, `
		UPDATE member_invitations SET revoked_at = $1
		WHERE tenant_id = $2 AND invited_by_user_id = $3
		  AND accepted_at IS NULL AND revoked_at IS NULL
		RETURNING id`,
		at, tenantID, userID)
	if err != nil {
		return fmt.Errorf("revoke the member's invitations: %w", err)
	}
	var revoked []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("revoke the member's invitations: %w", err)
		}
		revoked = append(revoked, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("revoke the member's invitations: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("revoke the member's invitations: %w", err)
	}

	if err := stampForAudit(ctx, tx, tenantID); err != nil {
		return err
	}

	if err := then(tx, revoked); err != nil {
		return err
	}
	return tx.Commit()
}

// stampForAudit sets what audit.Logger.LogInTx reads on a bypass
// transaction: the row's tenant from app.tenant_id, and its mode from
// app.livemode, which takes the request's mode (postgres.Livemode).
func stampForAudit(ctx context.Context, tx *sql.Tx, tenantID string) error {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return err
	}
	livemode := "off"
	if live {
		livemode = "on"
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("stamp tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, livemode); err != nil {
		return fmt.Errorf("stamp livemode: %w", err)
	}
	return nil
}
