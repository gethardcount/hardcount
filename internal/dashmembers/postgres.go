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
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// PostgresStore persists invitations and reads memberships. Queries run
// under TxBypass like the sibling auth tables (users, sessions,
// password_reset_tokens): the accept path runs before any tenant context
// exists. tenant_id predicates scope every tenant-shaped query at the
// application layer; the table still carries the 0006 RLS fence
// (bypass-admitting, m0139) as the structural backstop for any future
// tenant-scoped query.
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
// pending invite per tenant+email) as a clean 409.
var ErrPendingInviteExists = errs.AlreadyExists("email", "a pending invitation for this email already exists — revoke it first to re-send")

func (s *PostgresStore) CreateInvitation(ctx context.Context, inv Invitation) (Invitation, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return Invitation{}, err
	}
	defer postgres.Rollback(tx)

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

// RevokeInvitation stamps revoked_at on a PENDING invite (tenant-scoped).
// Already-accepted or already-revoked rows are not-found — the CAS keeps
// revoke racing accept honest: exactly one wins.
func (s *PostgresStore) RevokeInvitation(ctx context.Context, tenantID, id string, at time.Time) error {
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
	if n, _ := res.RowsAffected(); n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

// AcceptInvitation CASes accepted_at on a pending, unexpired invite —
// the exactly-once gate for the accept flow (a replayed link or a
// concurrent second accept loses the CAS and reads the fresh state).
func (s *PostgresStore) AcceptInvitation(ctx context.Context, id string, at time.Time) error {
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
	if n, _ := res.RowsAffected(); n == 0 {
		return errs.ErrNotFound
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

// RemoveMember deletes the (user, tenant) membership. The service guards
// self-removal and last-member removal before calling.
func (s *PostgresStore) RemoveMember(ctx context.Context, tenantID, userID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx,
		`DELETE FROM user_tenants WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}
