package auth

import (
	"context"
	"database/sql"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// store reads and writes the api_keys table. The writes run on the caller's
// transaction, so a key change commits with its audit row (Service); the
// reads open their own.
type store struct {
	db *postgres.DB
}

// storedHash is a key's salt and hash as stored, both hex (hashKey).
type storedHash struct {
	salt, hash string
}

// ListFilter selects a page of a tenant's keys.
type ListFilter struct {
	TenantID string
	Limit    int
	Offset   int
}

const keyCols = `id, key_prefix, key_type, livemode, name, tenant_id, created_at, expires_at, revoked_at, last_used_at`

// scanKey reads one row of keyCols and sets the key's status as of now.
func scanKey(row interface{ Scan(...any) error }) (domain.APIKey, error) {
	var k domain.APIKey
	err := row.Scan(&k.ID, &k.KeyPrefix, &k.KeyType, &k.Livemode, &k.Name, &k.TenantID,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt)
	k.Status = statusAt(k, time.Now().UTC())
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
	k, err := scanKey(tx.QueryRowContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE id = $1 FOR UPDATE`, id))
	if err == sql.ErrNoRows {
		return domain.APIKey{}, errs.ErrNotFound
	}
	return k, err
}

// revoke sets revoked_at on the key with id and returns the row. A key
// already revoked returns errs.ErrNotFound: the UPDATE matches no row.
//
// Revoking the tenant's last key is allowed. The operator signs in with
// email and password (ADR-011), so revoking every API key does not lock the
// tenant out: the operator can still sign in and create new keys.
func (s store) revoke(ctx context.Context, tx *sql.Tx, id string) (domain.APIKey, error) {
	k, err := scanKey(tx.QueryRowContext(ctx, `
		UPDATE api_keys SET revoked_at = $1
		WHERE id = $2 AND revoked_at IS NULL
		RETURNING `+keyCols, time.Now().UTC(), id))
	if err == sql.ErrNoRows {
		return domain.APIKey{}, errs.ErrNotFound
	}
	return k, err
}

// scheduleExpiry sets expires_at on the key with id, if it is not revoked, to
// expiresAt, or keeps the key's own expires_at if it is earlier, and returns
// the row or errs.ErrNotFound.
func (s store) scheduleExpiry(ctx context.Context, tx *sql.Tx, id string, expiresAt time.Time) (domain.APIKey, error) {
	k, err := scanKey(tx.QueryRowContext(ctx, `
		-- LEAST ignores a NULL, so a key with no expiry takes the grace end.
		UPDATE api_keys SET expires_at = LEAST(expires_at, $1)
		WHERE id = $2 AND revoked_at IS NULL
		RETURNING `+keyCols, expiresAt, id))
	if err == sql.ErrNoRows {
		return domain.APIKey{}, errs.ErrNotFound
	}
	return k, err
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

	var key domain.APIKey
	var h storedHash
	err = tx.QueryRowContext(ctx, `
		SELECT `+keyCols+`, key_hash, key_salt
		FROM api_keys
		WHERE key_prefix = $1
	`, prefix).Scan(&key.ID, &key.KeyPrefix, &key.KeyType, &key.Livemode, &key.Name, &key.TenantID,
		&key.CreatedAt, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt, &h.hash, &h.salt)
	if err == sql.ErrNoRows {
		return domain.APIKey{}, storedHash{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.APIKey{}, storedHash{}, err
	}
	key.Status = statusAt(key, time.Now().UTC())
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
	// The handler sends a limit of 1 to 100 (respond.ParsePage).
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
