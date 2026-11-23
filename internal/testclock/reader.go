// Package testclock reads a test clock's row for the billing engine's
// resolver, which dates work on a clock's customer at the clock's time.
// Nothing creates or moves a clock.
package testclock

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type Reader struct {
	db *postgres.DB
}

func NewReader(db *postgres.DB) *Reader {
	return &Reader{db: db}
}

// Get reads the fields of clock id that the resolver dates work from.
func (r *Reader) Get(ctx context.Context, tenantID, id string) (domain.TestClock, error) {
	tx, err := r.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.TestClock{}, err
	}
	defer postgres.Rollback(tx)

	var clk domain.TestClock
	err = tx.QueryRowContext(ctx, `SELECT id, tenant_id, name, frozen_time, status, advance_target FROM test_clocks WHERE id = $1`, id).
		Scan(&clk.ID, &clk.TenantID, &clk.Name, &clk.FrozenTime, &clk.Status, &clk.AdvanceTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TestClock{}, errs.ErrNotFound
	}
	return clk, err
}
