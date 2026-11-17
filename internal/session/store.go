package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Store reads and writes dashboard_sessions rows. Service is its one caller,
// and Service's methods state each contract.
type Store interface {
	InsertTx(ctx context.Context, tx *sql.Tx, s Session) error
	GetByIDHash(ctx context.Context, idHash string) (Session, error)
	EndTx(ctx context.Context, tx *sql.Tx, idHash string) (bool, error)
	EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error
	EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
	EndWith(ctx context.Context, idHash string, then func(tx *sql.Tx, sess Session) error) (Session, bool, error)
	SetLivemodeWith(ctx context.Context, idHash string, livemode bool, then func(tx *sql.Tx, sess Session) error) (Session, bool, error)
}

type postgresStore struct {
	db *postgres.DB
}

// NewPostgresStore returns the Postgres-backed Store. GetByIDHash, EndWith and
// SetLivemodeWith open their own bypass transaction; the …Tx methods run on
// the caller's, which must be one (each Service method says why).
func NewPostgresStore(db *postgres.DB) Store {
	return &postgresStore{db: db}
}

func (s *postgresStore) InsertTx(ctx context.Context, tx *sql.Tx, sess Session) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO dashboard_sessions
			(id_hash, user_id, tenant_id, livemode, created_at,
			 expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, sess.IDHash, sess.UserID, sess.TenantID, sess.Livemode,
		sess.CreatedAt, sess.ExpiresAt, nullStr(sess.UserAgent), nullStr(sess.IP),
	)
	return err
}

// sessionCols are the columns scanSession reads, in its order.
const sessionCols = `id_hash, user_id, tenant_id, livemode, created_at,
	expires_at, revoked_at, user_agent, ip`

// scanSession reads one row of sessionCols.
func scanSession(row *sql.Row) (Session, error) {
	var sess Session
	var ua, ip sql.NullString
	var revokedAt sql.NullTime
	if err := row.Scan(
		&sess.IDHash, &sess.UserID, &sess.TenantID, &sess.Livemode,
		&sess.CreatedAt, &sess.ExpiresAt, &revokedAt,
		&ua, &ip,
	); err != nil {
		return Session{}, err
	}
	if revokedAt.Valid {
		sess.RevokedAt = &revokedAt.Time
	}
	sess.UserAgent = ua.String
	sess.IP = ip.String
	return sess, nil
}

// GetByIDHash returns the row whatever its state; Resolve refuses revoked and
// expired ones.
func (s *postgresStore) GetByIDHash(ctx context.Context, idHash string) (Session, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return Session{}, err
	}
	defer postgres.Rollback(tx)

	sess, err := scanSession(tx.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM dashboard_sessions WHERE id_hash = $1`, idHash))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	return sess, nil
}

func (s *postgresStore) EndTx(ctx context.Context, tx *sql.Tx, idHash string) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE id_hash = $2 AND revoked_at IS NULL AND expires_at > $1
	`, time.Now().UTC(), idHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *postgresStore) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE user_id = $2 AND revoked_at IS NULL
	`, time.Now().UTC(), userID)
	return err
}

func (s *postgresStore) EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE user_id = $2 AND tenant_id = $3 AND revoked_at IS NULL
	`, time.Now().UTC(), userID, tenantID)
	return err
}

// EndWith is Service.End's write, on a bypass transaction of its own.
func (s *postgresStore) EndWith(ctx context.Context, idHash string, then func(tx *sql.Tx, sess Session) error) (Session, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return Session{}, false, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	sess, err := scanSession(tx.QueryRowContext(ctx, `
		UPDATE dashboard_sessions
		SET revoked_at = $1
		WHERE id_hash = $2 AND revoked_at IS NULL AND expires_at > $1
		RETURNING `+sessionCols, now, idHash))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("end session: %w", err)
	}
	if err := auditThenCommit(ctx, tx, sess, then); err != nil {
		return Session{}, false, err
	}
	return sess, true, nil
}

// SetLivemodeWith is Service.SetLivemode's write, on a bypass transaction of
// its own.
func (s *postgresStore) SetLivemodeWith(ctx context.Context, idHash string, livemode bool, then func(tx *sql.Tx, sess Session) error) (Session, bool, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return Session{}, false, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	sess, err := scanSession(tx.QueryRowContext(ctx, `
		UPDATE dashboard_sessions
		SET livemode = $1
		WHERE id_hash = $2 AND revoked_at IS NULL AND expires_at > $3 AND livemode <> $1
		RETURNING `+sessionCols, livemode, idHash, now))
	if errors.Is(err, sql.ErrNoRows) {
		// Not live, or already in this mode.
		sess, err = scanSession(tx.QueryRowContext(ctx, `
			SELECT `+sessionCols+` FROM dashboard_sessions
			WHERE id_hash = $1 AND revoked_at IS NULL AND expires_at > $2`, idHash, now))
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, false, nil
		}
		if err != nil {
			return Session{}, false, fmt.Errorf("read session: %w", err)
		}
		return sess, true, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("set session mode: %w", err)
	}
	if err := auditThenCommit(ctx, tx, sess, then); err != nil {
		return Session{}, false, err
	}
	return sess, true, nil
}

// auditThenCommit finishes EndWith's and SetLivemodeWith's transaction. It
// sets what audit.Logger.LogInTx reads for the row (app.tenant_id, the
// session's tenant, and app.livemode off: account events are filed in test
// mode), calls then and commits.
func auditThenCommit(ctx context.Context, tx *sql.Tx, sess Session, then func(tx *sql.Tx, sess Session) error) error {
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id', $1, true)`, sess.TenantID); err != nil {
		return fmt.Errorf("stamp tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		return fmt.Errorf("stamp livemode: %w", err)
	}
	if err := then(tx, sess); err != nil {
		return err
	}
	return tx.Commit()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
