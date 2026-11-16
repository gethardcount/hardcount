// Package dashmembers implements the minimal team-membership surface
// (DP-readiness cut-reinstatement #2, 2026-07-06): invite by email,
// tokenized accept, member list, revoke, remove. Deliberately NO RBAC —
// every member gets the full owner permission set (auth.KeyTypeSession);
// the role column is recorded for the future role split but not enforced
// (internal/session/middleware.go documents the seam). The point is to
// kill the shared-password reality: per-person credentials and honest
// audit-log actor attribution on a revenue system.
//
// Storage: member_invitations (migration 0139 — an earlier 0035 version
// belonged to the cut Members feature and was dropped in 0068). sha256
// token hashes mirroring password_reset_tokens; partial-unique one
// PENDING invite per (tenant, email).
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
// under TxBypass like the sibling auth tables (users, sessions,
// password_reset_tokens): the accept path runs before any tenant context
// exists. tenant_id predicates scope every tenant-shaped query at the
// application layer; the table still carries the 0006 RLS fence
// (bypass-admitting, m0139) as the structural backstop for any future
// tenant-scoped query. CreateInvitation runs under TxTenant instead: the
// invite email's outbox row and the audit row commit on its transaction and
// need the mode.
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
	// InvitedByEmail is hydrated by the list/lookup joins (users has no
	// display_name — email is the identity, per the audit-hardening
	// convention).
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

// CreateInvitation creates a pending invitation in one tenant transaction.
// In order, it revokes the workspace's invitation to the same address when
// that one has expired and is not yet accepted or revoked, inserts inv, and
// calls then with the stored row and the id of the invitation it revoked (""
// when there was none). then runs last, on the same transaction. It must not
// commit or roll back tx; an error it returns rolls the whole invite back and
// is returned.
//
// at is the time written as revoked_at, and an invitation counts as expired
// when its expires_at is at or before it, the same boundary the accept uses.
// The service passes its clock's now. An invitation to the address that has
// not expired keeps its slot: the insert then returns ErrPendingInviteExists
// and nothing changes.
//
// ctx must carry the mode (postgres.WithLivemode). BeginTx stamps
// app.tenant_id and app.livemode on a tenant transaction, and the
// email_outbox and audit_log inserts take their mode from it.
func (s *PostgresStore) CreateInvitation(ctx context.Context, inv Invitation, at time.Time, then func(tx *sql.Tx, inv Invitation, replacedID string) error) (Invitation, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, inv.TenantID)
	if err != nil {
		return Invitation{}, err
	}
	defer postgres.Rollback(tx)

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

// RevokeInvitation revokes invitation id of tenantID in one bypass
// transaction. In order, it writes at as revoked_at when the invitation is
// not yet accepted or revoked, stamps app.tenant_id with tenantID and
// app.livemode with the request's mode (postgres.Livemode) for an audit row,
// and calls then. then runs last, on the same transaction. It must not commit
// or roll back tx; an error it returns rolls the revoke back and is returned.
//
// An accepted or already revoked invitation, or one of another workspace, is
// errs.ErrNotFound and nothing changes. The compare-and-set keeps a revoke
// racing an accept honest: exactly one wins.
func (s *PostgresStore) RevokeInvitation(ctx context.Context, tenantID, id string, at time.Time, then func(tx *sql.Tx) error) error {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return err
	}
	livemode := "off"
	if live {
		livemode = "on"
	}

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

	// audit.Logger.LogInTx reads the row's tenant from app.tenant_id and its
	// mode from app.livemode.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("stamp tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, livemode); err != nil {
		return fmt.Errorf("stamp livemode: %w", err)
	}

	if err := then(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AcceptInvitation claims a pending, unexpired invite with a CAS on
// accepted_at, then calls join on the same transaction to write the account
// and the membership. The claim and the membership commit together or not at
// all. The CAS is the exactly-once gate: a replayed link, or a concurrent
// second accept, gets errs.ErrNotFound and join does not run. When join
// fails, nothing is written and its error is returned, so the invitation
// stays pending and the invitee can try again.
func (s *PostgresStore) AcceptInvitation(ctx context.Context, id string, at time.Time, join func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE member_invitations SET accepted_at = $1
		WHERE id = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $1`,
		at, id)
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
	if err := join(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMembers returns every user attached to the tenant. users has NO
// display_name column (audit-hardening convention: the email IS the
// identity), so the view carries email only.
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

// RemoveMember removes a member from the workspace in one bypass
// transaction. In order, it deletes the (user, tenant) membership unless it
// is the workspace's last one, revokes every invitation the user sent in this
// workspace that is not yet accepted or revoked, expired or not, stamps
// app.tenant_id with tenantID and app.livemode with the request's mode
// (postgres.Livemode) for an audit row, and calls then with the ids of the
// invitations it revoked. then runs last, on the same transaction. It must
// not commit or roll back tx; an error it returns rolls the whole removal
// back and is returned.
//
// at is the time written as revoked_at on the invitations; the service
// passes its clock's now. The member's sessions, which then ends, are
// stamped by the session store's own clock.
//
// It returns ErrLastMember for the last member, and errs.ErrNotFound when
// userID is not a member, and changes nothing in both cases. The service
// refuses self-removal before calling.
func (s *PostgresStore) RemoveMember(ctx context.Context, tenantID, userID string, at time.Time, then func(tx *sql.Tx, revokedInvitationIDs []string) error) error {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return err
	}
	livemode := "off"
	if live {
		livemode = "on"
	}

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
	// too, so the rule needs no clock and an expired invitation does not keep
	// its address's pending slot.
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

	// audit.Logger.LogInTx reads the row's tenant from app.tenant_id and its
	// mode from app.livemode.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("stamp tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, livemode); err != nil {
		return fmt.Errorf("stamp livemode: %w", err)
	}

	if err := then(tx, revoked); err != nil {
		return err
	}
	return tx.Commit()
}
