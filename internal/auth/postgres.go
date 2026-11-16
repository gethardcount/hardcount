package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// store reads and writes api_keys. A method that takes a tx runs on the
// caller's transaction; the others open and end their own.
type store struct {
	db *postgres.DB
}

// ListFilter selects a page of a tenant's keys.
type ListFilter struct {
	TenantID string
	Limit    int
	Offset   int
}

const keyCols = `id, key_prefix, key_type, livemode, name, tenant_id, created_at, expires_at, revoked_at, last_used_at`

// scanKey scans keyCols, then extra, and sets Status as of now (statusAt).
func scanKey(row interface{ Scan(...any) error }, extra ...any) (domain.APIKey, error) {
	var k domain.APIKey
	dest := append([]any{&k.ID, &k.KeyPrefix, &k.KeyType, &k.Livemode, &k.Name, &k.TenantID,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return domain.APIKey{}, err
	}
	k.Status = statusAt(k, time.Now().UTC())
	return k, nil
}

// notFound returns errs.ErrNotFound for a query that matched no row.
func notFound(k domain.APIKey, err error) (domain.APIKey, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.APIKey{}, errs.ErrNotFound
	}
	return k, err
}

// insert stores key on tx and returns the stored row. The table's trigger
// sets livemode from the transaction's app.livemode, and
// api_keys_prefix_shape_check refuses a prefix whose mode differs.
func (s store) insert(ctx context.Context, tx *sql.Tx, key domain.APIKey, h storedHash) (domain.APIKey, error) {
	return scanKey(tx.QueryRowContext(ctx, `
		INSERT INTO api_keys (id, key_prefix, key_hash, key_salt, key_type, name, tenant_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+keyCols,
		key.ID, key.KeyPrefix, h.hash, h.salt, key.KeyType, key.Name, key.TenantID, time.Now().UTC(),
		postgres.NullableTime(key.ExpiresAt),
	))
}

// getForUpdate returns the key with id and locks its row until tx ends, or
// returns errs.ErrNotFound. Row-level security limits it to the
// transaction's tenant and mode.
func (s store) getForUpdate(ctx context.Context, tx *sql.Tx, id string) (domain.APIKey, error) {
	return notFound(scanKey(tx.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE id = $1 FOR UPDATE`, id)))
}

// revoke sets revoked_at on the key with id and returns the row. A key
// already revoked returns errs.ErrNotFound: the UPDATE matches no row.
func (s store) revoke(ctx context.Context, tx *sql.Tx, id string) (domain.APIKey, error) {
	return notFound(scanKey(tx.QueryRowContext(ctx, `
		UPDATE api_keys SET revoked_at = $1
		WHERE id = $2 AND revoked_at IS NULL
		RETURNING `+keyCols, time.Now().UTC(), id)))
}

// scheduleExpiry moves the unrevoked key's expires_at to expiresAt, never
// later, and returns the row or errs.ErrNotFound.
func (s store) scheduleExpiry(ctx context.Context, tx *sql.Tx, id string, expiresAt time.Time) (domain.APIKey, error) {
	return notFound(scanKey(tx.QueryRowContext(ctx, `
		-- LEAST ignores a NULL, so a key with no expiry takes the grace end.
		UPDATE api_keys SET expires_at = LEAST(expires_at, $1)
		WHERE id = $2 AND revoked_at IS NULL
		RETURNING `+keyCols, expiresAt, id)))
}

// getByPrefix returns the key with the given lookup prefix, with its status
// and its stored salt and hash, or errs.ErrNotFound. The prefix is unique
// (api_keys_key_prefix_key).
func (s store) getByPrefix(ctx context.Context, prefix string) (domain.APIKey, storedHash, error) {
	// The lookup bypasses RLS, because the key must be found before its
	// tenant is known.
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.APIKey{}, storedHash{}, err
	}
	defer postgres.Rollback(tx)

	var h storedHash
	key, err := notFound(scanKey(tx.QueryRowContext(ctx, `
		SELECT `+keyCols+`, key_hash, key_salt
		FROM api_keys
		WHERE key_prefix = $1
	`, prefix), &h.hash, &h.salt))
	if err != nil {
		return domain.APIKey{}, storedHash{}, err
	}
	return key, h, nil
}

// list returns one page of the tenant's API keys, newest first, and how many
// keys match.
func (s store) list(ctx context.Context, filter ListFilter) ([]domain.APIKey, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys
		ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, filter.Limit, filter.Offset)
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

// touchLastUsed sets the key's last_used_at to usedAt.
func (s store) touchLastUsed(ctx context.Context, id string, usedAt time.Time) error {
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
