package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// RunbookPath is the operator page for a dirty schema. Every dirty error
// names it, so the boot log points straight at the repair steps.
const RunbookPath = "docs/ops/runbook/migration-failed-midway.md"

// ErrNotDirty is returned by Force when schema_migrations is clean. Force
// exists only to clear the mark a failed migration leaves; on a clean
// database it could only move the recorded version away from the schema.
var ErrNotDirty = errors.New("schema_migrations is not dirty: there is nothing to repair")

// ErrMigrationRunning is returned by Force when a migration lock is held.
// While a migration runs, its dirty mark is expected; clearing it then
// would let a second run start on a half-changed schema.
var ErrMigrationRunning = errors.New("a migration is running right now (its lock is held): wait for it to finish, then run `velox migrate status` again")

// DirtyError says schema_migrations is marked dirty: a migration started
// and did not finish. Server start, `velox migrate` and `velox migrate
// rollback` all refuse with it until an operator checks the schema and runs
// `velox migrate force`. The message names the migration and the commands.
type DirtyError struct {
	Version uint
}

func (e *DirtyError) Error() string {
	v := e.Version
	file := migrationFileName(v)
	if file == "" {
		return fmt.Sprintf("schema_migrations is dirty at version %d: a migration started and did not finish. "+
			"This binary does not ship migration %d, so run the repair with the Velox release that does. Steps: %s",
			v, v, RunbookPath)
	}
	prev, _ := postApplyVersion(v, "down")
	var b strings.Builder
	fmt.Fprintf(&b, "schema_migrations is dirty at version %d (%s): a migration started and did not finish, "+
		"so Velox will not start or migrate until it is repaired. ", v, file)
	if noTx, _ := versionsWithNoTxHeader("up"); hasVersion(noTx, v) {
		b.WriteString("This migration runs outside a transaction, so some of its changes may be in the database. ")
	} else {
		b.WriteString("This migration runs in one transaction, so usually none of its changes are in the database. ")
	}
	fmt.Fprintf(&b, "Check the schema, then: if none of its changes are there, run `velox migrate force %d` and then `velox migrate`; "+
		"if all of them are there, run `velox migrate force %d`; if only some are, undo them by hand first. Steps: %s",
		prev, v, RunbookPath)
	return b.String()
}

// asDirtyError turns golang-migrate's ErrDirty into a DirtyError, so the
// library path reports the same message and repair steps as ours.
func asDirtyError(err error) error {
	var de migrate.ErrDirty
	if errors.As(err, &de) && de.Version >= 0 {
		return &DirtyError{Version: uint(de.Version)}
	}
	return err
}

func hasVersion(set map[uint]struct{}, v uint) bool {
	_, ok := set[v]
	return ok
}

// migrationFileName returns the embedded .up.sql name for version, or ""
// when this binary does not ship it.
func migrationFileName(version uint) string {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		return ""
	}
	prefix := fmt.Sprintf("%04d_", version)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".up.sql") {
			return name
		}
	}
	return ""
}

// ForceTargets returns the versions Force accepts for a database dirty at
// `dirty`: the version before it, the version itself, and the one after.
// One failed step leaves the schema between two neighbouring versions —
// an up of N fails between N-1 and N, a library down of N records its
// target N-1 and fails between N-1 and N — so no other target can match
// the schema. The previous version is 0 when `dirty` is the first
// migration, which Force records as "no migrations applied".
func ForceTargets(dirty uint) ([]uint, error) {
	versions, err := embeddedUpVersions()
	if err != nil {
		return nil, err
	}
	for i, v := range versions {
		if v != dirty {
			continue
		}
		var out []uint
		if i > 0 {
			out = append(out, versions[i-1])
		} else {
			out = append(out, 0)
		}
		out = append(out, v)
		if i+1 < len(versions) {
			out = append(out, versions[i+1])
		}
		return out, nil
	}
	return nil, fmt.Errorf("this binary does not ship migration %d: run the repair with the Velox release that does", dirty)
}

// Force clears a dirty migration mark by recording `version` as applied
// and clean — golang-migrate's `force`, with three refusals for the cases
// where forcing would make things worse:
//
//   - a migration is running (its lock is held): ErrMigrationRunning;
//   - the database is not dirty: ErrNotDirty;
//   - `version` is not next to the dirty version (see ForceTargets).
//
// Force never changes the schema itself. The operator checks which of the
// failed migration's changes are present first (RunbookPath); `velox
// migrate` then applies whatever comes after `version`.
func Force(dsn string, version uint) error {
	db, err := openMigrationPool(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Take both locks a migration run holds — the hybrid loop lock and
	// golang-migrate's own — without waiting. Holding them for the write
	// also keeps a migration from starting halfway through the repair.
	dbName, schemaName, err := lookupDBAndSchema(ctx, conn)
	if err != nil {
		return fmt.Errorf("lookup db/schema: %w", err)
	}
	var held []int64
	defer func() {
		for _, key := range held {
			_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key)
		}
	}()
	for _, key := range []int64{postgres.LockKeyMigrateHybrid, generateAdvisoryLockID(dbName, schemaName, "schema_migrations")} {
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
			return fmt.Errorf("try migration lock: %w", err)
		}
		if !got {
			return ErrMigrationRunning
		}
		held = append(held, key)
	}

	cur, dirty, err := readVersion(ctx, conn)
	if err != nil {
		return err
	}
	if !dirty {
		return fmt.Errorf("%w (version %d)", ErrNotDirty, cur)
	}
	targets, err := ForceTargets(cur)
	if err != nil {
		return err
	}
	allowed := false
	for _, t := range targets {
		if t == version {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("refusing to force version %d: the database is dirty at %d, so the schema can only match one of %v", version, cur, targets)
	}

	if version == 0 {
		// golang-migrate's "no version": an empty table. A recorded 0
		// would make the next Up look for a migration numbered 0.
		_, err := conn.ExecContext(ctx, `TRUNCATE schema_migrations`)
		return err
	}
	return setSchemaVersion(ctx, conn, version, false)
}
