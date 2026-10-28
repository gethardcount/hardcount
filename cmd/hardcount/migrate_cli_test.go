package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runHelper re-runs this test binary with HARDCOUNT_CLI_HELPER set, so a test
// can watch a subcommand exit with its code.
func runHelper(t *testing.T, test, helper string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), append(env, "HARDCOUNT_CLI_HELPER="+helper)...)
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

// `hardcount migrate force` is gone (B5): the one-release stub that pointed at
// the runbook is removed before the first tag, so it is an unknown
// subcommand like any other.
func TestMigrateForceIsUnknown(t *testing.T) {
	if os.Getenv("HARDCOUNT_CLI_HELPER") == "force" {
		os.Args = []string{"hardcount", "migrate", "force"}
		main()
		return
	}
	out, code := runHelper(t, "TestMigrateForceIsUnknown", "force")
	if code != 1 {
		t.Fatalf("exit code %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "unknown migrate subcommand: force") || strings.Contains(out, "removed in") {
		t.Errorf("want the plain unknown-subcommand answer; output:\n%s", out)
	}
}

// `hardcount migrate rollback` refuses outside APP_ENV=local, before it
// connects anywhere.
func TestMigrateRollbackRefusesOutsideLocal(t *testing.T) {
	if os.Getenv("HARDCOUNT_CLI_HELPER") == "rollback" {
		runMigrateRollback()
		return
	}
	for _, env := range []string{"production", " Staging "} {
		out, code := runHelper(t, "TestMigrateRollbackRefusesOutsideLocal", "rollback",
			"APP_ENV="+env, "DATABASE_URL=postgres://u:p@127.0.0.1:1/unreachable")
		if code != 2 || !strings.Contains(out, "runs only with APP_ENV=local") || !strings.Contains(out, "restore the backup") {
			t.Fatalf("APP_ENV=%q: exit %d, output:\n%s", env, code, out)
		}
	}
}
