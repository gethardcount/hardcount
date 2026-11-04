// Package testutil holds helpers that tests share: the integration test
// database (SetupTestDB, AdminPool), a wait for a backend blocked on a lock
// (WaitForLockWaiter), and small readers for credit balances and the mode
// on a context.
package testutil

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/gethardcount/hardcount/internal/platform/migrate"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

const (
	// defaultAdminDBURL is the superuser URL, used for migrations and
	// cleanup. It bypasses RLS. It points at the hardcount_test database, so
	// tests never touch dev data.
	defaultAdminDBURL = "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"
	// defaultAppDBURL is the app user URL, used for queries, with RLS
	// enforced.
	defaultAppDBURL = "postgres://hardcount_test_app:hardcount_test_app@localhost:5432/hardcount_test?sslmode=disable"
)

// utcOnce pins the test process to UTC, exactly like cmd/hardcount/main
// (ADR-075). Integration tests then see the same canonical-UTC timestamps
// as production, whatever the host's time zone. Otherwise a dev machine on
// IST would scan timestamptz back in +05:30, unlike CI and production.
// sync.Once keeps the global assignment race-free across parallel
// integration tests.
var utcOnce sync.Once

// SetupTestDB runs migrations as superuser, cleans data, and returns an
// app-user connection where RLS is enforced.
func SetupTestDB(t *testing.T) *postgres.DB {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test (use -short=false)")
	}
	utcOnce.Do(func() { time.Local = time.UTC })

	adminURL := envOr("TEST_ADMIN_DATABASE_URL", defaultAdminDBURL)
	appURL := envOr("TEST_DATABASE_URL", defaultAppDBURL)

	runMigrations(t, adminURL)

	adminPool := openPool(t, adminURL)
	// Truncate once, at setup. Truncating at setup guarantees a clean start
	// however the previous test ended: a test that stopped part-way with
	// Fatalf leaves rows behind, and the next setup wipes them. A second
	// truncate in t.Cleanup would add nothing and double the ACCESS
	// EXCLUSIVE lock churn (700 truncates per integration run where 350
	// will do). One consequence: the last test in a process leaves its rows
	// behind. The CI hardcount-doctor sweep after the integration job
	// examines whatever the alphabetically last DB package left.
	cleanDB(t, adminPool)

	// The app connection runs the actual queries, with RLS enforced.
	appPool := openPool(t, appURL)
	db := postgres.NewDB(appPool, 5*time.Second)

	t.Cleanup(func() {
		_ = appPool.Close()
		_ = adminPool.Close()
	})

	return db
}

// OpenTracedDB opens a second app-user connection to the test database,
// with RLS enforced, whose every statement passes through tracer. Call it
// after SetupTestDB, which migrates and cleans. It is for tests that assert
// how many queries or transactions a read path costs. An N+1 regression
// shows up as a count that grows with the row count (issue #1142).
func OpenTracedDB(t *testing.T, tracer pgx.QueryTracer) *postgres.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(envOr("TEST_DATABASE_URL", defaultAppDBURL))
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	cfg.Tracer = tracer
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(5)
	t.Cleanup(func() { _ = pool.Close() })
	return postgres.NewDB(pool, 5*time.Second)
}

// OpenExecModeDB opens an app-user connection to the test database that sends
// every statement in the given pgx exec mode. An operator can pick the mode
// with default_query_exec_mode in the database URL
// (docs/ops/postgres-requirements.md), so a test of how a parameter is
// encoded runs in each one. It needs no migrations or clean tables.
func OpenExecModeDB(t *testing.T, mode pgx.QueryExecMode) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test (use -short=false)")
	}
	cfg, err := pgx.ParseConfig(envOr("TEST_DATABASE_URL", defaultAppDBURL))
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	cfg.DefaultQueryExecMode = mode
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func openPool(t *testing.T, url string) *sql.DB {
	t.Helper()

	// Use the "pgx" driver (the pgx stdlib adapter), as production does.
	pool, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open db (%s): %v", url, err)
	}
	pool.SetMaxOpenConns(5)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.PingContext(ctx); err != nil {
		t.Fatalf("ping db (%s): %v", url, err)
	}
	return pool
}

func cleanDB(t *testing.T, pool *sql.DB) {
	t.Helper()
	if err := cleanDBErr(pool); err != nil {
		t.Fatalf("clean db: %v", err)
	}
}

// cleanDBErr truncates every public base table except the migration
// record (goose_db_version), in one statement. The table list comes from
// the catalog at call time.
//
// The list is derived, so it cannot go stale the way a hand-maintained
// list can. There is no EXCEPTION handler, because an error here is a real
// error and must surface. With a hand-maintained list, a handler such as
// `EXCEPTION WHEN undefined_table THEN NULL` would swallow the whole
// TRUNCATE once the list named a dropped table. Cleanup would then do
// nothing for a whole run, without a word.
//
// The migration record is excluded because truncating it leaves the schema
// in place with no record. The next process's migrate.Up then refuses the
// database (ErrNoMigrationRecord: tables, no record), and runMigrations
// drops and rebuilds everything. That is a slower route into the same
// class of silent no-op.
//
// There is no CASCADE. Every table is in the list, so foreign-key order
// does not matter.
//
// audit_log carries a statement-level BEFORE TRUNCATE trigger that blocks
// TRUNCATE, to keep the log tamper-evident. Replica mode turns it off for
// this transaction only.
func cleanDBErr(pool *sql.DB) error {
	_, err := pool.ExecContext(context.Background(), `
		DO $$
		DECLARE tbls text;
		BEGIN
			PERFORM set_config('session_replication_role', 'replica', true);
			SELECT string_agg(quote_ident(tablename), ', ')
			  INTO tbls
			  FROM pg_tables
			 WHERE schemaname = 'public'
			   AND tablename <> 'goose_db_version';
			IF tbls IS NULL THEN
				RAISE EXCEPTION 'testutil.cleanDB: no public tables found — migrations not applied?';
			END IF;
			EXECUTE 'TRUNCATE ' || tbls;
		END $$;
	`)
	return err
}

// CreateTestTenant inserts a tenant through the app connection, bypassing
// RLS. The tenants table has no RLS, so the insert works directly.
func CreateTestTenant(t *testing.T, db *postgres.DB, name string) string {
	t.Helper()

	id := postgres.NewID("hc_ten")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := db.Pool.ExecContext(ctx,
		`INSERT INTO tenants (id, name, status) VALUES ($1, $2, 'active')`, id, name)
	if err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	return id
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

// migrateOnce, migrateErr and migrateRuns hold the one migration run of
// this test process. migrateRuns is exported for the harness's own test,
// which checks that migrations run only once.
var (
	migrateOnce sync.Once
	migrateErr  error
	migrateRuns int
)

// runMigrations applies pending migrations once per test process, which is
// one package's test binary. Every SetupTestDB after the first reuses the
// result. Those later calls do not open a migration pool, take goose's
// advisory lock or check the version again, which would cost ~13 round
// trips per package. On failure, it drops everything and retries from
// scratch. That is safe because this is a test-only database. migrate.Up
// manages its own pool, so the DSN is all it needs.
func runMigrations(t *testing.T, adminURL string) {
	t.Helper()
	migrateOnce.Do(func() {
		migrateRuns++
		if err := migrate.Up(adminURL); err == nil {
			return
		}
		// migrate.Up cannot continue from some states: tables with no goose
		// record, or a version from another branch, which is common after
		// switching branches. Drop everything and retry.
		nukePool, err := sql.Open("pgx", adminURL)
		if err != nil {
			migrateErr = err
			return
		}
		defer func() { _ = nukePool.Close() }()
		if err := dropAllTablesErr(nukePool); err != nil {
			migrateErr = err
			return
		}
		migrateErr = migrate.Up(adminURL)
	})
	if migrateErr != nil {
		t.Fatalf("run migrations: %v", migrateErr)
	}
}

// dropAllTablesErr drops every object in public that no extension owns:
// tables (the migration record with them), views, sequences, functions,
// procedures and types. The baseline creates its functions and types with
// plain CREATE, so anything left behind would fail it.
func dropAllTablesErr(pool *sql.DB) error {
	_, err := pool.ExecContext(context.Background(), `
		DO $$
		DECLARE stmts text[]; stmt text;
		BEGIN
			-- Every statement is built before the first DROP: a name read
			-- after a CASCADE dropped its object would print as a bare OID.
			SELECT array_agg(s) INTO stmts FROM (
				SELECT format('DROP %s IF EXISTS %s CASCADE',
				              CASE c.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW'
				                             WHEN 'S' THEN 'SEQUENCE' WHEN 'f' THEN 'FOREIGN TABLE' ELSE 'TABLE' END,
				              c.oid::regclass)
				FROM pg_class c
				WHERE c.relnamespace = 'public'::regnamespace
				  AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
				  AND NOT EXISTS (SELECT 1 FROM pg_depend d
				                  WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('e', 'i', 'a'))
				UNION ALL
				SELECT format('DROP %s IF EXISTS %s CASCADE',
				              CASE p.prokind WHEN 'a' THEN 'AGGREGATE' WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END,
				              p.oid::regprocedure)
				FROM pg_proc p
				WHERE p.pronamespace = 'public'::regnamespace
				  AND NOT EXISTS (SELECT 1 FROM pg_depend d
				                  WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
				UNION ALL
				SELECT format('DROP TYPE IF EXISTS %s CASCADE', t.oid::regtype)
				FROM pg_type t
				WHERE t.typnamespace = 'public'::regnamespace
				  AND t.typtype IN ('e', 'd', 'c', 'r', 'm')
				  AND (t.typrelid = 0 OR (SELECT relkind FROM pg_class WHERE oid = t.typrelid) = 'c')
				  AND NOT EXISTS (SELECT 1 FROM pg_depend d
				                  WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
			) q(s);
			FOREACH stmt IN ARRAY coalesce(stmts, '{}') LOOP
				EXECUTE stmt;
			END LOOP;
		END $$;
	`)
	return err
}

// AdminPool opens the owner-role connection, the same role that runs
// migrations. It is for tests that must run DDL, for example to replay a
// shipped migration's data transform against a seeded pre-migration shape.
// The app role cannot drop or recreate indexes it does not own.
func AdminPool(t *testing.T) *sql.DB {
	t.Helper()
	pool := openPool(t, envOr("TEST_ADMIN_DATABASE_URL", defaultAdminDBURL))
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}
