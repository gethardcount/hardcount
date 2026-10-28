package migrate

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3/lock"
)

// RunbookPath is the operator page for an upgrade that stopped. Every
// error from a failed migration names it.
const RunbookPath = "docs/ops/runbook/migration-failed-midway.md"

// LockTimeoutRunbookPath is the operator page for a migration that stopped
// on a table lock. LockTimeoutError names it.
const LockTimeoutRunbookPath = "docs/ops/runbook/migration-failed-midway.md#it-waited-5s-for-a-table-lock"

// pgLockNotAvailable is SQLSTATE 55P03, what Postgres raises when
// lock_timeout expires ("canceling statement due to lock timeout").
const pgLockNotAvailable = "55P03"

// ErrNoMigrationRecord says the database has tables but goose has not
// recorded migration 180. Up, Down and CheckSchemaReady return it before
// they change anything; `hardcount migrate status` shows it as
// PlanUnrecorded.
var ErrNoMigrationRecord = errors.New("this database has tables but no migration record " +
	"(goose_db_version has no row for 180), so Hardcount cannot tell which version they are at. Nothing was changed. " +
	"Restore the backup together with its migration table, or use an empty database. " +
	"Upgrading from Velox 0.8.x is not supported; install fresh. Steps: " + RunbookPath)

// VeloxRunbookPath is the operator page for a database Velox made.
const VeloxRunbookPath = RunbookPath + "#the-database-was-made-by-velox"

// ErrVeloxDatabase says a build of Velox, Hardcount's old name, made the
// database. Hardcount v0.9.0 installs fresh only. Up, Down and
// CheckSchemaReady return it before they change anything; `hardcount
// migrate status` shows it as PlanVelox.
var ErrVeloxDatabase = errors.New("this database was made by Velox, Hardcount's old name " +
	"(its tables give new rows vlx_ ids), and Hardcount does not take over a Velox database. Nothing was changed. " +
	"Install fresh: point DATABASE_URL at an empty database. Steps: " + VeloxRunbookPath)

// FileError says a migration failed. goose stops at the first failure and
// records nothing for it.
type FileError struct {
	Version int64
	// Name is the file name, or "00180 baseline" for migration 180.
	Name string
	// NoTx is true for an index-build file, which runs without a
	// transaction.
	NoTx bool
	Err  error
}

func (e *FileError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "migration %s failed: %s. ", e.Name, describeDBError(e.Err))
	switch {
	case e.NoTx:
		b.WriteString("It runs without a transaction, so Postgres may have left the index INVALID. " +
			"Running the upgrade again is safe: the file drops the index and builds it again. Fix the cause first.")
	case e.Version == baselineVersion && isDuplicateObject(e.Err):
		b.WriteString("This database already holds objects the baseline creates, and none of the baseline was applied. " +
			"Use an empty database.")
	default:
		b.WriteString("It runs in one transaction, so none of it was applied. Fix the cause and run the upgrade again.")
	}
	fmt.Fprintf(&b, " Steps: %s", RunbookPath)
	return b.String()
}

func (e *FileError) Unwrap() error { return e.Err }

// LockTimeoutError says a migration could not get a table lock within
// MIGRATION_LOCK_TIMEOUT (#987). The migration ran in one transaction, so
// Postgres undid all of it and goose recorded nothing.
type LockTimeoutError struct {
	Version int64
	Name    string
	// Previous is the version the database is at: the last migration that
	// finished, 0 when none has.
	Previous int64
	Timeout  time.Duration
	Err      error
}

func (e *LockTimeoutError) Error() string {
	at := fmt.Sprintf("is still at version %d", e.Previous)
	if e.Previous == 0 {
		at = "has no migration applied"
	}
	return fmt.Sprintf("migration %s waited %s for a table lock and stopped: another database session, "+
		"usually a long-running transaction, holds a lock on a table it changes. "+
		"Postgres undid the whole migration and the database %s, so nothing needs repair: "+
		"run the upgrade again when the table is quieter, or end the blocking session first. "+
		"%s sets how long to wait. Steps: %s",
		e.Name, e.Timeout, at, LockTimeoutEnv, LockTimeoutRunbookPath)
}

func (e *LockTimeoutError) Unwrap() error { return e.Err }

// LockWaitError says another process held the migration lock for the
// whole wait. Nothing was changed.
type LockWaitError struct {
	Waited time.Duration
	Err    error
}

func (e *LockWaitError) Error() string {
	return fmt.Sprintf("another Hardcount process is migrating this database (it holds advisory lock %d), "+
		"and this one waited %s for it: `hardcount migrate status` shows it under \"running now\". "+
		"Run the upgrade again when it has finished. Nothing was changed.",
		lock.DefaultLockID, e.Waited)
}

func (e *LockWaitError) Unwrap() error { return e.Err }

// describeDBError renders a Postgres error with the fields an operator
// needs to find the cause: the detail names the duplicate key on 23505.
func describeDBError(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err.Error()
	}
	parts := []string{pgErr.Error()}
	if pgErr.Detail != "" {
		parts = append(parts, "detail: "+pgErr.Detail)
	}
	if pgErr.Hint != "" {
		parts = append(parts, "hint: "+pgErr.Hint)
	}
	if pgErr.ConstraintName != "" {
		parts = append(parts, "constraint: "+pgErr.ConstraintName)
	}
	if pgErr.Position > 0 {
		parts = append(parts, fmt.Sprintf("at character %d", pgErr.Position))
	}
	return strings.Join(parts, "; ")
}

// isLockTimeout reports whether err is Postgres giving up on a lock.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgLockNotAvailable
}

// isDuplicateObject reports a CREATE of something that already exists:
// a table or index (42P07), a function (42723) or a type (42710).
func isDuplicateObject(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P07", "42723", "42710":
		return true
	}
	return false
}
