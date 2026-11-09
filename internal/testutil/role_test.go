package testutil

import (
	"database/sql"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
)

// Two test databases give two role names, and every name is one Postgres
// accepts unquoted and keeps whole.
func TestRoleName(t *testing.T) {
	plain := regexp.MustCompile(`^[a-z0-9_]{1,63}$`)
	long := strings.Repeat("a", 63)
	cases := []struct {
		name   string
		db1    string
		db2    string
		prefix string
	}{
		{"hardcount_test_l2 and l3", "hardcount_test_l2", "hardcount_test_l3", "hardcount_p1317"},
		{"names that differ past the cut", long, long[:62] + "b", "hardcount_p7_bypass"},
		{"names that differ only in characters a role name drops", "hardcount-test", "hardcount.test", "hardcount_p1317"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r1, r2 := roleName(tc.prefix, tc.db1), roleName(tc.prefix, tc.db2)
			if r1 == r2 {
				t.Fatalf("roleName(%q, %q) = roleName(%q, %q) = %q, want two names", tc.prefix, tc.db1, tc.prefix, tc.db2, r1)
			}
			for _, r := range []string{r1, r2} {
				if !plain.MatchString(r) {
					t.Errorf("role name %q is not 1 to 63 lowercase letters, digits and underscores", r)
				}
				if !strings.HasPrefix(r, tc.prefix+"_") {
					t.Errorf("role name %q does not start with %q", r, tc.prefix+"_")
				}
			}
		})
	}
	if got, want := roleName("hardcount_p1317", "hardcount_test_l2"), "hardcount_p1317_hardcount_test_l2_"; !strings.HasPrefix(got, want) {
		t.Errorf("roleName = %q, want it to start with %q so \\du shows the database", got, want)
	}
}

// CreateRole replaces a role an earlier run left with a grant, the role
// logs in with its password, and nothing of it is left after the test.
func TestCreateRole_LeavesNoRole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test (use -short=false)")
	}
	admin := AdminPool(t)
	ctx := t.Context()
	const prefix, password = "hardcount_testutil_role", "testutil-role-pw"
	var database string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	role := roleName(prefix, database)

	// What a run that stopped before its cleanup leaves: the role, and a
	// grant that makes a plain DROP ROLE fail.
	for _, q := range []string{
		`DROP TABLE IF EXISTS public._testutil_role_probe`,
		`CREATE TABLE public._testutil_role_probe (id int)`,
		`DROP ROLE IF EXISTS ` + role,
		`CREATE ROLE ` + role,
		`GRANT SELECT ON public._testutil_role_probe TO ` + role,
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP TABLE IF EXISTS public._testutil_role_probe`) })

	t.Run("create", func(t *testing.T) {
		if got := CreateRole(t, prefix, password, ""); got != role {
			t.Fatalf("CreateRole = %q, want %q", got, role)
		}
		if _, err := admin.ExecContext(ctx, `GRANT SELECT ON public._testutil_role_probe TO `+role); err != nil {
			t.Fatal(err)
		}
		u, err := neturl.Parse(envOr("TEST_ADMIN_DATABASE_URL", defaultAdminDBURL))
		if err != nil {
			t.Fatal(err)
		}
		u.User = neturl.UserPassword(role, password)
		pool, err := sql.Open("pgx", u.String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pool.Close() }()
		var n int
		if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM public._testutil_role_probe`).Scan(&n); err != nil {
			t.Fatalf("query as %s: %v", role, err)
		}
	})

	var left bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left {
		t.Fatalf("role %s is left after the test that created it", role)
	}
}
