package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/migrate"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// TestRepairAfterFailedMigration walks issue #967 end to end on real
// Postgres: the latest migration really fails midway (an event trigger
// rejects its DDL, so the real runner marks it dirty), then
//
//   - server start (CheckSchemaReady) and `velox migrate` (Up) refuse with
//     a DirtyError naming the version and the force commands;
//   - Force refuses while a migration lock is held, and for a version not
//     next to the dirty one;
//   - Force to the previous version clears the mark, Up re-applies the
//     migration, and the schema check passes;
//   - Force on the now-clean database refuses.
func TestRepairAfterFailedMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}

	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	}
	scratchDB := "velox_migrate_repair"
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

	// Make the next migration fail inside its body. An event trigger is
	// the generic way: every migration changes the schema, while the
	// runner's own bookkeeping (TRUNCATE + INSERT into schema_migrations)
	// is not DDL and still goes through, leaving the real dirty mark.
	mustExec(t, db, `CREATE FUNCTION velox_test_fail_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $$
BEGIN
	IF tg_tag <> 'TRUNCATE TABLE' THEN
		RAISE EXCEPTION 'simulated migration failure';
	END IF;
END $$`)
	mustExec(t, db, `CREATE EVENT TRIGGER velox_test_fail_ddl ON ddl_command_start EXECUTE FUNCTION velox_test_fail_ddl()`)

	if err := migrate.Up(scratchURL); err == nil || !strings.Contains(err.Error(), "simulated migration failure") {
		t.Fatalf("Up with failing DDL: want the simulated failure, got %v", err)
	}
	mustExec(t, db, `DROP EVENT TRIGGER velox_test_fail_ddl`)
	mustExec(t, db, `DROP FUNCTION velox_test_fail_ddl()`)

	v, dirty, err := migrate.Status(scratchURL)
	if err != nil || v != latest || !dirty {
		t.Fatalf("after failure: got version %d dirty %v err %v, want %d dirty", v, dirty, err, latest)
	}

	// Server start and `velox migrate` both refuse, naming the repair.
	wantRepair := fmt.Sprintf("`velox migrate force %d` and then `velox migrate`", prev)
	for name, err := range map[string]error{
		"CheckSchemaReady": migrate.CheckSchemaReady(db),
		"Up":               migrate.Up(scratchURL),
	} {
		var de *migrate.DirtyError
		if !errors.As(err, &de) || de.Version != latest {
			t.Fatalf("%s: want DirtyError at %d, got %v", name, latest, err)
		}
		if !strings.Contains(err.Error(), wantRepair) || !strings.Contains(err.Error(), migrate.RunbookPath) {
			t.Fatalf("%s: message lacks the repair steps: %v", name, err)
		}
	}

	// Refused while a migration run holds its lock.
	lockConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, postgres.LockKeyMigrateHybrid); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Force(scratchURL, prev); !errors.Is(err, migrate.ErrMigrationRunning) {
		t.Fatalf("Force under a held lock: want ErrMigrationRunning, got %v", err)
	}
	if _, err := lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, postgres.LockKeyMigrateHybrid); err != nil {
		t.Fatal(err)
	}
	_ = lockConn.Close()

	// Refused for a version that cannot match the schema.
	if err := migrate.Force(scratchURL, 1); err == nil || !strings.Contains(err.Error(), "refusing to force version 1") {
		t.Fatalf("Force far version: want refusal, got %v", err)
	}
	if v, dirty, _ := migrate.Status(scratchURL); v != latest || !dirty {
		t.Fatalf("a refused Force changed schema_migrations: version %d dirty %v", v, dirty)
	}

	// The repair: the failed transaction left none of its changes, so
	// force the previous version, then migrate.
	if err := migrate.Force(scratchURL, prev); err != nil {
		t.Fatalf("Force(%d): %v", prev, err)
	}
	if v, dirty, _ := migrate.Status(scratchURL); v != prev || dirty {
		t.Fatalf("after Force: version %d dirty %v, want %d clean", v, dirty, prev)
	}
	if err := migrate.Up(scratchURL); err != nil {
		t.Fatalf("Up after Force: %v", err)
	}
	if err := migrate.CheckSchemaReady(db); err != nil {
		t.Fatalf("CheckSchemaReady after repair: %v", err)
	}

	if err := migrate.Force(scratchURL, latest); !errors.Is(err, migrate.ErrNotDirty) {
		t.Fatalf("Force on a clean database: want ErrNotDirty, got %v", err)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
