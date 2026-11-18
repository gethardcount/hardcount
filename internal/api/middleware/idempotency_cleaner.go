package middleware

import (
	"context"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// IdempotencyCleaner deletes expired keys, in the shape of the scheduler's
// cleaners (billing.IdempotencyCleaner), so the scheduler does not import
// this package.
type IdempotencyCleaner struct {
	db *postgres.DB
}

// NewIdempotencyCleaner returns an IdempotencyCleaner for the keys in db.
func NewIdempotencyCleaner(db *postgres.DB) *IdempotencyCleaner {
	return &IdempotencyCleaner{db: db}
}

// Cleanup deletes the expired keys of every tenant and returns how many it
// deleted. The scheduler calls it outside any request, so it bypasses the
// tenant_isolation policy (TxBypass).
func (c *IdempotencyCleaner) Cleanup(ctx context.Context) (int, error) {
	tx, err := c.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
