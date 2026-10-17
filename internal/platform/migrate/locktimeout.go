package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5/pgconn"
)

// LockTimeoutEnv names the setting that bounds how long a migration waits
// for a table lock (issue #987). A migration that changes a table needs a
// lock on it, and while it waits for that lock every other query on the
// table queues behind it. Without a limit, one long transaction in the
// app turns an upgrade into an outage of that table.
const LockTimeoutEnv = "MIGRATION_LOCK_TIMEOUT"

// DefaultLockTimeout is how long a migration waits for a lock when
// LockTimeoutEnv is unset. Long enough to get past ordinary short
// transactions, short enough that queued app queries recover quickly.
const DefaultLockTimeout = 5 * time.Second

// LockTimeoutRunbookPath is the operator page for a migration that
// stopped on a lock. LockTimeoutError names it.
const LockTimeoutRunbookPath = "docs/ops/runbook/migration-lock-timeout.md"

// pgLockNotAvailable is SQLSTATE 55P03, what Postgres raises when
// lock_timeout expires ("canceling statement due to lock timeout").
const pgLockNotAvailable = "55P03"

// lockTimeout reads LockTimeoutEnv. "0" means no limit (Postgres'
// default), for an operator who has chosen to wait.
func lockTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(LockTimeoutEnv))
	if raw == "" {
		return DefaultLockTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if raw == "0" {
		d, err = 0, nil
	}
	if err != nil || d < 0 || (d > 0 && d < time.Millisecond) {
		return 0, fmt.Errorf("%s=%q: want a duration such as 5s, 30s or 2m, or 0 for no limit", LockTimeoutEnv, raw)
	}
	return d, nil
}

// isLockTimeout reports whether err is Postgres giving up on a lock.
// golang-migrate wraps the driver error in database.Error, which has no
// Unwrap, so its OrigErr is followed by hand.
func isLockTimeout(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgLockNotAvailable
	}
	var dbErr database.Error
	if errors.As(err, &dbErr) {
		return isLockTimeout(dbErr.OrigErr)
	}
	var dbErrPtr *database.Error
	if errors.As(err, &dbErrPtr) && dbErrPtr != nil {
		return isLockTimeout(dbErrPtr.OrigErr)
	}
	return false
}

// LockTimeoutError says an upgrade stopped because a migration could not
// get a table lock within the lock timeout.
type LockTimeoutError struct {
	// Version is the migration that was waiting.
	Version uint
	// Previous is the version the database was at before it.
	Previous uint
	// Timeout is the limit that was reached.
	Timeout time.Duration
	// Restored is true when nothing of the migration is in the database
	// and schema_migrations is clean at Previous, so the upgrade can just
	// run again. False means the dirty mark is still there (a migration
	// that runs outside a transaction may be partly applied).
	Restored bool
	Err      error
}

func (e *LockTimeoutError) Error() string {
	file := migrationFileName(e.Version)
	if file == "" {
		file = fmt.Sprintf("%04d", e.Version)
	}
	head := fmt.Sprintf("migration %d (%s) waited %s for a table lock and stopped: another database session, "+
		"usually a long-running transaction, holds a lock on a table it changes", e.Version, file, e.Timeout)
	if !e.Restored {
		return head + ". " + (&DirtyError{Version: e.Version}).Error()
	}
	return fmt.Sprintf("%s. Postgres undid the whole migration and the database is still at version %d, "+
		"so nothing needs repair: run the upgrade again when the table is quieter, or end the blocking session first. "+
		"%s sets how long to wait. Steps: %s",
		head, e.Previous, LockTimeoutEnv, LockTimeoutRunbookPath)
}

func (e *LockTimeoutError) Unwrap() error { return e.Err }

// newLockTimeoutError builds the error for migration `attempted`, which
// stopped on a lock while the database was at `previous`.
//
// When the migration ran in one transaction (inTx), Postgres rolled all of
// it back, and the only trace is the dirty mark golang-migrate wrote before
// running it. Clearing that mark here is what makes a lock timeout a
// "run it again" event instead of a manual repair. It is safe because every
// in-transaction migration is all-or-nothing (TestInTxMigrationsAreAllOrNothing
// keeps it so) and the caller holds the hybrid loop lock, so no other
// runner or Force is touching schema_migrations.
//
// For a migration that runs outside a transaction the mark stays, unless
// the lock wait came before anything ran (the row is still clean at
// `previous`).
func newLockTimeoutError(db *sql.DB, attempted, previous uint, inTx bool, cause error) error {
	timeout, _ := lockTimeout() // already validated when the pool opened
	e := &LockTimeoutError{Version: attempted, Previous: previous, Timeout: timeout, Err: cause}
	restored, err := settleAfterLockTimeout(context.Background(), db, attempted, previous, inTx)
	if err != nil {
		return fmt.Errorf("%w (and reading schema_migrations afterwards failed: %v)", e, err)
	}
	e.Restored = restored
	return e
}

// settleAfterLockTimeout reports whether schema_migrations is clean at
// `previous`, first clearing the dirty mark for `attempted` when inTx.
func settleAfterLockTimeout(ctx context.Context, db *sql.DB, attempted, previous uint, inTx bool) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var version int64
	var dirty bool
	err = tx.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1 FOR UPDATE`).Scan(&version, &dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return previous == 0, nil
	case err != nil:
		return false, err
	}
	if !dirty {
		return version >= 0 && uint(version) == previous, nil
	}
	if !inTx || version < 0 || uint(version) != attempted {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `TRUNCATE schema_migrations`); err != nil {
		return false, err
	}
	if previous > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, dirty) VALUES ($1, false)`, previous); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
