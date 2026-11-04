package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// CheckSchemaReady refuses to let the server start on a database this
// binary cannot serve. It reads goose's record the way `hardcount migrate`
// does, read-only, on the caller's pool. It returns an error when a
// migration this binary ships is not applied, including migration 180 on
// an empty database. Without that migration, the app would fail on any
// query that touches a new column or table. It also returns
// ErrNoMigrationRecord for tables that goose has no record of, and
// ErrVeloxDatabase for a database that a build of Velox made.
//
// Call it at start-up, after Up when RUN_MIGRATIONS_ON_BOOT is on, and
// before serving. Refusing is safer than serving: the orchestrator
// retries, and by then the migration has run.
func CheckSchemaReady(db *sql.DB) error {
	ctx := context.Background()
	st, err := readState(ctx, db)
	if err != nil {
		return fmt.Errorf("read migration state: %w", err)
	}
	files, err := embeddedFiles(db, migrationFiles())
	if err != nil {
		return err
	}
	p := planFor(st, files)
	switch p.State {
	case PlanUnrecorded:
		return ErrNoMigrationRecord
	case PlanVelox:
		return ErrVeloxDatabase
	case PlanEmpty, PlanBehind:
		return fmt.Errorf("schema behind code: %s to apply; run `hardcount migrate`", countMigrations(len(p.Pending)))
	case PlanAhead:
		// This older binary runs on a newer schema, during a rolling deploy
		// overlap or after a rollback. It warns and continues, although old
		// code may not understand new columns or enum values, and no
		// expand/contract rule is enforced yet. (Under that rule a release
		// only adds, and a later release removes.) Whether to refuse
		// instead is still open. Before tightening this, see hazard 3 in
		// docs/dev/records/ha-readiness-2026-07-06.md.
		var ahead []int64
		for _, a := range p.Ahead {
			ahead = append(ahead, a.Version)
		}
		// This is not a normal path. To go back a release, the guide
		// restores the backup taken before the upgrade (#1245).
		slog.Warn("this release is older than the database: it may not read data the newer release wrote. To go back a release, restore the backup taken before the upgrade — see docs/upgrades.md#going-back",
			"database_version", p.Database,
			"binary_version", p.Latest,
			"versions_not_shipped", ahead,
		)
		return nil
	}
	slog.Info("schema ready", "version", p.Database, "binary_expects", p.Latest)
	return nil
}
