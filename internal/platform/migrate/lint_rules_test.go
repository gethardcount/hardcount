package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
)

// Fixtures for lint_test.go. Each rule has at least one directory that
// must fail it, and the valid shapes must pass, so the gate is neither
// vacuous nor stricter than docs/dev/migrations.md says.

// fixtureBaseline stands in for the v0.8.1 baseline: one table with a
// primary key and one plain index, written the way pg_dump writes them.
const fixtureBaseline = `
CREATE TABLE public.invoices (
    id text NOT NULL,
    tenant_id text NOT NULL,
    number text
);
ALTER TABLE ONLY public.invoices
    ADD CONSTRAINT invoices_pkey PRIMARY KEY (id);
CREATE INDEX idx_invoices_tenant ON public.invoices USING btree (tenant_id);
`

const okNormalFile = `-- +goose Up
-- Invoices get a free-text memo.
ALTER TABLE invoices ADD COLUMN memo TEXT;
-- +goose Down
ALTER TABLE invoices DROP COLUMN memo;
`

func indexBuildFile(name string) string {
	return "-- +goose NO TRANSACTION\n-- +goose Up\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS " + name +
		";\nCREATE INDEX CONCURRENTLY " + name + " ON invoices (number);\nRESET lock_timeout;\n" +
		"-- +goose Down\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS " + name + ";\nRESET lock_timeout;\n"
}

func indexDropFile(name string) string {
	return "-- +goose NO TRANSACTION\n-- +goose Up\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS " + name +
		";\nRESET lock_timeout;\n-- +goose Down\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS " + name +
		";\nCREATE INDEX CONCURRENTLY " + name + " ON invoices (tenant_id);\nRESET lock_timeout;\n"
}

func normalFile(up, down string) string {
	return "-- +goose Up\n" + up + "\n-- +goose Down\n" + down + "\n"
}

func TestLintMigrationDir(t *testing.T) {
	const noFinding = ""
	cases := []struct {
		name  string
		files map[string]string
		want  string // a part of an expected finding, or noFinding
	}{
		// Valid shapes.
		{"only the README", map[string]string{"README.md": "# Migrations"}, noFinding},
		{"a normal file", map[string]string{"00181_add_invoice_memo.sql": okNormalFile}, noFinding},
		{"an index build", map[string]string{"00181_idx_invoices_number.sql": indexBuildFile("idx_invoices_number")}, noFinding},
		{"a unique index build, upper case, quoted name", map[string]string{"00181_idx_invoices_number.sql": strings.NewReplacer(
			"CREATE INDEX", "create unique index", "idx_invoices_number", `"idx_invoices_number"`, "SET lock_timeout = 0", "set lock_timeout to 0",
		).Replace(indexBuildFile("idx_invoices_number"))}, noFinding},
		{"a normal file, then an index build", map[string]string{
			"00181_add_invoice_memo.sql":    okNormalFile,
			"00182_idx_invoices_number.sql": indexBuildFile("idx_invoices_number"),
		}, noFinding},
		{"dropping a baseline index", map[string]string{"00181_drop_idx_invoices_tenant.sql": indexDropFile("idx_invoices_tenant")}, noFinding},
		{"dropping an index an earlier file built", map[string]string{
			"00181_idx_invoices_number.sql":      indexBuildFile("idx_invoices_number"),
			"00182_drop_idx_invoices_number.sql": indexDropFile("public.idx_invoices_number"),
		}, noFinding},
		{"a function body between StatementBegin and StatementEnd", map[string]string{"00181_add_fn.sql": normalFile(
			"-- +goose StatementBegin\nCREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$\nBEGIN\n  PERFORM 1;\nEND\n$$;\n-- +goose StatementEnd",
			"DROP FUNCTION f();")}, noFinding},
		{"SET LOCAL, SET CONSTRAINTS and a transaction-local set_config", map[string]string{"00181_x.sql": normalFile(
			"SET LOCAL lock_timeout = '30s';\nSET CONSTRAINTS ALL DEFERRED;\nSELECT set_config('search_path', 'public', true);\nALTER TABLE invoices ADD COLUMN memo TEXT;",
			"ALTER TABLE invoices DROP COLUMN memo;")}, noFinding},
		{"a blocking statement with a reason", map[string]string{"00181_idx.sql": normalFile(
			"-- velox:blocking-ddl-ok: fx_rates has under 200 rows on every install\nCREATE INDEX idx_fx ON fx_rates (code);",
			"DROP INDEX idx_fx;")}, noFinding},

		// File names.
		{"a dash instead of an underscore", map[string]string{"00181-add_invoice_memo.sql": okNormalFile}, "NNNNN_name.sql"},
		{"three digits", map[string]string{"181_add_invoice_memo.sql": okNormalFile}, "NNNNN_name.sql"},
		{"upper case in the name", map[string]string{"00181_AddInvoiceMemo.sql": okNormalFile}, "NNNNN_name.sql"},
		{"upper-case extension", map[string]string{"00181_add_invoice_memo.SQL": okNormalFile}, "NNNNN_name.sql"},
		{"golang-migrate's .up.sql", map[string]string{"00181_add_invoice_memo.up.sql": okNormalFile}, "NNNNN_name.sql"},
		{"a hidden file", map[string]string{".00181_add_invoice_memo.sql": okNormalFile}, "NNNNN_name.sql"},
		{"a subdirectory", map[string]string{"new/00181_add_invoice_memo.sql": okNormalFile}, "top level"},
		{"version zero", map[string]string{"00000_add_invoice_memo.sql": okNormalFile}, "does not collect"},

		// Versions.
		{"two files with one version", map[string]string{
			"00181_add_invoice_memo.sql": okNormalFile,
			"00181_add_other.sql":        okNormalFile,
		}, "duplicate"},
		{"version 180 is the baseline", map[string]string{"00180_add_invoice_memo.sql": okNormalFile}, "duplicate"},
		{"the first file is not 181", map[string]string{"00182_add_invoice_memo.sql": okNormalFile}, "should be 00181"},
		{"a gap", map[string]string{
			"00181_add_invoice_memo.sql": okNormalFile,
			"00183_add_other.sql":        okNormalFile,
		}, "should be 00182"},

		// Annotations and sections.
		{"no Up line", map[string]string{"00181_x.sql": "ALTER TABLE invoices ADD COLUMN memo TEXT;\n-- +goose Down\nSELECT 1;\n"}, "SQL before"},
		{"no Down line", map[string]string{"00181_x.sql": "-- +goose Up\nALTER TABLE invoices ADD COLUMN memo TEXT;\n"}, "0 -- +goose Down lines"},
		{"two Up lines", map[string]string{"00181_x.sql": "-- +goose Up\n-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n"}, "2 -- +goose Up lines"},
		{"Down before Up", map[string]string{"00181_x.sql": "-- +goose Down\nSELECT 1;\n-- +goose Up\nSELECT 1;\n"}, "must come after"},
		{"an empty Up", map[string]string{"00181_x.sql": normalFile("-- nothing yet", "SELECT 1;")}, "Up section has no statement"},
		{"an empty Down", map[string]string{"00181_x.sql": normalFile("ALTER TABLE invoices ADD COLUMN memo TEXT;", "")}, "Down section has no statement"},
		{"ENVSUB", map[string]string{"00181_x.sql": "-- +goose ENVSUB ON\n" + okNormalFile}, "not allowed"},
		{"+goose inside a comment", map[string]string{"00181_x.sql": normalFile("-- put the body between -- +goose StatementBegin and End\nSELECT 1;", "SELECT 1;")}, "not allowed"},
		{"an indented annotation", map[string]string{"00181_x.sql": "  -- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n"}, "space before"},

		// How goose cuts statements.
		{"a one-line $$ body needs no StatementBegin", map[string]string{"00181_add_fn.sql": normalFile(
			"CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1; $$;", "DROP FUNCTION f();")}, noFinding},
		{"a multi-line $$ body without StatementBegin", map[string]string{"00181_add_fn.sql": normalFile(
			"CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$\nBEGIN\n  PERFORM 1;\nEND\n$$;", "DROP FUNCTION f();")}, "cut in two"},
		{"a StatementBegin with no StatementEnd", map[string]string{"00181_add_fn.sql": normalFile(
			"-- +goose StatementBegin\nCREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$\nBEGIN\n  PERFORM 1;\nEND\n$$;", "DROP FUNCTION f();")}, "no StatementEnd"},
		{"a StatementEnd with no StatementBegin", map[string]string{"00181_x.sql": normalFile("SELECT 1;\n-- +goose StatementEnd", "SELECT 1;")}, "no StatementBegin"},
		{"a last statement with no closing semicolon", map[string]string{"00181_x.sql": normalFile("ALTER TABLE invoices ADD COLUMN memo TEXT", "SELECT 1;")}, "does not end with"},
		{"two statements on one line of an index build", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "SET lock_timeout = 0;\nDROP", "SET lock_timeout = 0; DROP", 1)}, "line of its own"},

		// Normal files: transaction control and session settings.
		{"BEGIN and COMMIT", map[string]string{"00181_x.sql": normalFile("BEGIN;\nALTER TABLE invoices ADD COLUMN memo TEXT;\nCOMMIT;", "ALTER TABLE invoices DROP COLUMN memo;")}, "controls its own transaction"},
		{"a plain SET", map[string]string{"00181_x.sql": normalFile("SET lock_timeout = 0;\nALTER TABLE invoices ADD COLUMN memo TEXT;", "ALTER TABLE invoices DROP COLUMN memo;")}, "Use SET LOCAL"},
		{"SET search_path", map[string]string{"00181_x.sql": normalFile("SET search_path TO other;\nSELECT 1;", "SELECT 1;")}, "Use SET LOCAL"},
		{"SET SESSION", map[string]string{"00181_x.sql": normalFile("SET SESSION statement_timeout = 0;\nSELECT 1;", "SELECT 1;")}, "Use SET LOCAL"},
		{"RESET", map[string]string{"00181_x.sql": normalFile("SELECT 1;", "RESET lock_timeout;")}, "Use SET LOCAL"},
		{"set_config for the session", map[string]string{"00181_x.sql": normalFile("SELECT pg_catalog.set_config('search_path', '', false);\nSELECT 1;", "SELECT 1;")}, "Use SET LOCAL"},

		// Index builds.
		{"a build without the DROP", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "DROP INDEX CONCURRENTLY IF EXISTS idx_n;\nCREATE", "CREATE", 1)}, "exactly this shape"},
		{"a build with IF NOT EXISTS", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "CONCURRENTLY idx_n ON", "CONCURRENTLY IF NOT EXISTS idx_n ON", 1)}, "IF NOT EXISTS is not allowed"},
		{"a build without CONCURRENTLY", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "INDEX CONCURRENTLY idx_n ON", "INDEX idx_n ON", 1)}, "exactly this shape"},
		{"a build without the lock_timeout lines", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "RESET lock_timeout;\n-- +goose Down", "-- +goose Down", 1)}, "exactly this shape"},
		{"two indexes in one file", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "RESET lock_timeout;\n-- +goose Down",
			"CREATE INDEX CONCURRENTLY idx_m ON invoices (tenant_id);\nRESET lock_timeout;\n-- +goose Down", 1)}, "exactly this shape"},
		{"a build with another statement", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "SET lock_timeout = 0;\nDROP",
			"ALTER TABLE invoices ADD COLUMN memo TEXT;\nSET lock_timeout = 0;\nDROP", 1)}, "exactly this shape"},
		{"the DROP names another index", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "IF EXISTS idx_n;\nCREATE", "IF EXISTS idx_m;\nCREATE", 1)}, "different indexes"},
		{"a Down that does not undo the Up", map[string]string{"00181_idx.sql": strings.Replace(indexBuildFile("idx_n"), "-- +goose Down\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS idx_n;\n",
			"-- +goose Down\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS idx_n;\nCREATE INDEX CONCURRENTLY idx_n ON invoices (number);\n", 1)}, "must undo the Up"},
		{"a build named like a baseline index", map[string]string{"00181_idx.sql": indexBuildFile("idx_invoices_tenant")}, "already exists"},
		{"a build named like a baseline constraint", map[string]string{"00181_idx.sql": indexBuildFile("invoices_pkey")}, "already exists"},
		{"a build named like an earlier file's index", map[string]string{
			"00181_add_widgets.sql": normalFile("CREATE TABLE widgets (id TEXT);\nCREATE INDEX idx_widgets_id ON widgets (id);", "DROP TABLE widgets;"),
			"00182_idx.sql":         indexBuildFile("idx_widgets_id"),
		}, "already exists"},
		{"dropping an index nothing created", map[string]string{"00181_drop.sql": indexDropFile("idx_invoices_tennant")}, "would do nothing"},
		{"dropping a constraint's index", map[string]string{"00181_drop.sql": indexDropFile("invoices_pkey")}, "belongs to a constraint"},

		// Write-blocking statements (#987).
		{"a plain index on a live table", map[string]string{"00181_idx.sql": normalFile("CREATE INDEX idx_n ON invoices (number);", "DROP INDEX idx_n;")}, "blocks writes on a live table"},
		{"a blocking statement with a placeholder reason", map[string]string{"00181_idx.sql": normalFile(
			"-- velox:blocking-ddl-ok: small\nCREATE INDEX idx_n ON invoices (number);", "DROP INDEX idx_n;")}, "blocks writes on a live table"},
		{"VALIDATE in the same file as its ADD", map[string]string{"00181_chk.sql": normalFile(
			"ALTER TABLE invoices ADD CONSTRAINT chk CHECK (number <> '') NOT VALID;\nALTER TABLE invoices VALIDATE CONSTRAINT chk;",
			"ALTER TABLE invoices DROP CONSTRAINT chk;")}, "validate in the next file"},

		// Added columns (#1231). TestAddColumnRules has the other forms.
		{"a NOT NULL column with a constant default", map[string]string{"00181_x.sql": normalFile(
			"ALTER TABLE invoices ADD COLUMN paid BOOLEAN NOT NULL DEFAULT false;", "ALTER TABLE invoices DROP COLUMN paid;")}, noFinding},
		{"a generated value set as the default after the column is added", map[string]string{"00181_x.sql": normalFile(
			"ALTER TABLE invoices ADD COLUMN ref UUID;\nALTER TABLE invoices ALTER COLUMN ref SET DEFAULT gen_random_uuid();",
			"ALTER TABLE invoices DROP COLUMN ref;")}, noFinding},
		{"a NOT NULL column with no default on a table the file creates", map[string]string{"00181_x.sql": normalFile(
			"CREATE TABLE widgets (id TEXT);\nALTER TABLE widgets ADD COLUMN name TEXT NOT NULL;", "DROP TABLE widgets;")}, noFinding},
		{"a NOT NULL column with no default", map[string]string{"00181_x.sql": normalFile(
			"ALTER TABLE invoices ADD COLUMN memo TEXT NOT NULL;", "ALTER TABLE invoices DROP COLUMN memo;")}, "NOT NULL with no DEFAULT"},
		{"a NOT NULL column with no default and the opt-out line", map[string]string{"00181_x.sql": normalFile(
			"-- velox:blocking-ddl-ok: invoices is small on every install\nALTER TABLE invoices ADD COLUMN memo TEXT NOT NULL;",
			"ALTER TABLE invoices DROP COLUMN memo;")}, "NOT NULL with no DEFAULT"},
		{"a volatile default", map[string]string{"00181_x.sql": normalFile(
			"ALTER TABLE invoices ADD COLUMN ref UUID DEFAULT gen_random_uuid();", "ALTER TABLE invoices DROP COLUMN ref;")}, "to fill every existing row"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for name, body := range tc.files {
				fsys[name] = &fstest.MapFile{Data: []byte(body)}
			}
			got := lintMigrationDir(fsys, fixtureBaseline)
			if tc.want == noFinding {
				if len(got) != 0 {
					t.Fatalf("want no finding, got:\n%s", strings.Join(got, "\n"))
				}
				return
			}
			found := false
			for _, f := range got {
				found = found || strings.Contains(f, tc.want)
				if !strings.HasPrefix(f, "sql/") && !strings.HasPrefix(f, "goose cannot collect sql/") {
					t.Errorf("a finding must name its file: %q", f)
				}
			}
			if !found {
				t.Fatalf("want a finding containing %q, got %d:\n%s", tc.want, len(got), strings.Join(got, "\n"))
			}
		})
	}
}

// The real baseline's index names, read the way the index-name rule reads
// them. A pattern that missed pg_dump's form would leave the rule checking
// against nothing. Recount with
//
//	grep -cE '^CREATE (UNIQUE )?INDEX' baseline/00180_baseline.sql
//	grep -cE 'CONSTRAINT \S+ (PRIMARY KEY|UNIQUE|EXCLUDE)' baseline/00180_baseline.sql
func TestBaselineIndexNames(t *testing.T) {
	known := indexSet{}
	known.addFrom([]byte(baselineSQL))
	count := map[string]int{}
	for _, kind := range known {
		count[kind]++
	}
	if count[kindIndex] != 111 || count[kindConstraint] != 78 {
		t.Fatalf("baseline: %d plain indexes and %d constraint indexes, want 111 and 78", count[kindIndex], count[kindConstraint])
	}
	for name, kind := range map[string]string{
		"api_keys_pkey":                       kindConstraint,
		"billing_intervals_no_overlap":        kindConstraint,
		"idx_invoices_provider_sync_inflight": kindIndex,
	} {
		if known[name] != kind {
			t.Errorf("baseline: %s is %q, want %q", name, known[name], kind)
		}
	}
}

func TestBlockingDDLFindings(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"plain index on existing table", "CREATE INDEX idx_a ON invoices (tenant_id);", 1},
		{"unique index, quoted schema-qualified table", `CREATE UNIQUE INDEX idx_a ON public."invoices"(tenant_id);`, 1},
		{"concurrent index", "-- +goose NO TRANSACTION\nCREATE INDEX CONCURRENTLY idx_a ON invoices (tenant_id);", 0},
		{"index on a table created in the same file", "CREATE TABLE widgets (id TEXT);\nCREATE INDEX idx_w ON widgets (id);", 0},
		{"index on a table created IF NOT EXISTS", "CREATE TABLE IF NOT EXISTS widgets(id TEXT);\ncreate index idx_w on widgets(id);", 0},
		{"foreign key without NOT VALID", "ALTER TABLE invoices ADD CONSTRAINT fk_c FOREIGN KEY (customer_id) REFERENCES customers(id);", 1},
		{"foreign key NOT VALID", "ALTER TABLE invoices ADD CONSTRAINT fk_c FOREIGN KEY (customer_id) REFERENCES customers(id) NOT VALID;", 0},
		{"check without NOT VALID, multi-line", "ALTER TABLE invoices\n  ADD CONSTRAINT chk\n  CHECK (total >= 0);", 1},
		{"check NOT VALID, then VALIDATE in the same file", "ALTER TABLE invoices ADD CONSTRAINT chk CHECK (total >= 0) NOT VALID;\nALTER TABLE invoices VALIDATE CONSTRAINT chk;", 1},
		{"check NOT VALID and VALIDATE in one statement", "ALTER TABLE invoices ADD CONSTRAINT chk CHECK (total >= 0) NOT VALID, VALIDATE CONSTRAINT chk;", 1},
		{"VALIDATE alone, as the next file does", "ALTER TABLE invoices VALIDATE CONSTRAINT chk;", 0},
		{"two VALIDATEs", "ALTER TABLE invoices VALIDATE CONSTRAINT chk;\nALTER TABLE invoices VALIDATE CONSTRAINT fk_c;", 0},
		{"VALIDATE after a change to another table", "ALTER TABLE customers ADD COLUMN note TEXT;\nALTER TABLE invoices VALIDATE CONSTRAINT chk;", 0},
		{"unique constraint", "ALTER TABLE invoices ADD CONSTRAINT uq UNIQUE (tenant_id, number);", 1},
		{"unique constraint using a prebuilt index", "ALTER TABLE invoices ADD CONSTRAINT uq UNIQUE USING INDEX idx_uq;", 0},
		{"set not null", "ALTER TABLE invoices ALTER COLUMN number SET NOT NULL;", 1},
		{"add nullable column", "ALTER TABLE invoices ADD COLUMN note TEXT;", 0},
		{"add column with constant default", "ALTER TABLE invoices ADD COLUMN seeded BOOLEAN NOT NULL DEFAULT false;", 0},
		{"constraint on a table created in the same file", "CREATE TABLE widgets (id TEXT);\nALTER TABLE widgets ADD CONSTRAINT chk CHECK (id <> '');", 0},
		{"index text inside a function body is not a statement", "CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $$ CREATE INDEX x ON invoices (id) $$;", 0},
		{"commented-out index", "-- CREATE INDEX idx_a ON invoices (tenant_id);\nSELECT 1;", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := blockingDDLFindings([]byte(tc.sql))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d findings %q, want %d", len(got), got, tc.want)
			}
		})
	}
}

// The issue's own example: 0167 built an index on invoices with plain
// CREATE INDEX. Had the lint existed then, it would have stopped it.
func TestBlockingDDLFindings_Flags0167(t *testing.T) {
	body := []byte(`-- +goose Up
ALTER TABLE invoices
  ADD COLUMN provider_payment_status TEXT,
  ADD COLUMN provider_synced_at      TIMESTAMPTZ;
CREATE INDEX idx_invoices_provider_sync_inflight
  ON invoices (provider_synced_at NULLS FIRST, updated_at)
  WHERE payment_status IN ('processing', 'unknown');
`)
	got, err := blockingDDLFindings(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "CREATE INDEX on invoices") {
		t.Fatalf("0167: want one CREATE INDEX finding on invoices, got %q", got)
	}
}

// addColumnCases are ALTER TABLE statements on a live invoices table, and
// what each does to a table that has rows: refuses the new column
// (required), or rewrites the table (rewrite). TestAddColumnRules checks
// that the lint says so, and TestAddColumnRulesMatchPostgres checks the
// same answers against Postgres.
var addColumnCases = []struct {
	name              string
	sql               string
	required, rewrite bool
}{
	{"nullable", "ALTER TABLE invoices ADD COLUMN c TEXT;", false, false},
	{"NOT NULL with a constant default", "ALTER TABLE invoices ADD COLUMN c TEXT NOT NULL DEFAULT '';", false, false},
	{"the default before NOT NULL", "ALTER TABLE invoices ADD COLUMN c INT DEFAULT 0 NOT NULL;", false, false},
	{"#966's two columns", "ALTER TABLE invoices\n    ADD COLUMN decline_code TEXT NOT NULL DEFAULT '',\n    ADD COLUMN stripe_payment_method_id TEXT NOT NULL DEFAULT '';", false, false},
	{"NOT NULL with no default", "ALTER TABLE invoices ADD COLUMN c TEXT NOT NULL;", true, false},
	{"without the COLUMN keyword", "alter table invoices add c text not null;", true, false},
	{"IF NOT EXISTS", "ALTER TABLE invoices ADD COLUMN IF NOT EXISTS c TEXT NOT NULL;", true, false},
	{"quoted, schema-qualified", `ALTER TABLE public."invoices" ADD COLUMN "Memo" TEXT NOT NULL;`, true, false},
	{"the second of two columns", "ALTER TABLE invoices ADD COLUMN a TEXT NOT NULL DEFAULT 'x, y',\n  ADD COLUMN b NUMERIC(10,2) NOT NULL;", true, false},
	{"DEFAULT NULL", "ALTER TABLE invoices ADD COLUMN c TEXT NOT NULL DEFAULT NULL;", true, false},
	{"NOT NULL inside a literal", "ALTER TABLE invoices ADD COLUMN c TEXT DEFAULT 'NOT NULL';", false, false},
	{"NOT NULL inside parentheses", "ALTER TABLE invoices ADD COLUMN c BOOLEAN DEFAULT (NULL IS NOT NULL);", false, false},
	{"now()", "ALTER TABLE invoices ADD COLUMN c TIMESTAMPTZ NOT NULL DEFAULT now();", false, false},
	{"pg_catalog.now()", "ALTER TABLE invoices ADD COLUMN c TIMESTAMPTZ NOT NULL DEFAULT pg_catalog.now();", false, false},
	{"CURRENT_TIMESTAMP", "ALTER TABLE invoices ADD COLUMN c TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;", false, false},
	{"now() plus an interval", "ALTER TABLE invoices ADD COLUMN c TIMESTAMPTZ NOT NULL DEFAULT (now() + '30 days'::interval);", false, false},
	{"a cast with a type modifier", "ALTER TABLE invoices ADD COLUMN c NUMERIC(10,2) NOT NULL DEFAULT '0'::numeric(10,2);", false, false},
	{"CAST to character varying(n)", "ALTER TABLE invoices ADD COLUMN c VARCHAR(10) NOT NULL DEFAULT CAST('x' AS character varying(10));", false, false},
	{"encode() of a constant", "ALTER TABLE invoices ADD COLUMN c TEXT NOT NULL DEFAULT encode('abc'::bytea, 'hex');", false, false},
	{"a function in a CHECK, not in the default", "ALTER TABLE invoices ADD COLUMN c TEXT NOT NULL DEFAULT '' CHECK (char_length(c) <= 64);", false, false},
	{"a function name inside a literal", "ALTER TABLE invoices ADD COLUMN c TEXT DEFAULT 'random()';", false, false},
	{"gen_random_uuid()", "ALTER TABLE invoices ADD COLUMN c UUID DEFAULT gen_random_uuid();", false, true},
	{"the baseline's ID default", "ALTER TABLE invoices ADD COLUMN c TEXT NOT NULL DEFAULT ('vlx_x_'::text || encode(public.gen_random_bytes(12), 'hex'::text));", false, true},
	{"clock_timestamp()", "ALTER TABLE invoices ADD COLUMN c TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp();", false, true},
	{"random() inside another call", "ALTER TABLE invoices ADD COLUMN c DOUBLE PRECISION DEFAULT floor(random() * 10);", false, true},
	{"nextval()", "ALTER TABLE invoices ADD COLUMN c BIGINT DEFAULT nextval('invoice_seq');", false, true},
	{"a function the check does not know", "ALTER TABLE invoices ADD COLUMN c TEXT DEFAULT velox_new_id();", false, true},
	{"BIGSERIAL", "ALTER TABLE invoices ADD COLUMN c BIGSERIAL;", false, true},
	{"an identity column", "ALTER TABLE invoices ADD COLUMN c BIGINT GENERATED ALWAYS AS IDENTITY;", false, true},
	{"an identity column BY DEFAULT", "ALTER TABLE invoices ADD COLUMN c BIGINT NOT NULL GENERATED BY DEFAULT AS IDENTITY;", false, true},
	{"a stored generated column", "ALTER TABLE invoices ADD COLUMN c BIGINT GENERATED ALWAYS AS (amount_due * 2) STORED;", false, true},
	{"SET DEFAULT on an existing column", "ALTER TABLE invoices ALTER COLUMN number SET DEFAULT gen_random_uuid()::text;", false, false},
	{"a constraint is not a column", "ALTER TABLE invoices ADD CONSTRAINT chk CHECK (number IS NOT NULL) NOT VALID;", false, false},
	{"on a table the file creates", "CREATE TABLE widgets (id TEXT);\nALTER TABLE widgets ADD COLUMN ref UUID NOT NULL DEFAULT gen_random_uuid(),\n  ADD COLUMN name TEXT NOT NULL;", false, false},
}

// addColumnVerdict runs the lint on one Up section and reads its findings.
func addColumnVerdict(t *testing.T, up string) (required, rewrite bool) {
	t.Helper()
	for _, f := range lintMigrationFile([]byte(normalFile(up, "SELECT 1;")), false, indexSet{}) {
		r, w := strings.Contains(f, "NOT NULL with no DEFAULT"), strings.Contains(f, "to fill every existing row")
		if !r && !w {
			t.Errorf("unexpected finding: %s", f)
		}
		required, rewrite = required || r, rewrite || w
	}
	return required, rewrite
}

func TestAddColumnRules(t *testing.T) {
	for _, tc := range addColumnCases {
		t.Run(tc.name, func(t *testing.T) {
			required, rewrite := addColumnVerdict(t, tc.sql)
			if required != tc.required || rewrite != tc.rewrite {
				t.Fatalf("lint: required=%v rewrite=%v, want required=%v rewrite=%v", required, rewrite, tc.required, tc.rewrite)
			}
		})
	}
}

// TestAddColumnRulesMatchPostgres runs each case on a table with a row and
// checks the lint's answers against what Postgres does: a refused column
// fails with not_null_violation, and a rewrite gives the table a new file.
func TestAddColumnRulesMatchPostgres(t *testing.T) {
	_, db := scratchDB(t, "velox_migrate_add_column")
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE EXTENSION pgcrypto;
		CREATE SEQUENCE invoice_seq;
		CREATE FUNCTION velox_new_id() RETURNS text LANGUAGE sql AS $$ SELECT md5(random()::text) $$;
		CREATE TABLE invoices (id TEXT PRIMARY KEY, number TEXT, amount_due BIGINT NOT NULL DEFAULT 0);
		INSERT INTO invoices (id, number) VALUES ('inv_1', 'INV-1');`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range addColumnCases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			var before, after int64
			if err := tx.QueryRowContext(ctx, `SELECT pg_relation_filenode('invoices')`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			_, err = tx.ExecContext(ctx, tc.sql)
			var pgErr *pgconn.PgError
			required := errors.As(err, &pgErr) && pgErr.Code == "23502"
			if err != nil && !required {
				t.Fatalf("postgres: %v", err)
			}
			rewrite := false
			if err == nil {
				if err := tx.QueryRowContext(ctx, `SELECT pg_relation_filenode('invoices')`).Scan(&after); err != nil {
					t.Fatal(err)
				}
				rewrite = after != before
			}
			if required != tc.required || rewrite != tc.rewrite {
				t.Fatalf("postgres: required=%v rewrite=%v, want required=%v rewrite=%v", required, rewrite, tc.required, tc.rewrite)
			}
		})
	}
}

func TestBlockingDDLOptOutReason(t *testing.T) {
	body := []byte("-- 0190: something\n-- velox:blocking-ddl-ok: fx_rates has under 200 rows on every install\nCREATE INDEX i ON fx_rates (code);\n")
	if got := blockingDDLOptOutReason(body); got != "fx_rates has under 200 rows on every install" {
		t.Fatalf("reason = %q", got)
	}
	if got := blockingDDLOptOutReason([]byte("CREATE INDEX i ON t (c);")); got != "" {
		t.Fatalf("no marker: reason = %q", got)
	}
}

func TestTxControlFindings(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"plain statements", "-- +goose Up\nALTER TABLE t ADD COLUMN c TEXT;\n-- +goose Down\nALTER TABLE t DROP COLUMN c;", 0},
		{"wrapped in BEGIN and COMMIT", "-- +goose Up\nBEGIN;\nALTER TABLE t ADD COLUMN c TEXT;\nCOMMIT;", 2},
		{"a savepoint", "SAVEPOINT s;\nSELECT 1;\nRELEASE s;", 2},
		{"BEGIN inside a function body is not a statement", "CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$ BEGIN PERFORM 1; END $$;", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := txControlFindings([]byte(tc.sql))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d findings %q, want %d", len(got), got, tc.want)
			}
		})
	}
}
