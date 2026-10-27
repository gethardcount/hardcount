package migrate

import (
	"context"
	"database/sql"
	_ "embed" // the baseline file
	"errors"

	"github.com/pressly/goose/v3"
)

// baselineVersion is migration 180, the starting schema. Files in sql/
// start at 00181.
const baselineVersion int64 = 180

// baselineName labels migration 180 in logs and errors. It has no file in
// sql/: it is a Go migration.
const baselineName = "00180 baseline"

// baselineSQL is the starting schema. It lives outside sql/, so goose
// never collects it as a migration of its own.
//
//go:embed baseline/00180_baseline.sql
var baselineSQL string

// errBaselineDown is migration 180's Down: the database never goes below 180.
var errBaselineDown = errors.New("the baseline (migration 180) cannot be rolled back: restore the backup taken before the upgrade")

// newMigration180 builds migration 180 for one provider. goose runs it
// inside its session lock and its transaction, and writes row 180 in that
// same transaction, so a failure leaves nothing behind and two copies
// cannot both run it.
func newMigration180() *goose.Migration {
	return goose.NewGoMigration(baselineVersion,
		&goose.GoFunc{RunTx: runBaseline},
		&goose.GoFunc{RunTx: func(context.Context, *sql.Tx) error { return errBaselineDown }},
	)
}

// runBaseline runs the baseline: one Exec with no arguments, which pgx
// sends as one simple query inside goose's transaction.
func runBaseline(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, baselineSQL)
	return err
}
