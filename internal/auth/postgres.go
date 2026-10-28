package auth

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type PostgresStore struct {
	db *postgres.DB
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

const keyCols = `id, key_prefix, key_type, livemode, name, tenant_id, created_at, expires_at, revoked_at, last_used_at`

func scanKey(row interface{ Scan(...any) error }) (domain.APIKey, error) {
	var k domain.APIKey
	err := row.Scan(&k.ID, &k.KeyPrefix, &k.KeyType, &k.Livemode, &k.Name, &k.TenantID,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt)
	return k, err
}

func (s *PostgresStore) Create(ctx context.Context, key domain.APIKey) (domain.APIKey, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, key.TenantID)
	if err != nil {
		return domain.APIKey{}, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	k, err := scanKey(tx.QueryRowContext(ctx, `
		INSERT INTO api_keys (id, key_prefix, key_hash, key_salt, key_type, livemode, name, tenant_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+keyCols,
		key.ID, key.KeyPrefix, key.KeyHash, key.KeySalt, key.KeyType, key.Livemode, key.Name, key.TenantID, now,
		postgres.NullableTime(key.ExpiresAt),
	))
	if err != nil {
		return domain.APIKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.APIKey{}, err
	}
	return k, nil
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (domain.APIKey, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.APIKey{}, err
	}
	defer postgres.Rollback(tx)

	k, err := scanKey(tx.QueryRowContext(ctx,
		`SELECT `+keyCols+` FROM api_keys WHERE id = $1`, id))
	if err == sql.ErrNoRows {
		return domain.APIKey{}, errs.ErrNotFound
	}
	return k, err
}

func (s *PostgresStore) GetByPrefix(ctx context.Context, prefix string) (domain.APIKey, error) {
	// API key lookup bypasses RLS — we need to find the key to determine the tenant.
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.APIKey{}, err
	}
	defer postgres.Rollback(tx)

	var key domain.APIKey
	err = tx.QueryRowContext(ctx, `
		SELECT `+keyCols+`, key_hash, key_salt
		FROM api_keys
		WHERE key_prefix = $1 AND revoked_at IS NULL
	`, prefix).Scan(&key.ID, &key.KeyPrefix, &key.KeyType, &key.Livemode, &key.Name, &key.TenantID,
		&key.CreatedAt, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt, &key.KeyHash, &key.KeySalt)

	if err == sql.ErrNoRows {
		return domain.APIKey{}, errs.ErrNotFound
	}
	return key, err
}

// Revoke flips revoked_at on the target key. Idempotent: revoking an
// already-revoked or expired key returns ErrNotFound (the UPDATE
// matches zero rows). Caller decides how to surface that.
//
// Pre-ADR-011 included a last-active-secret/platform safeguard that
// refused to leave the tenant with zero active manage-keys-capable
// credentials — necessary when the API key was the dashboard sign-in
// credential. With user accounts (ADR-011), revoking all API keys
// no longer locks the tenant out: the operator still signs in via
// email + password and creates new keys.
func (s *PostgresStore) Revoke(ctx context.Context, tenantID, id string) (domain.APIKey, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.APIKey{}, err
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	k, err := scanKey(tx.QueryRowContext(ctx, `
		UPDATE api_keys SET revoked_at = $1
		WHERE id = $2 AND revoked_at IS NULL
		RETURNING `+keyCols, now, id))
	if err == sql.ErrNoRows {
		return domain.APIKey{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.APIKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.APIKey{}, err
	}
	return k, nil
}

func (s *PostgresStore) ScheduleExpiry(ctx context.Context, tenantID, id string, expiresAt time.Time) (domain.APIKey, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.APIKey{}, err
	}
	defer postgres.Rollback(tx)

	// Only schedule expiry on keys that aren't already revoked — rotating a
	// revoked key makes no sense, and the predicate guards against racing
	// revoke+rotate for the same row.
	k, err := scanKey(tx.QueryRowContext(ctx, `
		UPDATE api_keys SET expires_at = $1
		WHERE id = $2 AND revoked_at IS NULL
		RETURNING `+keyCols, expiresAt, id))
	if err == sql.ErrNoRows {
		return domain.APIKey{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.APIKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.APIKey{}, err
	}
	return k, nil
}

// List returns one page of the tenant's API keys, newest first, and how many
// keys match.
func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.APIKey, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// The handler sends a limit of 1 to 100 (respond.ParsePage).
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	where := ""
	args := []any{}
	if filter.Role != "" {
		where = " WHERE key_type = $1"
		args = append(args, filter.Role)
	}
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, filter.Offset)
	query := `SELECT ` + keyCols + ` FROM api_keys` + where +
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var keys []domain.APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, 0, err
		}
		keys = append(keys, k)
	}
	return keys, total, rows.Err()
}

func (s *PostgresStore) TouchLastUsed(ctx context.Context, id string, usedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(ctx, `UPDATE api_keys SET last_used_at = $1 WHERE id = $2`, usedAt, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
