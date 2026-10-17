package migrate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The migration lint (#987). A migration that builds an index or adds a
// constraint the plain way holds a lock that blocks every write to the
// table until it finishes: on a large table, that is an outage of the
// table for the whole build. The write-safe forms are
//
//   - CREATE INDEX CONCURRENTLY, in a `-- velox:no-transaction` file;
//   - ADD CONSTRAINT ... NOT VALID, then VALIDATE CONSTRAINT in a later
//     statement (validation does not block writes);
//   - ADD CONSTRAINT ... UNIQUE / PRIMARY KEY USING INDEX, on an index
//     built CONCURRENTLY first;
//   - SET NOT NULL after a validated CHECK (col IS NOT NULL) constraint,
//     which lets Postgres skip the table scan.
//
// Statements on a table the same file creates are exempt: the table is
// empty and nobody else can see it yet.
//
// A migration that must use a blocking form anyway (a table known to be
// small, say) says so with a line
//
//	-- velox:blocking-ddl-ok: <why this is safe>
//
// The check covers migrations from firstLintedVersion on. The ones before
// it shipped before the lint and are already applied on every install.

// firstLintedVersion is the first migration the lint applies to.
const firstLintedVersion = 181

const blockingDDLOptOut = "-- velox:blocking-ddl-ok:"

// minOptOutReason keeps the reason a sentence, not a placeholder.
const minOptOutReason = 15

var (
	ddlLineComment   = regexp.MustCompile(`--[^\n]*`)
	ddlSpace         = regexp.MustCompile(`\s+`)
	ddlCreateTable   = regexp.MustCompile(`^CREATE (?:UNLOGGED )?TABLE (?:IF NOT EXISTS )?([^\s(]+)`)
	ddlCreateIndex   = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (CONCURRENTLY )?.*? ON (?:ONLY )?([^\s(]+)`)
	ddlAlterTable    = regexp.MustCompile(`^ALTER TABLE (?:IF EXISTS )?(?:ONLY )?([^\s(]+) (.*)$`)
	ddlAddCheckOrFK  = regexp.MustCompile(`\bADD (?:CONSTRAINT \S+ )?(?:FOREIGN KEY|CHECK)\b`)
	ddlAddUniqueOrPK = regexp.MustCompile(`\bADD (?:CONSTRAINT \S+ )?(?:UNIQUE|PRIMARY KEY)\b`)
	ddlSetNotNull    = regexp.MustCompile(`\bSET NOT NULL\b`)
)

// normalizeDDL strips line comments, collapses whitespace and upper-cases
// a statement, so the patterns above can stay simple.
func normalizeDDL(stmt string) string {
	s := ddlLineComment.ReplaceAllString(stmt, " ")
	return strings.ToUpper(strings.TrimSpace(ddlSpace.ReplaceAllString(s, " ")))
}

func ddlTableName(raw string) string {
	name := strings.ReplaceAll(raw, `"`, "")
	return strings.TrimPrefix(name, "PUBLIC.")
}

// blockingDDLFindings lists the write-blocking statements in a migration
// body, each as a short explanation.
func blockingDDLFindings(body []byte) ([]string, error) {
	stmts, err := splitSQLStatements(body)
	if err != nil {
		return nil, err
	}
	created := map[string]bool{}
	var out []string
	for _, raw := range stmts {
		s := normalizeDDL(raw)
		if m := ddlCreateTable.FindStringSubmatch(s); m != nil {
			created[ddlTableName(m[1])] = true
			continue
		}
		if m := ddlCreateIndex.FindStringSubmatch(s); m != nil {
			table := ddlTableName(m[2])
			if m[1] == "" && !created[table] {
				out = append(out, fmt.Sprintf("CREATE INDEX on %s without CONCURRENTLY blocks writes to %s until the index is built", strings.ToLower(table), strings.ToLower(table)))
			}
			continue
		}
		m := ddlAlterTable.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		table, rest := ddlTableName(m[1]), m[2]
		if created[table] {
			continue
		}
		lt := strings.ToLower(table)
		if ddlAddCheckOrFK.MatchString(rest) && !strings.Contains(rest, "NOT VALID") {
			out = append(out, fmt.Sprintf("ADD CONSTRAINT (CHECK / FOREIGN KEY) on %s without NOT VALID blocks writes to %s while every row is checked", lt, lt))
		}
		if ddlAddUniqueOrPK.MatchString(rest) && !strings.Contains(rest, "USING INDEX") {
			out = append(out, fmt.Sprintf("ADD UNIQUE / PRIMARY KEY on %s without USING INDEX blocks writes to %s while its index is built", lt, lt))
		}
		if ddlSetNotNull.MatchString(rest) {
			out = append(out, fmt.Sprintf("SET NOT NULL on %s blocks writes to %s while every row is checked, unless a validated CHECK (col IS NOT NULL) exists", lt, lt))
		}
	}
	return out, nil
}

// blockingDDLOptOutReason returns the reason written after the opt-out
// marker, or "" when the file has none.
func blockingDDLOptOutReason(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, blockingDDLOptOut) {
			return strings.TrimSpace(strings.TrimPrefix(line, blockingDDLOptOut))
		}
	}
	return ""
}

func TestBlockingDDLFindings(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{"plain index on existing table", "CREATE INDEX idx_a ON invoices (tenant_id);", 1},
		{"unique index, quoted schema-qualified table", `CREATE UNIQUE INDEX idx_a ON public."invoices"(tenant_id);`, 1},
		{"concurrent index", "-- velox:no-transaction\nCREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON invoices (tenant_id);", 0},
		{"index on a table created in the same file", "CREATE TABLE widgets (id TEXT);\nCREATE INDEX idx_w ON widgets (id);", 0},
		{"index on a table created IF NOT EXISTS", "CREATE TABLE IF NOT EXISTS widgets(id TEXT);\ncreate index idx_w on widgets(id);", 0},
		{"foreign key without NOT VALID", "ALTER TABLE invoices ADD CONSTRAINT fk_c FOREIGN KEY (customer_id) REFERENCES customers(id);", 1},
		{"foreign key NOT VALID", "ALTER TABLE invoices ADD CONSTRAINT fk_c FOREIGN KEY (customer_id) REFERENCES customers(id) NOT VALID;", 0},
		{"check without NOT VALID, multi-line", "ALTER TABLE invoices\n  ADD CONSTRAINT chk\n  CHECK (total >= 0);", 1},
		{"check NOT VALID then VALIDATE", "ALTER TABLE invoices ADD CONSTRAINT chk CHECK (total >= 0) NOT VALID;\nALTER TABLE invoices VALIDATE CONSTRAINT chk;", 0},
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

// The issue's own example: 0167 builds an index on invoices with plain
// CREATE INDEX. Had the lint existed then, it would have stopped it.
func TestBlockingDDLFindings_Flags0167(t *testing.T) {
	body, err := readMigrationBody(167, "up")
	if err != nil {
		t.Fatal(err)
	}
	got, err := blockingDDLFindings(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "CREATE INDEX on invoices") {
		t.Fatalf("0167: want one CREATE INDEX finding on invoices, got %q", got)
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

// TestNewMigrationsAvoidBlockingDDL is the CI gate: every migration from
// firstLintedVersion on either uses the write-safe forms or carries the
// opt-out marker with a reason.
func TestNewMigrationsAvoidBlockingDDL(t *testing.T) {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		m := migrationFilenamePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v < firstLintedVersion {
			continue
		}
		body, err := sqlFS.ReadFile("sql/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := blockingDDLFindings(body)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if len(findings) == 0 {
			continue
		}
		reason := blockingDDLOptOutReason(body)
		if len(reason) >= minOptOutReason {
			continue
		}
		t.Errorf("%s blocks writes on a live table:\n  - %s\n"+
			"Use the write-safe form (see the comment at the top of internal/platform/migrate/blocking_ddl_test.go), "+
			"or, if blocking is safe here, add a line `%s <why>` (at least %d characters).",
			e.Name(), strings.Join(findings, "\n  - "), blockingDDLOptOut, minOptOutReason)
	}
}

// TestInTxMigrationsAreAllOrNothing keeps the claim newLockTimeoutError
// relies on when it clears a dirty mark: a migration that runs in a
// transaction is sent to Postgres as one simple query, which is one
// transaction unless the file controls transactions itself. A file may
// wrap its body in BEGIN ... COMMIT, but no statement may run after its
// COMMIT (that part would commit separately).
func TestInTxMigrationsAreAllOrNothing(t *testing.T) {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		t.Fatal(err)
	}
	txControl := regexp.MustCompile(`^(BEGIN|START TRANSACTION|COMMIT|END|ROLLBACK|SAVEPOINT|RELEASE)\b`)
	for _, e := range entries {
		if !migrationFilenamePattern.MatchString(e.Name()) {
			continue
		}
		body, err := sqlFS.ReadFile("sql/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if hasNoTxHeader(body) {
			continue
		}
		stmts, err := splitSQLStatements(body)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		for i, raw := range stmts {
			s := normalizeDDL(raw)
			if !txControl.MatchString(s) {
				continue
			}
			first, last := i == 0, i == len(stmts)-1
			if (first && strings.HasPrefix(s, "BEGIN")) || (last && strings.HasPrefix(s, "COMMIT")) {
				continue
			}
			t.Errorf("%s: statement %d (%q) controls the transaction mid-file; an in-transaction migration must be all-or-nothing "+
				"(only a leading BEGIN and a final COMMIT are allowed)", e.Name(), i+1, s)
		}
	}
}
