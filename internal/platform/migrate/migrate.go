package migrate

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	mpg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

//go:embed sql/*.sql
var sqlFS embed.FS

// migrationFilenamePattern matches golang-migrate's file convention:
// {version}_{name}.{up|down}.sql — e.g., "0003_tax_cleanup.up.sql".
var migrationFilenamePattern = regexp.MustCompile(`^(\d+)_.*\.up\.sql$`)
var migrationDownFilenamePattern = regexp.MustCompile(`^(\d+)_.*\.down\.sql$`)

// noTxHeader is the opt-out marker. A migration whose first 5 lines contain
// this exact line is applied OUTSIDE any transaction wrapping (autocommit),
// which is required for statements such as `CREATE INDEX CONCURRENTLY` and
// `ALTER TYPE ... ADD VALUE` that PostgreSQL refuses to run inside a
// transaction block.
//
// Why a custom header rather than golang-migrate's `x-no-tx-wrap=true`:
// the postgres driver in golang-migrate v4 does NOT support that flag — only
// the sqlite/sqlite3/sqlcipher drivers do. The postgres driver runs the SQL
// via `conn.ExecContext(ctx, fileBytes)`; PostgreSQL treats a multi-statement
// Exec as an implicit transaction block, which is what trips CONCURRENTLY.
// The fix is to pull such files out of the migrate-library path and apply
// them via our own `db.ExecContext` after splitting on `;` so each statement
// runs in autocommit. See the runner code below.
//
// Header rules:
//   - Exact match `-- velox:no-transaction` (case-sensitive).
//   - Must appear on its own line within the first 5 non-empty-or-comment
//     lines of the file. We deliberately scan a small window so a stray
//     occurrence inside a multi-line comment lower in the file does not
//     accidentally flip the migration's transaction mode.
//   - The matching `.down.sql` must carry the same header if its rollback
//     statement also requires no-tx (e.g., `DROP INDEX CONCURRENTLY`).
const noTxHeader = "-- velox:no-transaction"

// noTxHeaderScanLines bounds how far into a file we look for the header.
// 5 is enough for a leading copyright/comment block plus the marker.
const noTxHeaderScanLines = 5

// hybridLockPollInterval/hybridLockWaitMax bound the try-lock poll in
// acquireHybridLoopLock. 30 minutes is far beyond any migration this
// repo ships (the slowest CONCURRENTLY index builds in seconds at
// current scale) while still failing loudly if a replica wedges.
const (
	hybridLockPollInterval = 500 * time.Millisecond
	hybridLockWaitMax      = 30 * time.Minute
)

// openMigrationPool opens a dedicated short-lived *sql.DB for one migration
// command. Migrations are single-threaded, so a 1-connection pool is enough.
//
// A dedicated pool is required because golang-migrate's postgres driver
// closes the underlying *sql.DB inside its Close() — passing the app's
// shared pool would leave it unusable for every subsequent operation (e.g.,
// CheckSchemaReady, serving requests). This helper keeps that close
// side-effect scoped to a pool the caller never sees.
func openMigrationPool(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open migration db: %w", err)
	}
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

// newMigrator creates a golang-migrate instance from embedded SQL files.
// Internal helper — callers outside this package should use Up, Status,
// or Rollback so they don't have to reason about the driver's Close()
// side-effect on the supplied *sql.DB.
func newMigrator(db *sql.DB) (*migrate.Migrate, error) {
	subFS, err := fs.Sub(sqlFS, "sql")
	if err != nil {
		return nil, fmt.Errorf("sub fs: %w", err)
	}

	source, err := iofs.New(subFS, ".")
	if err != nil {
		return nil, fmt.Errorf("iofs source: %w", err)
	}

	driver, err := mpg.WithInstance(db, &mpg.Config{
		MigrationsTable: "schema_migrations",
	})
	if err != nil {
		return nil, fmt.Errorf("postgres driver: %w", err)
	}

	return migrate.NewWithInstance("iofs", source, "postgres", driver)
}

// Up applies all pending migrations using a dedicated short-lived connection
// pool. The caller's app pool (if any) is untouched — only the DSN is needed.
//
// Concurrency: the pure-library fast path relies on golang-migrate's
// internal pg_advisory_lock (keyed on db+schema) — racing replicas
// serialize there, the losers find ErrNoChange and proceed. The hybrid
// path (any embedded no-tx file — always, since 0062) additionally
// serializes its WHOLE loop under LockKeyMigrateHybrid (ADR-073): the
// library's per-step lock cannot protect the loop's decide-then-dispatch
// reads, and unlocked racers could rewind versions or push a
// CONCURRENTLY file through the in-tx path. See acquireHybridLoopLock.
//
// Production guidance: run migrations as a dedicated deploy step (e.g., a
// Kubernetes Job with activeDeadlineSeconds, or a CI step before rollout),
// not on app boot. Boot-time migrations complicate rolling deploys and
// startup probes, and a wedged DDL cannot be cancelled from Go (Postgres
// advisory locks ignore client-side context cancellation). App replicas
// should instead call CheckSchemaReady at startup to refuse to serve with
// an outdated schema.
//
// No-transaction migrations: any `.up.sql` whose first lines contain
// `-- velox:no-transaction` is applied OUTSIDE the migrate library's
// path — see noTxHeader for rationale. We process pending migrations
// sequentially, dispatching each to either golang-migrate's `Steps(1)`
// (in-tx, the default) or our own apply (autocommit, when the header is
// present). Ordering and the advisory-lock semantics are preserved end-to-end.
func Up(dsn string) error {
	db, err := openMigrationPool(dsn)
	if err != nil {
		return err
	}

	noTxVersions, err := versionsWithNoTxHeader("up")
	if err != nil {
		_ = db.Close()
		return err
	}

	// Fast path — if no migration in the embedded set opts out of the
	// transaction wrap, behaviour is identical to before this change:
	// `m.Up()` runs the whole batch in one shot through golang-migrate.
	// The hybrid runner is only engaged when at least one no-tx file
	// exists, keeping the change inert for the existing 60+ migrations.
	if len(noTxVersions) == 0 {
		m, err := newMigrator(db)
		if err != nil {
			_ = db.Close()
			return err
		}
		defer closeMigrator(m) // closes db via the postgres driver

		start := time.Now()
		err = m.Up()
		if errors.Is(err, migrate.ErrNoChange) {
			slog.Info("migrations up to date")
			return nil
		}
		if err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}

		v, _, _ := m.Version()
		slog.Info("migrations applied",
			"version", v,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return nil
	}

	// Hybrid path — at least one pending migration needs autocommit.
	// Close our preliminary read-only check pool — the hybrid runner
	// opens its own pools per-step (one for the library, one for the
	// no-tx applier).
	_ = db.Close()
	return upHybrid(dsn, noTxVersions)
}

// acquireHybridLoopLock serializes the ENTIRE hybrid loop across
// replicas (ADR-073). The loop's decide-then-dispatch reads of
// schema_migrations were unlocked: two replicas booting together could
// double-apply a no-tx migration, rewind the recorded version, or —
// worst — mis-dispatch a `CONCURRENTLY` file through the library's
// in-tx path (replica B's Steps(1) applies "whatever is next" under the
// library's lock, not the version B's loop intended), leaving a dirty
// crash-loop. Holding one Velox-owned lock for the loop's whole
// duration makes every per-iteration read authoritative: the loser
// replica waits, then finds each version already applied and skips.
//
// The key is deliberately NOT golang-migrate's derived lock id — the
// library takes that id on its own session inside each step, and
// holding the same id here would deadlock it. A dedicated 1-conn pool
// carries the lock so holding it can't starve statusDB's bookkeeping
// reads (both pools are capped at one connection).
func acquireHybridLoopLock(dsn string) (release func(), err error) {
	lockDB, err := openMigrationPool(dsn)
	if err != nil {
		return nil, fmt.Errorf("open hybrid lock pool: %w", err)
	}
	ctx := context.Background()
	conn, err := lockDB.Conn(ctx)
	if err != nil {
		_ = lockDB.Close()
		return nil, fmt.Errorf("acquire hybrid lock conn: %w", err)
	}
	// POLL with try-lock instead of a blocking pg_advisory_lock: a
	// blocked lock statement holds a snapshot for as long as it waits,
	// and the winner's CREATE INDEX CONCURRENTLY must wait out every
	// older snapshot — a mutual wait Postgres cannot detect (advisory
	// waits aren't in its deadlock graph). Short try-lock statements
	// hold nothing between polls, so the winner's DDL proceeds and the
	// loser gets the lock when it finishes.
	deadline := time.Now().Add(hybridLockWaitMax)
	for attempt := 0; ; attempt++ {
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, postgres.LockKeyMigrateHybrid).Scan(&got); err != nil {
			_ = conn.Close()
			_ = lockDB.Close()
			return nil, fmt.Errorf("acquire hybrid migration loop lock: %w", err)
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			_ = conn.Close()
			_ = lockDB.Close()
			return nil, fmt.Errorf("hybrid migration loop lock still held after %s — another replica's migration run appears wedged", hybridLockWaitMax)
		}
		if attempt%60 == 59 { // ~every 30s at 500ms polls
			slog.Info("waiting for hybrid migration loop lock (another replica is migrating)")
		}
		time.Sleep(hybridLockPollInterval)
	}
	return func() {
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, postgres.LockKeyMigrateHybrid); err != nil {
			slog.Warn("release hybrid migration loop lock", "error", err)
		}
		_ = conn.Close()
		_ = lockDB.Close()
	}, nil
}

// upHybrid applies migrations one at a time, dispatching each to the
// appropriate runner: golang-migrate's `Steps(1)` for normal in-tx files,
// and our own `applyNoTx` for files marked with `-- velox:no-transaction`.
//
// We hold a single dedicated *sql.DB only for the no-tx path (advisory
// lock + raw exec). The library path opens its own throwaway pool per
// step (see stepOneViaLibrary) so its Close() doesn't fight ours.
// The whole loop runs under the hybrid loop lock (ADR-073) so racing
// replicas serialize here, not per-step.
func upHybrid(dsn string, noTxVersions map[uint]struct{}) error {
	release, err := acquireHybridLoopLock(dsn)
	if err != nil {
		return err
	}
	defer release()

	statusDB, err := openMigrationPool(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = statusDB.Close() }()

	versions, err := embeddedUpVersions()
	if err != nil {
		return err
	}

	overallStart := time.Now()
	applied := 0
	for _, v := range versions {
		curVer, dirty, err := DatabaseVersion(statusDB)
		if err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		if dirty {
			return fmt.Errorf("schema_migrations is dirty at version %d — refuse to step forward", curVer)
		}
		if v <= curVer {
			continue
		}
		if _, isNoTx := noTxVersions[v]; isNoTx {
			start := time.Now()
			if err := applyNoTxOnPool(dsn, v, "up"); err != nil {
				return fmt.Errorf("apply no-tx migration %04d up: %w", v, err)
			}
			slog.Info("no-tx migration applied",
				"version", v,
				"direction", "up",
				"duration_ms", time.Since(start).Milliseconds(),
			)
			applied++
			continue
		}
		// In-tx step via the library — re-using all of golang-migrate's
		// machinery (advisory lock, dirty bookkeeping, error formatting).
		if err := stepOneViaLibrary(dsn, +1); err != nil {
			return fmt.Errorf("apply migration %04d up via library: %w", v, err)
		}
		applied++
	}

	if applied == 0 {
		slog.Info("migrations up to date")
		return nil
	}
	v, _, _ := DatabaseVersion(statusDB)
	slog.Info("migrations applied",
		"version", v,
		"applied", applied,
		"duration_ms", time.Since(overallStart).Milliseconds(),
	)
	return nil
}

// applyNoTxOnPool opens a dedicated short-lived pool for one no-tx
// migration. We can't share the statusDB pool because applyNoTx holds the
// connection while running long DDL — concurrent reads of schema_migrations
// from the outer loop would otherwise queue behind it on the 1-conn pool.
func applyNoTxOnPool(dsn string, version uint, direction string) error {
	pool, err := openMigrationPool(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()
	return applyNoTx(pool, version, direction)
}

// stepOneViaLibrary runs a single migrate.Steps(±1) by opening a fresh,
// throwaway *sql.DB pool just for this step. The library's Close() shuts
// the pool down — we do not want that to affect the persistent pool
// upHybrid/rollbackHybrid uses for its own bookkeeping and no-tx applies.
//
// dsn is the raw connection string we re-open with every step. The cost
// is a TCP+auth handshake per migration (~1-5ms on localhost) — trivial
// next to the migration body itself, and it preserves clean ownership:
// each library call gets its own pool, owns it fully, and closes it at
// the end with no entanglement with our outer code.
func stepOneViaLibrary(dsn string, direction int) error {
	stepDB, err := openMigrationPool(dsn)
	if err != nil {
		return err
	}
	m, err := newMigrator(stepDB)
	if err != nil {
		_ = stepDB.Close()
		return err
	}
	defer closeMigrator(m) // closes stepDB via the postgres driver

	if err := m.Steps(direction); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			return nil
		}
		return err
	}
	return nil
}

// applyNoTx executes a no-transaction migration directly via the *sql.DB.
//
// This bypasses golang-migrate's postgres driver, which sends each migration
// file to PostgreSQL as a single multi-statement Exec — implicitly grouped
// into a single transaction by PG's simple-query protocol. CONCURRENTLY,
// VACUUM, and similar statements refuse to run inside that block.
//
// Steps:
//  1. Acquire the same `pg_advisory_lock` golang-migrate uses, so concurrent
//     replicas booting against the same DB serialize through this code path
//     just as they would through `m.Up()`. This keeps the library's
//     ordering primitive intact when the two paths interleave.
//  2. Mark the version as dirty in `schema_migrations` BEFORE executing,
//     mirroring what the library does — so a crash mid-statement leaves a
//     clear "needs human attention" signal.
//  3. Split the file on `;` boundaries (single-statement granularity) and
//     run each non-empty statement via `db.ExecContext`. Each Exec is its
//     own implicit single-statement transaction in PG — exactly the
//     autocommit shape CONCURRENTLY needs.
//  4. On success, set the version to clean. On failure, leave the dirty
//     marker behind and return the error.
//  5. Release the advisory lock.
func applyNoTx(db *sql.DB, version uint, direction string) error {
	body, err := readMigrationBody(version, direction)
	if err != nil {
		return err
	}
	stmts, err := splitSQLStatements(body)
	if err != nil {
		return fmt.Errorf("split statements: %w", err)
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Resolve current schema once, so the advisory-lock id matches what
	// golang-migrate would generate. The library hashes (database, schema,
	// migrations_table) into an int64. We replicate the inputs by deriving
	// them from the connection.
	dbName, schemaName, err := lookupDBAndSchema(ctx, conn)
	if err != nil {
		return fmt.Errorf("lookup db/schema: %w", err)
	}
	lockID := generateAdvisoryLockID(dbName, schemaName, "schema_migrations")
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockID); err != nil {
			slog.Warn("release advisory lock", "error", err)
		}
	}()

	// Compute the target version we'll record after the SQL runs:
	//   up   → record `version`
	//   down → record version-prior-to-this-one (or 0 if none).
	targetVersion, err := postApplyVersion(version, direction)
	if err != nil {
		return err
	}

	// Mirror golang-migrate: mark dirty BEFORE running, clean AFTER.
	// schema_migrations always carries exactly one row (the library
	// TRUNCATEs+INSERTs to set it). For our intermediate "dirty" state we
	// record this migration's own version with dirty=true regardless of
	// direction. If we crash mid-run, a human will see
	// schema_migrations.version = N, dirty = true and know migration N
	// (or its rollback) failed mid-way — same shape the library leaves
	// after a failed step.
	if err := setSchemaVersion(ctx, conn, version, true); err != nil {
		return fmt.Errorf("mark dirty before no-tx run: %w", err)
	}

	for i, stmt := range stmts {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
	}

	if err := setSchemaVersion(ctx, conn, targetVersion, false); err != nil {
		return fmt.Errorf("mark clean after no-tx run: %w", err)
	}
	return nil
}

// postApplyVersion computes the version that schema_migrations should hold
// once the no-tx migration has finished. golang-migrate's bookkeeping rule:
// up records the migration's own version; down records the version of the
// migration that would re-Up to here (i.e. the previous version), with 0
// meaning "no migrations applied".
func postApplyVersion(v uint, direction string) (uint, error) {
	switch direction {
	case "up":
		return v, nil
	case "down":
		versions, err := embeddedUpVersions()
		if err != nil {
			return 0, err
		}
		var prev uint
		for _, x := range versions {
			if x >= v {
				break
			}
			prev = x
		}
		return prev, nil
	default:
		return 0, fmt.Errorf("unknown direction %q", direction)
	}
}

// setSchemaVersion mirrors the postgres driver's SetVersion: TRUNCATE
// schema_migrations then INSERT a single row. Wrapped in a tx so the table
// is never empty mid-update (concurrent observers see a consistent row).
func setSchemaVersion(ctx context.Context, conn *sql.Conn, version uint, dirty bool) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `TRUNCATE schema_migrations`); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)`,
		version, dirty,
	); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// lookupDBAndSchema resolves the connection's current database name and
// schema name. We need both so the derived advisory-lock id matches what
// golang-migrate would compute for the same DB.
func lookupDBAndSchema(ctx context.Context, conn *sql.Conn) (string, string, error) {
	var dbName, schemaName string
	if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		return "", "", err
	}
	if err := conn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schemaName); err != nil {
		return "", "", err
	}
	return dbName, schemaName, nil
}

// generateAdvisoryLockID re-implements golang-migrate's
// `database.GenerateAdvisoryLockId` so our hybrid runner contends on the
// same numeric lock id the library uses internally. Mirrors the library's
// CRC32(IEEE) over the joined inputs, then multiplied by the library's
// constant salt. Source: golang-migrate/v4/database/util.go.
//
// The library's call shape is GenerateAdvisoryLockId(dbName, schema, table)
// and it joins as `additionalNames || dbName`. We replicate that exactly so
// concurrent boots interleave cleanly between library calls and ours.
func generateAdvisoryLockID(databaseName string, additionalNames ...string) int64 {
	const advisoryLockIDSalt uint32 = 1486364155
	joined := databaseName
	if len(additionalNames) > 0 {
		joined = strings.Join(append(additionalNames, databaseName), "\x00")
	}
	sum := crc32.ChecksumIEEE([]byte(joined))
	sum *= advisoryLockIDSalt
	// pg_advisory_lock takes a bigint. The library passes the value as a
	// decimal string; we widen to int64 for parameter binding clarity.
	return int64(sum)
}

// versionsWithNoTxHeader scans the embedded SQL directory and returns the
// set of versions whose corresponding direction file (.up.sql or .down.sql)
// carries the no-tx header. Direction must be "up" or "down".
func versionsWithNoTxHeader(direction string) (map[uint]struct{}, error) {
	pattern := migrationFilenamePattern
	if direction == "down" {
		pattern = migrationDownFilenamePattern
	}
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	out := map[uint]struct{}{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := pattern.FindStringSubmatch(e.Name())
		if len(m) != 2 {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		body, err := sqlFS.ReadFile("sql/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if hasNoTxHeader(body) {
			out[uint(v)] = struct{}{}
		}
	}
	return out, nil
}

// hasNoTxHeader returns true if the file's leading lines (up to
// noTxHeaderScanLines) contain the exact `noTxHeader` line. We trim each
// line of trailing whitespace and compare verbatim — no fuzzy matching.
func hasNoTxHeader(body []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 4096), 1<<16)
	for i := 0; i < noTxHeaderScanLines && scanner.Scan(); i++ {
		line := strings.TrimRight(scanner.Text(), " \t\r")
		if line == noTxHeader {
			return true
		}
	}
	return false
}

// readMigrationBody loads the raw bytes of the .up.sql or .down.sql for
// `version`. The version-to-filename mapping requires us to scan because
// the suffix after the version prefix is migration-specific.
func readMigrationBody(version uint, direction string) ([]byte, error) {
	pattern := migrationFilenamePattern
	suffix := ".up.sql"
	if direction == "down" {
		pattern = migrationDownFilenamePattern
		suffix = ".down.sql"
	}
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := pattern.FindStringSubmatch(e.Name())
		if len(m) != 2 {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		if uint(v) != version {
			continue
		}
		return sqlFS.ReadFile("sql/" + e.Name())
	}
	return nil, fmt.Errorf("no %s migration for version %04d", suffix, version)
}

// embeddedUpVersions returns all up-migration versions in ascending order.
func embeddedUpVersions() ([]uint, error) {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var versions []uint
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenamePattern.FindStringSubmatch(e.Name())
		if len(m) != 2 {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		versions = append(versions, uint(v))
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions, nil
}

// splitSQLStatements splits an SQL file into individual statements on `;`
// boundaries, respecting single-quoted strings, double-quoted identifiers,
// dollar-quoted bodies (including tagged variants like `$tag$`), and
// `--` line comments. Multi-line `/* … */` comments are passed through —
// they are uncommon in our migrations and the splitter would only fail
// closed (treat the whole comment body as not-a-string), which is safe.
//
// Why we need this: when applying a no-tx migration via plain ExecContext,
// PG's simple-query protocol treats a multi-statement string as an
// implicit transaction block. CONCURRENTLY refuses to run there. Running
// each statement via its own Exec call avoids that grouping.
//
// This splitter is intentionally minimal — we are not building a full SQL
// parser. The migrations that need it (today: just the GIN CONCURRENTLY
// retrofit in 0062) are small and shaped around the supported subset.
func splitSQLStatements(body []byte) ([]string, error) {
	var out []string
	src := string(body)
	var cur strings.Builder

	inLineComment := false
	inBlockComment := false
	inSingle := false
	inDouble := false
	dollarTag := "" // non-empty when inside a dollar-quoted body

	flush := func() {
		s := strings.TrimSpace(cur.String())
		// Strip leading line-comments (e.g. our `-- velox:no-transaction`
		// header) so the resulting statement is "clean SQL". Statements
		// that are entirely comments collapse to "" and are dropped.
		s = stripLeadingComments(s)
		if s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		// Closing handlers first.
		if inLineComment {
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			cur.WriteByte(c)
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				cur.WriteByte('/')
				i++
				inBlockComment = false
			}
			continue
		}
		if inSingle {
			cur.WriteByte(c)
			if c == '\'' {
				// Doubled single-quote is an escape, not a close.
				if i+1 < len(src) && src[i+1] == '\'' {
					cur.WriteByte('\'')
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			cur.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if dollarTag != "" {
			cur.WriteByte(c)
			if c == '$' && strings.HasPrefix(src[i:], dollarTag) {
				cur.WriteString(dollarTag[1:])
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}

		// Outside any string/comment context.
		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inLineComment = true
			cur.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlockComment = true
			cur.WriteByte(c)
			cur.WriteByte('*')
			i++
		case c == '\'':
			inSingle = true
			cur.WriteByte(c)
		case c == '"':
			inDouble = true
			cur.WriteByte(c)
		case c == '$':
			// Try to read `$tag$` or `$$`. The tag is [a-zA-Z0-9_]*.
			j := i + 1
			for j < len(src) {
				bb := src[j]
				if bb == '$' {
					break
				}
				if !isDollarTagByte(bb) {
					break
				}
				j++
			}
			if j < len(src) && src[j] == '$' {
				tag := src[i : j+1] // includes both $s
				dollarTag = tag
				cur.WriteString(tag)
				i = j
				continue
			}
			cur.WriteByte(c)
		case c == ';':
			flush()
		default:
			cur.WriteByte(c)
		}
	}

	if inSingle || inDouble || dollarTag != "" || inBlockComment {
		return nil, fmt.Errorf("unterminated string/comment in SQL body")
	}
	flush()
	return out, nil
}

func isDollarTagByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

// stripLeadingComments removes leading whitespace + `-- …\n` lines from a
// candidate statement. Used by splitSQLStatements so our header line
// doesn't leak into the SQL we send to PG (PG would tolerate it, but
// stripping keeps the wire payload clean for diagnostics).
func stripLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		if !strings.HasPrefix(s, "--") {
			return s
		}
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			return ""
		}
		s = s[nl+1:]
	}
}

// Status reports the current database migration version. Returns (0, false,
// nil) for a fresh database with no schema_migrations table. Opens and closes
// its own short-lived connection pool.
func Status(dsn string) (uint, bool, error) {
	db, err := openMigrationPool(dsn)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = db.Close() }()

	return DatabaseVersion(db)
}

// Rollback rolls back the last `steps` migrations and returns the new
// database version. Opens and closes its own short-lived connection pool.
//
// If any migration in the rollback range was applied with the
// `-- velox:no-transaction` header on its `.down.sql`, that step is run
// outside a transaction (autocommit) — symmetric to Up.
func Rollback(dsn string, steps int) (uint, error) {
	if steps <= 0 {
		return 0, fmt.Errorf("rollback steps must be positive, got %d", steps)
	}

	db, err := openMigrationPool(dsn)
	if err != nil {
		return 0, err
	}

	noTxDownVersions, err := versionsWithNoTxHeader("down")
	if err != nil {
		_ = db.Close()
		return 0, err
	}

	// Fast path — no no-tx down files; let the library do everything.
	if len(noTxDownVersions) == 0 {
		m, err := newMigrator(db)
		if err != nil {
			_ = db.Close()
			return 0, err
		}
		defer closeMigrator(m)

		if err := m.Steps(-steps); err != nil {
			return 0, fmt.Errorf("rollback %d step(s): %w", steps, err)
		}
		v, _, _ := m.Version()
		return v, nil
	}

	_ = db.Close()
	return rollbackHybrid(dsn, steps, noTxDownVersions)
}

// rollbackHybrid is the symmetric counterpart to upHybrid for the down
// direction. Walks back N steps, dispatching each to the library
// (`Steps(-1)`) or our autocommit applier as the down file requires.
// Serialized under the same hybrid loop lock as upHybrid (ADR-073).
func rollbackHybrid(dsn string, steps int, noTxDown map[uint]struct{}) (uint, error) {
	release, err := acquireHybridLoopLock(dsn)
	if err != nil {
		return 0, err
	}
	defer release()

	statusDB, err := openMigrationPool(dsn)
	if err != nil {
		return 0, err
	}
	defer func() { _ = statusDB.Close() }()

	versions, err := embeddedUpVersions()
	if err != nil {
		return 0, err
	}
	var lastVer uint
	for i := 0; i < steps; i++ {
		curVer, dirty, err := DatabaseVersion(statusDB)
		if err != nil {
			return lastVer, fmt.Errorf("read schema_migrations: %w", err)
		}
		if dirty {
			return curVer, fmt.Errorf("schema_migrations is dirty at version %d — refuse to step backward", curVer)
		}
		if curVer == 0 {
			break
		}
		if !contains(versions, curVer) {
			return curVer, fmt.Errorf("current version %d not in embedded migration set", curVer)
		}
		lastVer = curVer
		if _, isNoTx := noTxDown[curVer]; isNoTx {
			start := time.Now()
			if err := applyNoTxOnPool(dsn, curVer, "down"); err != nil {
				return curVer, fmt.Errorf("apply no-tx migration %04d down: %w", curVer, err)
			}
			slog.Info("no-tx migration applied",
				"version", curVer,
				"direction", "down",
				"duration_ms", time.Since(start).Milliseconds(),
			)
			continue
		}
		if err := stepOneViaLibrary(dsn, -1); err != nil {
			return curVer, fmt.Errorf("rollback %04d via library: %w", curVer, err)
		}
	}
	v, _, _ := DatabaseVersion(statusDB)
	return v, nil
}

func contains(xs []uint, x uint) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// EmbeddedMigrationCount returns the number of .up.sql files in the embedded
// migrations directory. Differs from EmbeddedLatestVersion when version
// numbers are non-contiguous (e.g. one branch skipped a number to
// coordinate with a sibling parallel branch). The migrate library's
// Steps(-N) operates on a per-file basis, so callers that want to roll
// back "every applied migration" should use this count, not the latest
// version number.
func EmbeddedMigrationCount() (int, error) {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}

	var n int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if migrationFilenamePattern.MatchString(entry.Name()) {
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no up-migrations found in embedded fs")
	}
	return n, nil
}

// EmbeddedLatestVersion returns the highest migration version packaged into
// this binary. Used by CheckSchemaReady to compare against the database's
// current version.
func EmbeddedLatestVersion() (uint, error) {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}

	var latest uint
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := migrationFilenamePattern.FindStringSubmatch(entry.Name())
		if len(m) != 2 {
			continue
		}
		n, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		if uint(n) > latest {
			latest = uint(n)
		}
	}
	if latest == 0 {
		return 0, fmt.Errorf("no up-migrations found in embedded fs")
	}
	return latest, nil
}

// DatabaseVersion returns the current migration version recorded in the
// database, along with whether the last migration left the schema in a
// dirty (partially-applied) state. Returns (0, false, nil) if the
// schema_migrations table does not yet exist (fresh database).
//
// Queries schema_migrations directly rather than going through the migrate
// library so we don't need to construct a Migrate instance (which opens
// connections and prepares source drivers for no reason if we just want
// to read a version number).
func DatabaseVersion(db *sql.DB) (uint, bool, error) {
	var version int64
	var dirty bool
	err := db.QueryRow(`SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	// Fresh DB — schema_migrations table doesn't exist yet.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read schema_migrations: %w", err)
	}
	if version < 0 {
		return 0, dirty, fmt.Errorf("invalid negative version %d in schema_migrations", version)
	}
	return uint(version), dirty, nil
}

// CheckSchemaReady verifies the database schema matches what this binary
// expects. It returns an error if:
//   - The database is in a dirty migration state (previous migration failed
//     mid-way; a human needs to decide whether to rollback or fix-forward).
//   - The database schema version is behind the embedded latest version
//     (the app would return 500s on any query touching new columns/tables).
//
// Call at startup, AFTER optionally running migrations, BEFORE opening the
// HTTP server. Refusing to start is safer than serving with a stale schema:
// the orchestrator (Kubernetes, systemd, etc.) will retry, and by then
// migrations should have completed.
func CheckSchemaReady(db *sql.DB) error {
	embedded, err := EmbeddedLatestVersion()
	if err != nil {
		return fmt.Errorf("read embedded version: %w", err)
	}

	dbVer, dirty, err := DatabaseVersion(db)
	if err != nil {
		return fmt.Errorf("read database version: %w", err)
	}

	if dirty {
		return fmt.Errorf("database at version %d is in a dirty migration state — prior migration failed; run `velox migrate status` and fix before restarting", dbVer)
	}

	if dbVer < embedded {
		return fmt.Errorf("schema behind code: database at version %d, binary expects version %d — run migrations before starting the app", dbVer, embedded)
	}

	if dbVer > embedded {
		// dbVer > embedded means THIS (older) binary is running against a
		// NEWER schema — a rolling deploy overlap or a rollback. Today we
		// WARN and continue: old code may not understand new columns/enum
		// values, and no expand/contract migration discipline is enforced,
		// so this window is governed by nothing but migration authors'
		// care. Whether to refuse instead is an open decision — see
		// docs/records/ha-readiness-2026-07-06.md (N=2 hazard: old-code-on-
		// new-schema windows) before tightening.
		slog.Warn("schema ahead of binary — likely a rollback in progress",
			"database_version", dbVer,
			"binary_version", embedded,
		)
	} else {
		slog.Info("schema ready",
			"version", dbVer,
			"binary_expects", embedded,
		)
	}
	return nil
}

// closeMigrator closes a Migrate instance, logging any error. The library
// returns two errors (source, database); we combine into one log line.
func closeMigrator(m *migrate.Migrate) {
	srcErr, dbErr := m.Close()
	if srcErr != nil || dbErr != nil {
		slog.Warn("migrate close",
			"source_error", srcErr,
			"database_error", dbErr,
		)
	}
}
