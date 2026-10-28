package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3/database"
)

// Helpers for the real-Postgres tests in this package. Each test gets a
// scratch database of its own, so the shared hardcount_test database the
// other packages use is never touched.

func adminTestURL() string {
	if u := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL")); u != "" {
		return u
	}
	return "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"
}

// scratchDB creates an empty database and returns its DSN and a pool for
// the test's own queries. Both go away when the test ends.
func scratchDB(t *testing.T, name string) (string, *sql.DB) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	admin, err := sql.Open("pgx", adminTestURL())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name) + ` WITH (FORCE)`); err != nil {
		t.Fatalf("drop scratch db: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + quoteIdent(name)); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	dsn := withDBName(t, adminTestURL(), name)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		admin, err := sql.Open("pgx", adminTestURL())
		if err != nil {
			return
		}
		defer func() { _ = admin.Close() }()
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name) + ` WITH (FORCE)`); err != nil {
			t.Logf("drop scratch db: %v", err)
		}
	})
	return dsn, db
}

func withDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// files builds the migrations after 180 for one test, as goose reads them.
func files(named map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range named {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// noFiles is the set with only migration 180, which is always registered.
var noFiles = fstest.MapFS{}

// fastLockWait makes a waiting command poll every second, `polls` times.
func fastLockWait(t *testing.T, polls uint64) {
	t.Helper()
	period, n := lockWaitPeriod, lockWaitPolls
	lockWaitPeriod, lockWaitPolls = 1, polls
	t.Cleanup(func() { lockWaitPeriod, lockWaitPolls = period, n })
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", oneLine(query), err)
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func tableExists(t *testing.T, db *sql.DB, qualified string) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, qualified).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// gooseRows returns how many rows goose_db_version holds for a version,
// 0 when the table does not exist.
func gooseRows(t *testing.T, db *sql.DB, version int64) int {
	t.Helper()
	if !tableExists(t, db, versionTable) {
		return 0
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM public.goose_db_version WHERE version_id = $1`, version).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// buildV08 turns a database that Up has migrated into what Velox v0.8.x
// leaves: the tables, no goose record, and golang-migrate's two-column
// record at (180, false). The baseline was generated from v0.8.1's schema
// (ADR-125).
func buildV08(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP TABLE public.goose_db_version`)
	writeV08Record(t, db, 180, false)
}

// writeV08Record creates golang-migrate's record in public exactly as it
// does: CREATE TABLE IF NOT EXISTS, then TRUNCATE and INSERT.
func writeV08Record(t *testing.T, db *sql.DB, version int64, dirty bool) {
	t.Helper()
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS public.schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`)
	mustExec(t, db, `TRUNCATE public.schema_migrations`)
	mustExec(t, db, `INSERT INTO public.schema_migrations (version, dirty) VALUES ($1, $2)`, version, dirty)
}

// recordGoose writes goose's record by hand, the way goose does: the
// table, row 0, then one row per version.
func recordGoose(t *testing.T, db *sql.DB, versions ...int64) {
	t.Helper()
	store, err := versionStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !tableExists(t, db, versionTable) {
		if err := store.CreateVersionTable(ctx, db); err != nil {
			t.Fatal(err)
		}
		versions = append([]int64{0}, versions...)
	}
	for _, v := range versions {
		if err := store.Insert(ctx, db, database.InsertRequest{Version: v}); err != nil {
			t.Fatal(err)
		}
	}
}

// fingerprint summarises everything a failed or no-op command must not
// change: the relations in public, their comments, golang-migrate's old
// record and goose's.
func fingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	var rels, sm, goose sql.NullString
	if err := db.QueryRow(`
		SELECT string_agg(c.relname || ':' || c.relkind::text || ':' || coalesce(obj_description(c.oid, 'pg_class'), ''), ',' ORDER BY c.relname)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'`).Scan(&rels); err != nil {
		t.Fatal(err)
	}
	if tableExists(t, db, "public.schema_migrations") {
		if err := db.QueryRow(`SELECT string_agg(version || '/' || dirty, ',') FROM public.schema_migrations`).Scan(&sm); err != nil {
			t.Fatal(err)
		}
	}
	if tableExists(t, db, versionTable) {
		if err := db.QueryRow(`SELECT string_agg(version_id::text, ',' ORDER BY id) FROM public.goose_db_version`).Scan(&goose); err != nil {
			t.Fatal(err)
		}
	}
	return rels.String + "|" + sm.String + "|" + goose.String
}
