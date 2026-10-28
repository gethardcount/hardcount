package migrate

import (
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// The baseline keeps the properties migration 180 relies on, so an edit
// cannot break them: no session settings, no transaction control (goose's
// transaction holds the whole baseline and row 180), and grants only on
// Hardcount's own objects, only to hardcount_app. That it applies under each role
// setup is the migrations CI job's (scripts/ci/migrations-fresh-install.sh).
// Mutation: a COMMIT, a GRANT ... ON SCHEMA or a grant to another role
// added to the baseline; each fails here.
func TestBaselineFile(t *testing.T) {
	if !strings.HasPrefix(baselineSQL, "-- The starting schema: migration 180.") {
		t.Fatal("the baseline lost its header")
	}
	var sets int
	for i, line := range strings.Split(baselineSQL, "\n") {
		switch {
		case strings.HasPrefix(line, `\`):
			t.Errorf("line %d is a psql meta-command: %s", i+1, line)
		case strings.HasPrefix(line, "SET ") && !strings.HasPrefix(line, "SET LOCAL "):
			t.Errorf("line %d sets a session value, which would leak into the next file: %s", i+1, line)
		case strings.HasPrefix(line, "SET LOCAL "):
			sets++
		case strings.HasPrefix(line, "COMMENT ON EXTENSION"):
			t.Errorf("line %d needs the extension's owner: %s", i+1, line)
		case strings.Contains(line, "ALTER DEFAULT PRIVILEGES"):
			t.Errorf("line %d: %s", i+1, line)
		}
	}
	if sets == 0 {
		t.Error("no SET LOCAL lines: the header SETs are missing")
	}
	for _, want := range []string{
		"SELECT pg_catalog.set_config('search_path', '', true);",
		"CREATE TABLE public.tenants (",
		"INSERT INTO public.leader_leases (role, holder_token)\nSELECT r, (extract(epoch FROM clock_timestamp()) * 1000)::bigint",
		"REVOKE UPDATE, DELETE, TRUNCATE ON TABLE public.audit_log FROM hardcount_app;",
	} {
		if !strings.Contains(baselineSQL, want) {
			t.Errorf("the baseline lacks %q", want)
		}
	}
	if strings.Contains(baselineSQL, "set_config('search_path', '', false)") {
		t.Error("search_path is set for the session")
	}

	stmts, err := splitSQLStatements([]byte(baselineSQL))
	if err != nil {
		t.Fatalf("split the baseline: %v", err)
	}
	for i, raw := range stmts {
		s := strings.TrimSuffix(normalizeDDL(raw), ";")
		switch {
		// Migration 180 runs inside goose's transaction, which also writes
		// row 180. A COMMIT would keep the tables before it when a later
		// statement fails, with no row 180.
		case txControl.MatchString(s):
			t.Errorf("statement %d controls the transaction: %q", i+1, s)
		case !grantStatement.MatchString(s):
		case strings.Contains(s, " ON SCHEMA "):
			t.Errorf("statement %d grants on a schema, which belongs to the install, not to Hardcount: %q", i+1, s)
		case !strings.HasSuffix(s, " TO HARDCOUNT_APP") && !strings.HasSuffix(s, " FROM HARDCOUNT_APP"):
			t.Errorf("statement %d grants to a role other than hardcount_app: %q", i+1, s)
		}
	}
}

var grantStatement = regexp.MustCompile(`^(GRANT|REVOKE)\b`)

// The embedded set as goose sees it: migration 180, then the files in
// sql/, with no other source. It builds the provider over a handle that
// never connects: goose reads no database to list its sources.
func TestEmbeddedSources(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	files, err := embeddedFiles(db, migrationFiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || files[0].Version != baselineVersion || files[0].Name != baselineName {
		t.Fatalf("want migration 180 first, got %+v", files)
	}
	for _, f := range files[1:] {
		if f.Version <= baselineVersion {
			t.Errorf("%s: files in sql/ start at 00181", f.Name)
		}
	}
	p, err := newProvider(db, migrationFiles(), true)
	if err != nil {
		t.Fatal(err)
	}
	if src := p.ListSources()[0]; src.Type != goose.TypeGo || src.Path != "" {
		t.Fatalf("migration 180 should be the registered Go migration, got %+v", src)
	}
}
