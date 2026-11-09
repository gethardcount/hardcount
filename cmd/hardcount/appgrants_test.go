package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/gethardcount/hardcount/internal/testutil"
)

func testDBURL(env, fallback string) string {
	if u := strings.TrimSpace(os.Getenv(env)); u != "" {
		return u
	}
	return fallback
}

// A correctly set-up app role has every grant the boot check asks for, so a
// good database still boots (a false refusal would take the app down).
func TestMissingAppGrants_ProvisionedRoleHasNone(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	pool, err := sql.Open("pgx", testDBURL("TEST_DATABASE_URL",
		"postgres://hardcount_test_app:hardcount_test_app@localhost:5432/hardcount_test?sslmode=disable"))
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer func() { _ = pool.Close() }()
	role, missing, err := missingAppGrants(pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("provisioned role %s reported missing grants: %v", role, missing)
	}
}

// A role created without the default privileges (#1317) is told exactly what
// to grant, and a table it can fully use is not listed.
func TestMissingAppGrants_NamesTheMissingGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}
	adminURL := testDBURL("TEST_ADMIN_DATABASE_URL",
		"postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable")
	// AdminPool closes its pool after the test's cleanups, which still need it.
	admin := testutil.AdminPool(t)
	role := testutil.CreateRole(t, "hardcount_p1317", "p1317-test-pw", "")
	for _, q := range []string{
		`DROP TABLE IF EXISTS public.p1317_probe`,
		`CREATE TABLE public.p1317_probe (id int)`,
		`GRANT SELECT, UPDATE ON public.p1317_probe TO ` + role,
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP TABLE IF EXISTS public.p1317_probe`); err != nil {
			t.Errorf("drop probe table: %v", err)
		}
	})
	pool, err := sql.Open("pgx", rewriteUser(t, adminURL, role, "p1317-test-pw"))
	if err != nil {
		t.Fatalf("open role pool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	_, missing, err := missingAppGrants(pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	want := "GRANT INSERT, DELETE ON public.p1317_probe TO " + role
	if !slices.Contains(missing, want) {
		t.Fatalf("missing = %v, want it to contain %q", missing, want)
	}

	if _, err := admin.Exec(`GRANT INSERT, DELETE ON public.p1317_probe TO ` + role); err != nil {
		t.Fatalf("grant: %v", err)
	}
	_, missing, err = missingAppGrants(pool)
	if err != nil {
		t.Fatalf("check again: %v", err)
	}
	for _, m := range missing {
		if strings.Contains(m, "p1317_probe") {
			t.Fatalf("probe still listed after the grant: %q", m)
		}
	}
}

// The tables the baseline or a later migration grants narrowly are asked
// only for what it grants: asking the audit log for UPDATE would refuse
// every correctly set-up database.
func TestAppTableNeeds_MatchTheBaselineGrants(t *testing.T) {
	baseline, err := os.ReadFile("../../internal/platform/migrate/baseline/00180_baseline.sql")
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	sqls := string(baseline)
	migrations, err := filepath.Glob("../../internal/platform/migrate/sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(migrations))
	}
	sort.Strings(migrations)
	for _, f := range migrations {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		sqls += "\n" + string(b)
	}
	for table, privs := range appTableNeeds {
		line := "ON TABLE public." + table + " TO hardcount_app;"
		var grant string
		for _, l := range strings.Split(sqls, "\n") {
			if strings.HasPrefix(l, "GRANT ") && strings.HasSuffix(l, line) {
				grant = l
			}
		}
		if grant == "" {
			t.Fatalf("no baseline GRANT for %s", table)
		}
		for _, p := range privs {
			if !strings.Contains(grant, p) && !strings.Contains(grant, "GRANT ALL") {
				t.Errorf("%s: the check asks for %s, which the baseline does not grant (%s)", table, p, grant)
			}
		}
	}
}
