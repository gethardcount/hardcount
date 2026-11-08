package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/testutil"
)

// freePort returns a TCP port nothing listens on now.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }() // only the port number was needed
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// runServe runs `hardcount serve` against the test database in a child
// process, and returns its output and exit code. A serve that does not exit
// within the limit is stopped and reported as -1.
func runServe(t *testing.T, test string, env ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), append([]string{
		"HARDCOUNT_CLI_HELPER=serve",
		"APP_ENV=local",
		"PORT=" + freePort(t),
		"DATABASE_URL=" + testDBURL("TEST_ADMIN_DATABASE_URL", "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"),
		"APP_DATABASE_URL=" + testDBURL("TEST_DATABASE_URL", "postgres://hardcount_test_app:hardcount_test_app@localhost:5432/hardcount_test?sslmode=disable"),
	}, env...)...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

// A restored database carries the restore marker until `hardcount restore
// catch-up` has applied what Stripe did since the backup (ADR-135). serve
// refuses to start while it is there: it exits 2 before it listens or takes
// a lease, with or without RUN_MIGRATIONS_ON_BOOT. The marker is written here
// as scripts/restore.sh writes it, table included, because a backup made
// before the table existed restores without it.
func TestServeRefusesWhileRestoreMarkerIsSet(t *testing.T) {
	if os.Getenv("HARDCOUNT_CLI_HELPER") == "serve" {
		os.Args = []string{"hardcount", "serve"}
		main()
		return
	}
	testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	mustExec(t, admin, `CREATE TABLE IF NOT EXISTS public.restore_catchup (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    set_at timestamptz NOT NULL DEFAULT now(),
    set_by text NOT NULL,
    anchor timestamptz
)`)
	mustExec(t, admin, `INSERT INTO public.restore_catchup (set_by) VALUES ('restore.sh') ON CONFLICT (id) DO NOTHING`)

	for _, onBoot := range []string{"false", "true"} {
		out, code := runServe(t, "TestServeRefusesWhileRestoreMarkerIsSet", "RUN_MIGRATIONS_ON_BOOT="+onBoot)
		if code != 2 {
			t.Fatalf("RUN_MIGRATIONS_ON_BOOT=%s: serve exited %d, want 2 (refused); output:\n%s", onBoot, code, out)
		}
		if !strings.Contains(out, "hardcount restore catch-up") {
			t.Errorf("RUN_MIGRATIONS_ON_BOOT=%s: the refusal does not name the command to run; output:\n%s", onBoot, out)
		}
		if strings.Contains(out, `"msg":"listening"`) {
			t.Errorf("RUN_MIGRATIONS_ON_BOOT=%s: serve listened before refusing; output:\n%s", onBoot, out)
		}
	}
	var leases int
	if err := admin.QueryRow(`SELECT count(*) FROM leader_leases`).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 0 {
		t.Fatalf("%d leader lease rows: serve took a lease before refusing", leases)
	}
}
