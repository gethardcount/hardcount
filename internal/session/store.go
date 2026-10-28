package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Store is the persistence interface for dashboard_sessions rows.
// Narrow on purpose — the dashboard auth flow only needs Insert,
// GetByIDHash, Revoke. Any further surface (pruning expired rows,
// listing for an ops UI) belongs on a sibling type when it ships,
// not here.
//
// Pre-ADR-011 included RevokeAllForKey for the API-key-revoke
// fan-out path; that's gone now that sessions are user-bound and
// independent of API key lifecycle.
type Store interface {
	Insert(ctx context.Context, s Session) error
	GetByIDHash(ctx context.Context, idHash string) (Session, error)
	Revoke(ctx context.Context, idHash string) error
	RevokeAllForUser(ctx context.Context, userID string) error
	UpdateLivemode(ctx context.Context, idHash string, livemode bool) error
}

type postgresStore struct {
	db *postgres.DB
}

// NewPostgresStore wires the postgres-backed implementation. Sessions
// query by id_hash (PK) so RLS isn't strictly necessary — there's no
// cross-tenant overlap on the id space — but inserts and tenant-scoped
// reads still run inside TxBypass since the session id is the auth
// boundary itself.
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

// RevokeAllForUser revokes every active session for a user in one
// statement. Backs the password-reset flow: changing the password
// must invalidate any session a thief minted from a stolen cookie,
// not let it ride out the 7-day TTL. Uses idx_dashboard_sessions_user
// (migration 0070). Idempotent — already-revoked rows are skipped by
// the revoked_at IS NULL guard.
func (s *postgresStore) RevokeAllForUser(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE user_id = $2 AND revoked_at IS NULL
	`, time.Now().UTC(), userID); err != nil {
		return err
	}
	return tx.Commit()
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
