package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/migrate"
)

// TestUpStopsOnLockTimeout walks issue #987 on real Postgres. Another
// session holds a lock on every table, the way a long transaction does,
// and the latest migration (which changes an existing table) needs one:
//
//   - with MIGRATION_LOCK_TIMEOUT=1s, Up returns a LockTimeoutError in
//     about a second instead of waiting, the migration left nothing behind,
//     and schema_migrations is clean at the previous version — no repair;
//   - control: with MIGRATION_LOCK_TIMEOUT=0 (no limit, Postgres' default
//     and Velox's behaviour before #987) Up is still waiting seconds later,
//     and finishes once the blocking session ends.
func TestUpStopsOnLockTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}

	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	}
	scratchDB := "velox_migrate_locktimeout"
	scratchURL := rewriteDBName(t, adminURL, scratchDB)
	createScratchDB(t, adminURL, scratchDB)
	t.Cleanup(func() { dropScratchDB(t, adminURL, scratchDB) })

	if err := migrate.Up(scratchURL); err != nil {
		t.Fatalf("Up: %v", err)
	}
	latest, err := migrate.EmbeddedLatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	prev, err := migrate.Rollback(scratchURL, 1)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	db, err := sql.Open("pgx", scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// The blocking session: a transaction holding ACCESS SHARE (what any
	// open SELECT holds) on every table but schema_migrations. Any ALTER
	// TABLE needs ACCESS EXCLUSIVE, which conflicts with it.
	ctx := context.Background()
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = blocker.Rollback()
		}
	}
	t.Cleanup(release)
	if _, err := blocker.ExecContext(ctx, `DO $$
DECLARE r record;
BEGIN
	FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename <> 'schema_migrations' LOOP
		EXECUTE format('LOCK TABLE %I IN ACCESS SHARE MODE', r.tablename);
	END LOOP;
END $$`); err != nil {
		t.Fatalf("take blocking locks: %v", err)
	}

	// Bounded: Up gives up in about the timeout.
	t.Setenv(migrate.LockTimeoutEnv, "1s")
	start := time.Now()
	err = migrate.Up(scratchURL)
	elapsed := time.Since(start)

	var lte *migrate.LockTimeoutError
	if !errors.As(err, &lte) {
		t.Fatalf("Up under a held lock: want LockTimeoutError, got %v (the latest migration must change an existing table for this test to bite)", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("Up took %s to give up with a 1s lock timeout", elapsed)
	}
	if lte.Version != latest || lte.Previous != prev || !lte.Restored || lte.Timeout != time.Second {
		t.Fatalf("LockTimeoutError = %+v, want version %d previous %d restored, timeout 1s", lte, latest, prev)
	}
	for _, want := range []string{"waited 1s for a table lock", "nothing needs repair", migrate.LockTimeoutEnv, migrate.LockTimeoutRunbookPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message lacks %q: %v", want, err)
		}
	}
	if v, dirty, err := migrate.Status(scratchURL); err != nil || v != prev || dirty {
		t.Fatalf("after the timeout: version %d dirty %v err %v, want %d clean", v, dirty, err, prev)
	}
	if err := migrate.CheckSchemaReady(db); err == nil || !strings.Contains(err.Error(), "schema behind code") {
		t.Fatalf("CheckSchemaReady: want 'schema behind code' (not dirty), got %v", err)
	}

	// Control: no limit means waiting for as long as the lock is held.
	t.Setenv(migrate.LockTimeoutEnv, "0")
	done := make(chan error, 1)
	go func() { done <- migrate.Up(scratchURL) }()
	select {
	case err := <-done:
		t.Fatalf("Up with no lock timeout returned while the lock was held: %v", err)
	case <-time.After(4 * time.Second):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Up after the blocker ended: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Up still waiting 60s after the blocker ended")
	}
	if v, dirty, err := migrate.Status(scratchURL); err != nil || v != latest || dirty {
		t.Fatalf("after the retry: version %d dirty %v err %v, want %d clean", v, dirty, err, latest)
	}
}
