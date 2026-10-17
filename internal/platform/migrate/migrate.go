// Package migrate applies Velox's database migrations with goose
// (ADR-125).
//
// Migration 180 is a Go migration: on an empty database it runs the
// v0.8.1 baseline, and on a v0.8.0 or v0.8.1 database it takes the
// golang-migrate record over without changing a table (baseline.go). Files
// after it live in sql/ as NNNNN_name.sql. A read-only state check runs
// before every command (state.go).
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// sql/ holds the migrations after 180. README.md keeps the embed valid
// while there are none; goose collects only *.sql and *.go files.
//
//go:embed sql
var sqlFS embed.FS

// versionTable is goose's record. Schema-qualified, because goose writes
// its row in the migration's own transaction, and the baseline sets an
// empty search_path for that transaction.
const versionTable = "public.goose_db_version"

// How long a command waits for another process's migration lock: goose
// polls pg_try_advisory_lock every lockWaitPeriod seconds, lockWaitPolls
// times. 30 minutes, the same total wait as v0.8.1. Polling, not a
// blocking pg_advisory_lock: a blocked lock statement holds a snapshot,
// and an index build CONCURRENTLY in the other process waits for every
// older snapshot, so the two would wait on each other (ADR-125, E3).
// Tests shorten it.
var (
	lockWaitPeriod uint64 = 5
	lockWaitPolls  uint64 = 360
)

// gooseLockBusy is the error goose's session locker returns when every
// poll found the lock taken (lock/postgres.go in goose v3.27.0).
const gooseLockBusy = "failed to acquire lock"

// migrationFiles returns the embedded files after the baseline.
func migrationFiles() fs.FS {
	sub, err := fs.Sub(sqlFS, "sql")
	if err != nil {
		panic(err) // the embed directive guarantees sql/ exists
	}
	return sub
}

// openPool opens a pool of one connection for one command. The caller
// closes it before returning.
//
// One connection is enough: goose runs every file on the connection that
// holds its lock, and a Go migration in a transaction (migration 180) uses
// that same connection. A Go migration without a transaction would need a
// second one; goose refuses those when the pool has one connection, so such
// a file fails loudly instead of waiting forever.
//
// Every session carries lock_timeout from MIGRATION_LOCK_TIMEOUT (#987), so
// a migration that cannot get a table lock stops instead of queueing every
// app query on that table behind it. statement_timeout is left alone: a
// migration that holds its locks should finish.
//
// Closing the pool matters: an index-build file that fails leaves
// lock_timeout = 0 on its session (its RESET never ran), and the next
// command must not reuse that session.
func openPool(dsn string) (*sql.DB, error) {
	timeout, err := lockTimeout()
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse migration db url: %w", err)
	}
	cfg.RuntimeParams["lock_timeout"] = strconv.FormatInt(timeout.Milliseconds(), 10)
	cfg.RuntimeParams["application_name"] = "velox-migrate"
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping migration db: %w", err)
	}
	return db, nil
}

// newProvider builds the goose provider for one command. locked adds the
// session locker; ReadPlan and CheckSchemaReady only list the sources and
// never call the provider's database methods.
func newProvider(db *sql.DB, fsys fs.FS, locked bool) (*goose.Provider, error) {
	opts := []goose.ProviderOption{
		goose.WithTableName(versionTable),
		goose.WithDisableGlobalRegistry(true),
		goose.WithGoMigrations(newMigration180()),
	}
	if locked {
		inner, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(lockWaitPeriod, lockWaitPolls))
		if err != nil {
			return nil, err
		}
		opts = append(opts, goose.WithSessionLocker(lockWait{inner}))
	}
	return goose.NewProvider(goose.DialectPostgres, db, fsys, opts...)
}

// lockWait turns goose's "failed to acquire lock" into a LockWaitError.
// goose wraps the locker's error with %w, so errors.As finds it.
type lockWait struct{ lock.SessionLocker }

func (l lockWait) SessionLock(ctx context.Context, conn *sql.Conn) error {
	err := l.SessionLocker.SessionLock(ctx, conn)
	if err != nil && err.Error() == gooseLockBusy {
		waited := time.Duration(lockWaitPeriod*lockWaitPolls) * time.Second
		return &LockWaitError{Waited: waited, Err: err}
	}
	return err
}

// Up applies every pending migration with a pool of its own, closed on
// return. The caller's app pool is untouched; only the DSN is needed.
//
// Several processes may call Up at once (RUN_MIGRATIONS_ON_BOOT on several
// copies): they take turns on goose's lock, and each applies only what is
// still missing.
func Up(dsn string) error {
	return up(context.Background(), dsn, migrationFiles())
}

// up is Up with the files as a parameter, for tests.
func up(ctx context.Context, dsn string, fsys fs.FS) error {
	db, err := openPool(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	st, err := readState(ctx, db)
	if err != nil {
		return fmt.Errorf("read migration state: %w", err)
	}
	if err := decide(st).refusal(); err != nil {
		return err
	}
	p, err := newProvider(db, fsys, true)
	if err != nil {
		return err
	}

	start := time.Now()
	results, err := p.Up(ctx)
	var partial *goose.PartialError
	if errors.As(err, &partial) {
		results = partial.Applied
	}
	for _, r := range results {
		slog.Info("migration applied",
			"migration", sourceName(r.Source),
			"version", r.Source.Version,
			"duration_ms", r.Duration.Milliseconds(),
		)
	}
	if err != nil {
		return upError(err, fsys, st, results)
	}
	if len(results) == 0 {
		slog.Info("migrations up to date", "version", st.gooseVersion())
		return nil
	}
	slog.Info("migrations applied",
		"version", results[len(results)-1].Source.Version,
		"applied", len(results),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// upError maps goose's error to Velox's typed errors.
func upError(err error, fsys fs.FS, st state, applied []*goose.MigrationResult) error {
	var waited *LockWaitError
	if errors.As(err, &waited) {
		return waited
	}
	var partial *goose.PartialError
	if !errors.As(err, &partial) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	var refused *V08Error
	if errors.As(partial.Err, &refused) {
		return refused
	}
	src := partial.Failed.Source
	noTx := src.Type == goose.TypeSQL && isNoTxFile(fsys, src.Path)
	if !noTx && isLockTimeout(partial.Err) {
		prev := st.gooseVersion()
		if len(applied) > 0 {
			prev = applied[len(applied)-1].Source.Version
		}
		timeout, _ := lockTimeout() // already checked when the pool opened
		return &LockTimeoutError{Version: src.Version, Name: sourceName(src), Previous: prev, Timeout: timeout, Err: partial.Err}
	}
	return &FileError{Version: src.Version, Name: sourceName(src), NoTx: noTx, Err: partial.Err}
}

// sourceName is a migration's file name, or baselineName for 180.
func sourceName(s *goose.Source) string {
	if s.Version == baselineVersion && s.Type == goose.TypeGo {
		return baselineName
	}
	if s.Path == "" {
		return fmt.Sprintf("%05d", s.Version)
	}
	return s.Path
}

// isNoTxFile reports whether a migration file carries goose's
// `-- +goose NO TRANSACTION` annotation, the mark of an index build.
func isNoTxFile(fsys fs.FS, path string) bool {
	body, err := fs.ReadFile(fsys, path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, " \t\r"), "-- +goose ")
		if ok && strings.EqualFold(strings.TrimSpace(rest), "NO TRANSACTION") {
			return true
		}
	}
	return false
}
