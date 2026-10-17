package migrate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestLockTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultLockTimeout, false},
		{"30s", 30 * time.Second, false},
		{" 2m ", 2 * time.Minute, false},
		{"0", 0, false},
		{"0s", 0, false},
		{"abc", 0, true},
		{"5", 0, true}, // a bare number is ambiguous: seconds or ms?
		{"-1s", 0, true},
		{"500us", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(LockTimeoutEnv, tc.raw)
			got, err := lockTimeout()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), LockTimeoutEnv) {
				t.Fatalf("error does not name the setting: %v", err)
			}
		})
	}
}

// An invalid setting stops the migration before it connects, rather than
// silently running with no limit.
func TestOpenMigrationPool_RejectsBadLockTimeout(t *testing.T) {
	t.Setenv(LockTimeoutEnv, "soon")
	if _, err := openMigrationPool("postgres://u:p@127.0.0.1:1/x"); err == nil || !strings.Contains(err.Error(), LockTimeoutEnv) {
		t.Fatalf("want a %s error, got %v", LockTimeoutEnv, err)
	}
}

func TestIsLockTimeout(t *testing.T) {
	lockErr := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	other := &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                         {nil, false},
		"pg error":                    {lockErr, true},
		"wrapped pg error":            {fmt.Errorf("statement 1: %w", lockErr), true},
		"library database.Error":      {database.Error{OrigErr: lockErr, Err: "migration failed"}, true},
		"library *database.Error":     {&database.Error{OrigErr: lockErr, Err: "try lock failed"}, true},
		"wrapped library error":       {fmt.Errorf("step: %w", database.Error{OrigErr: lockErr}), true},
		"other pg error":              {other, false},
		"library error, other pg err": {database.Error{OrigErr: other}, false},
		"plain error":                 {errors.New("boom"), false},
	}
	for name, tc := range cases {
		if got := isLockTimeout(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestLockTimeoutError_Messages(t *testing.T) {
	latest, err := EmbeddedLatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := postApplyVersion(latest, "down")

	restored := (&LockTimeoutError{Version: latest, Previous: prev, Timeout: 5 * time.Second, Restored: true}).Error()
	for _, want := range []string{
		migrationFileName(latest),
		"waited 5s for a table lock",
		fmt.Sprintf("still at version %d", prev),
		"nothing needs repair",
		LockTimeoutEnv,
		LockTimeoutRunbookPath,
	} {
		if !strings.Contains(restored, want) {
			t.Errorf("restored message lacks %q: %s", want, restored)
		}
	}

	// Not restored: the dirty-repair steps follow.
	dirty := (&LockTimeoutError{Version: latest, Previous: prev, Timeout: time.Second}).Error()
	if !strings.Contains(dirty, "waited 1s for a table lock") || !strings.Contains(dirty, fmt.Sprintf("velox migrate force %d", prev)) || !strings.Contains(dirty, RunbookPath) {
		t.Errorf("dirty message lacks the repair steps: %s", dirty)
	}
	if strings.Contains(dirty, "nothing needs repair") {
		t.Errorf("dirty message claims nothing needs repair: %s", dirty)
	}
}

func TestExecNoTxStatement_ConcurrentlyPattern(t *testing.T) {
	for stmt, want := range map[string]bool{
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON t (c)": true,
		"drop index concurrently if exists i":                true,
		"ALTER TYPE invoice_status ADD VALUE 'held'":         false,
		"CREATE INDEX i ON concurrently_log (c)":             false,
	} {
		if got := concurrentlyPattern.MatchString(stmt); got != want {
			t.Errorf("%q: got %v, want %v", stmt, got, want)
		}
	}
}
