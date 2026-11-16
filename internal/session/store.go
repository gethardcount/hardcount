package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Store reads and writes dashboard_sessions rows. A session is bound to a
// user, not to a key (ADR-011), so nothing here revokes sessions by key.
type Store interface {
	Insert(ctx context.Context, s Session) error
	GetByIDHash(ctx context.Context, idHash string) (Session, error)
	Revoke(ctx context.Context, idHash string) error
	EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error
	EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
	UpdateLivemode(ctx context.Context, idHash string, livemode bool) error
}

type postgresStore struct {
	db *postgres.DB
}

// NewPostgresStore returns the Postgres-backed Store. Sessions are queried
// by id_hash (the primary key), so RLS is not strictly needed: the id space
// has no overlap across tenants. Inserts and tenant-scoped reads run inside
// TxBypass, because the session id is itself the auth boundary.
func NewPostgresStore(db *postgres.DB) Store {
	return &postgresStore{db: db}
}

func (s *postgresStore) Insert(ctx context.Context, sess Session) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dashboard_sessions
			(id_hash, user_id, tenant_id, livemode, created_at,
			 last_seen_at, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8)
	`, sess.IDHash, sess.UserID, sess.TenantID, sess.Livemode,
		sess.CreatedAt, sess.ExpiresAt, nullStr(sess.UserAgent), nullStr(sess.IP),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) GetByIDHash(ctx context.Context, idHash string) (Session, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return Session{}, err
	}
	defer postgres.Rollback(tx)

	var sess Session
	var ua, ip sql.NullString
	var revokedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id_hash, user_id, tenant_id, livemode, created_at,
		       last_seen_at, expires_at, revoked_at, user_agent, ip
		FROM dashboard_sessions WHERE id_hash = $1
	`, idHash).Scan(
		&sess.IDHash, &sess.UserID, &sess.TenantID, &sess.Livemode,
		&sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &revokedAt,
		&ua, &ip,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	if revokedAt.Valid {
		sess.RevokedAt = &revokedAt.Time
	}
	sess.UserAgent = ua.String
	sess.IP = ip.String
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	return sess, nil
}

func (s *postgresStore) Revoke(ctx context.Context, idHash string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE id_hash = $2 AND revoked_at IS NULL
	`, time.Now().UTC(), idHash); err != nil {
		return err
	}
	return tx.Commit()
}

// EndAllForUserTx revokes every active session of a user, in every workspace
// and mode, on the caller's transaction. It does not commit. The caller's
// transaction must see every session row: a bypass transaction does, while a
// tenant transaction's row-level security hides the other workspaces and
// mode.
func (s *postgresStore) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE user_id = $2 AND revoked_at IS NULL
	`, time.Now().UTC(), userID)
	return err
}

// EndForMemberTx revokes every active session of a user in one workspace, in
// both modes, on the caller's transaction. It does not commit. The caller's
// transaction must see the sessions of both modes: a bypass transaction does,
// while a tenant transaction's row-level security hides the other mode.
func (s *postgresStore) EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE user_id = $2 AND tenant_id = $3 AND revoked_at IS NULL
	`, time.Now().UTC(), userID, tenantID)
	return err
}

func (s *postgresStore) UpdateLivemode(ctx context.Context, idHash string, livemode bool) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET livemode = $1
		WHERE id_hash = $2 AND revoked_at IS NULL
	`, livemode, idHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
