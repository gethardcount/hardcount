package migrate_test

import (
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/platform/migrate"
)

// TestReadPlanStates walks the states `velox migrate status` reports on
// real Postgres, each against what `velox migrate` then really does:
//
//   - a new database plans every shipped file, and reading the plan does
//     not create schema_migrations;
//   - after Up it is up to date;
//   - after a one-step rollback it is behind by exactly the latest file,
//     and Up brings it back to the latest version;
//   - a recorded version above the latest is ahead;
//   - a recorded version below the latest that this binary does not ship
//     is unknown, and Up does fail from it;
//   - a dirty mark plans nothing.
func TestReadPlanStates(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — requires postgres")
	}

	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://velox:velox@localhost:5432/velox_test?sslmode=disable"
	}
	scratchDB := "velox_migrate_plan"
	scratchURL := rewriteDBName(t, adminURL, scratchDB)
	createScratchDB(t, adminURL, scratchDB)
	t.Cleanup(func() { dropScratchDB(t, adminURL, scratchDB) })

	db, err := sql.Open("pgx", scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	latest, err := migrate.EmbeddedLatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	count, err := migrate.EmbeddedMigrationCount()
	if err != nil {
		t.Fatal(err)
	}
	readPlan := func(step string) migrate.Plan {
		t.Helper()
		p, err := migrate.ReadPlan(scratchURL)
		if err != nil {
			t.Fatalf("%s: ReadPlan: %v", step, err)
		}
		if p.Latest != latest {
			t.Fatalf("%s: Latest %d, want %d", step, p.Latest, latest)
		}
		return p
	}

	// New database: every shipped file, in order, and nothing written.
	fresh := readPlan("new database")
	if fresh.State != migrate.PlanBehind || fresh.Database != 0 || fresh.Dirty || len(fresh.Pending) != count {
		t.Fatalf("new database: got state %q database %d dirty %v with %d pending, want behind at 0 with %d",
			fresh.State, fresh.Database, fresh.Dirty, len(fresh.Pending), count)
	}
	latestFile := fresh.Pending[len(fresh.Pending)-1]
	if !strings.HasPrefix(fresh.Pending[0], "0001_") || !strings.HasPrefix(latestFile, fmt.Sprintf("%04d_", latest)) {
		t.Fatalf("new database: pending runs %s .. %s, want 0001_ .. %04d_", fresh.Pending[0], latestFile, latest)
	}
	var table sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('schema_migrations')::text`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table.Valid {
		t.Fatal("ReadPlan created schema_migrations on a new database; status must not write")
	}

	// Up to date.
	if err := migrate.Up(scratchURL); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if p := readPlan("after Up"); p.State != migrate.PlanUpToDate || p.Database != latest || len(p.Pending) != 0 {
		t.Fatalf("after Up: got %+v, want up to date at %d", p, latest)
	}

	// Behind by one: the plan names the latest file, and Up applies it.
	prev, err := migrate.Rollback(scratchURL, 1)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if p := readPlan("after rollback"); p.State != migrate.PlanBehind || p.Database != prev || !reflect.DeepEqual(p.Pending, []string{latestFile}) {
		t.Fatalf("after rollback: got %+v, want behind at %d with [%s]", p, prev, latestFile)
	}
	if err := migrate.Up(scratchURL); err != nil {
		t.Fatalf("Up after rollback: %v", err)
	}
	if v, dirty, _ := migrate.Status(scratchURL); v != latest || dirty {
		t.Fatalf("Up after rollback: version %d dirty %v, want %d clean", v, dirty, latest)
	}

	setVersion := func(v uint, dirty bool) {
		t.Helper()
		if _, err := db.Exec(`UPDATE schema_migrations SET version = $1, dirty = $2`, int64(v), dirty); err != nil {
			t.Fatal(err)
		}
	}

	// Ahead: a newer release recorded a later version.
	setVersion(latest+5, false)
	if p := readPlan("ahead"); p.State != migrate.PlanAhead || p.Database != latest+5 || len(p.Pending) != 0 {
		t.Fatalf("ahead: got %+v", p)
	}

	// Unknown: a version below the latest that no shipped file has. The
	// shipped set has gaps (0143 and 0166 were never used).
	shipped := map[uint]bool{}
	for _, name := range fresh.Pending {
		v, err := strconv.ParseUint(name[:strings.IndexByte(name, '_')], 10, 32)
		if err != nil {
			t.Fatalf("file %s: %v", name, err)
		}
		shipped[uint(v)] = true
	}
	var gap uint
	for v := uint(1); v < latest; v++ {
		if !shipped[v] {
			gap = v
			break
		}
	}
	if gap == 0 {
		t.Fatal("no unshipped version below the latest; the shipped set was expected to have gaps (0143, 0166)")
	}
	setVersion(gap, false)
	if p := readPlan("unknown"); p.State != migrate.PlanUnknown || p.Database != gap || len(p.Pending) != 0 {
		t.Fatalf("unknown: got %+v", p)
	}
	if err := migrate.Up(scratchURL); err == nil {
		t.Fatalf("Up from unshipped version %d: want an error, as the unknown state says", gap)
	}
	if v, dirty, _ := migrate.Status(scratchURL); v != gap || dirty {
		t.Fatalf("the failed Up changed schema_migrations: version %d dirty %v, want %d clean", v, dirty, gap)
	}

	// Dirty: nothing is planned until the repair.
	setVersion(latest, true)
	if p := readPlan("dirty"); p.State != migrate.PlanDirty || p.Database != latest || !p.Dirty || len(p.Pending) != 0 {
		t.Fatalf("dirty: got %+v", p)
	}
}
