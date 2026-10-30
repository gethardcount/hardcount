package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// Real-Postgres tests of the goose runner (ADR-125). Each names the
// mutation that must turn it red; the migrations CI job covers the rest
// (each role setup, the limited user's timeout, the restore drill,
// Postgres 14).

const widgetsFile = `-- +goose Up
CREATE TABLE widgets (id int NOT NULL);
-- +goose Down
DROP TABLE widgets;
`

// T4. A new database: the baseline, then a file with unqualified names, in
// the same Up. Mutation: set the baseline's empty search_path for the
// session (set_config(..., false)); it then leaks into the widgets file,
// which has no schema to create its table in.
func TestUpBaselineThenFileInOneUp(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_t4")
	widgets, version := pastShipped(t, "widgets.sql")
	fsys := files(map[string]string{widgets: widgetsFile})
	if err := up(context.Background(), dsn, fsys); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !tableExists(t, db, "public.widgets") || !tableExists(t, db, "public.tenants") {
		t.Fatalf("want public.tenants (baseline) and public.widgets (%s)", widgets)
	}
	for _, v := range []int64{180, version} {
		if n := gooseRows(t, db, v); n != 1 {
			t.Fatalf("version %d: %d rows, want 1", v, n)
		}
	}
	// 0174's seed: five rows, each token from clock_timestamp() at install.
	var leases int
	var fresh bool
	if err := db.QueryRow(`SELECT count(*), bool_and(abs(holder_token - (extract(epoch FROM now()) * 1000)::bigint) < 86400000) FROM leader_leases`).Scan(&leases, &fresh); err != nil || leases != 5 || !fresh {
		t.Fatalf("leader_leases: want 5 rows with tokens from install time, got %d rows, fresh=%v (%v)", leases, fresh, err)
	}
	// A second run finds nothing to do.
	if err := up(context.Background(), dsn, fsys); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	// The widgets version is past every file this binary ships, so to the
	// boot check the scratch database is ahead: a WARN, not an error.
	if err := CheckSchemaReady(db); err != nil {
		t.Fatalf("CheckSchemaReady: %v", err)
	}
}

// pastShipped names a test migration file one version past the highest
// migration this binary ships. A scratch database that has it reads as
// ahead to the boot check, so these tests keep passing as sql/ grows. With
// a fixed 00181, the second shipped file after the baseline made the
// scratch database read as behind ("1 migration to apply").
func pastShipped(t *testing.T, name string) (file string, version int64) {
	t.Helper()
	_, latest := shippedSet(t)
	return fmt.Sprintf("%05d_%s", latest+1, name), latest + 1
}

// A new database where the admin user owns a schema named after itself,
// so its default search_path ("$user", public) puts new objects there. The
// baseline names every object public.x; a later file's unqualified CREATE
// TABLE must land in public too, where hardcount_app looks. Mutation: drop
// migrationSession's SET search_path; widgets then lands in the user's
// own schema.
func TestUpCreatesInPublicWhateverTheSearchPath(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_search_path")
	mustExec(t, db, `CREATE SCHEMA AUTHORIZATION CURRENT_USER`)
	widgets, _ := pastShipped(t, "widgets.sql")
	fsys := files(map[string]string{widgets: widgetsFile})
	if err := up(context.Background(), dsn, fsys); err != nil {
		t.Fatalf("Up: %v", err)
	}
	var schemas sql.NullString
	if err := db.QueryRow(`SELECT string_agg(n.nspname, ',' ORDER BY n.nspname) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relname IN ('widgets', 'tenants')`).Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas.String != "public,public" {
		t.Fatalf("tenants (baseline) and widgets (%s) are in %q, want both in public", widgets, schemas.String)
	}
	// A second run, then the boot check on a pool with the user's own
	// search_path.
	if err := up(context.Background(), dsn, fsys); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if err := CheckSchemaReady(db); err != nil {
		t.Fatalf("CheckSchemaReady: %v", err)
	}
}

// T5. Copies start together on a new database, and two copies meet an
// index build that waits for an old snapshot. Mutation: a blocking
// pg_advisory_lock locker; the waiting copy's lock statement holds a
// snapshot the build waits for, and Postgres cancels one side (E3).
func TestUpConcurrentCopies(t *testing.T) {
	fastLockWait(t, 120)
	ctx := context.Background()

	t.Run("new database, three copies", func(t *testing.T) {
		dsn, db := scratchDB(t, "hardcount_migrate_t5a")
		fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
		errs := make([]error, 3)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) { defer wg.Done(); errs[i] = up(ctx, dsn, fsys) }(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("copy %d: %v", i, err)
			}
		}
		for _, v := range []int64{0, 180, 181} {
			if n := gooseRows(t, db, v); n != 1 {
				t.Fatalf("version %d: %d rows, want 1", v, n)
			}
		}
	})

	t.Run("index build waits for an old snapshot", func(t *testing.T) {
		dsn, db := scratchDB(t, "hardcount_migrate_t5b")
		fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
		if err := up(ctx, dsn, fsys); err != nil {
			t.Fatalf("Up: %v", err)
		}
		mustExec(t, db, `INSERT INTO widgets SELECT g FROM generate_series(1, 1000) g`)
		fsys["00182_widgets_id_index.sql"] = indexFile("idx_widgets_id", "CREATE UNIQUE INDEX CONCURRENTLY idx_widgets_id ON widgets (id);")

		holder, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(`SELECT 1`); err != nil { // takes the snapshot
			t.Fatal(err)
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) { defer wg.Done(); errs[i] = up(ctx, dsn, fsys) }(i)
		}
		time.Sleep(3 * time.Second) // the build is now waiting for the holder
		_ = holder.Commit()
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("copy %d: %v", i, err)
			}
		}
		if !indexValid(t, db, "idx_widgets_id") {
			t.Fatal("idx_widgets_id is missing or INVALID")
		}
		if n := gooseRows(t, db, 182); n != 1 {
			t.Fatalf("version 182: %d rows, want 1", n)
		}
	})
}

func indexFile(name, create string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("-- +goose NO TRANSACTION\n-- +goose Up\nSET lock_timeout = 0;\n" +
		"DROP INDEX CONCURRENTLY IF EXISTS " + name + ";\n" + create + "\nRESET lock_timeout;\n" +
		"-- +goose Down\nSET lock_timeout = 0;\nDROP INDEX CONCURRENTLY IF EXISTS " + name + ";\nRESET lock_timeout;\n")}
}

func indexValid(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var valid bool
	err := db.QueryRow(`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`, name).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return valid
}

// T6. A failed normal file leaves no table and no row, and passes once
// fixed (E5 as a regression test).
func TestUpFailedFileLeavesNothing(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_t6")
	ctx := context.Background()
	bad := files(map[string]string{"00181_broken.sql": "-- +goose Up\nCREATE TABLE half_done (id int);\nSELECT 1/0;\n-- +goose Down\nDROP TABLE half_done;\n"})
	err := up(ctx, dsn, bad)
	var fe *FileError
	if !errors.As(err, &fe) || fe.Version != 181 || fe.NoTx {
		t.Fatalf("want a FileError for 181, got %v", err)
	}
	for _, want := range []string{"00181_broken.sql", "division by zero", "22012", "none of it was applied", RunbookPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if tableExists(t, db, "public.half_done") || gooseRows(t, db, 181) != 0 {
		t.Fatal("the failed file left its table or its row")
	}
	if gooseRows(t, db, 180) != 1 {
		t.Fatal("the baseline before it should have committed")
	}
	fixed := files(map[string]string{"00181_broken.sql": "-- +goose Up\nCREATE TABLE half_done (id int);\n-- +goose Down\nDROP TABLE half_done;\n"})
	if err := up(ctx, dsn, fixed); err != nil {
		t.Fatalf("Up after the fix: %v", err)
	}
	if !tableExists(t, db, "public.half_done") || gooseRows(t, db, 181) != 1 {
		t.Fatal("the fixed file did not apply")
	}
}

// A baseline that fails partway leaves nothing: not the tables before the
// failing statement, and no row 180. The baseline is hand-edited source,
// and an edit that commits partway would leave tables goose has no record
// of, which every later command refuses. Mutation: a COMMIT after
// CREATE TABLE public.tenants in the baseline; tenants then stays.
func TestUpFailedBaselineLeavesNothing(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_baseline_fails")
	ctx := context.Background()
	// The baseline creates tenants long before webhook_events.
	mustExec(t, db, `CREATE TABLE public.webhook_events (id int)`)
	err := up(ctx, dsn, noFiles)
	var fe *FileError
	if !errors.As(err, &fe) || fe.Version != baselineVersion {
		t.Fatalf("want a FileError for 180, got %v", err)
	}
	if !strings.Contains(err.Error(), "none of the baseline was applied") {
		t.Errorf("message: %v", err)
	}
	if tableExists(t, db, "public.tenants") || gooseRows(t, db, baselineVersion) != 0 {
		t.Fatal("the failed baseline left its tables or row 180")
	}
	mustExec(t, db, `DROP TABLE public.webhook_events`)
	if err := up(ctx, dsn, noFiles); err != nil {
		t.Fatalf("Up on the emptied database: %v", err)
	}
	if !tableExists(t, db, "public.tenants") || gooseRows(t, db, baselineVersion) != 1 {
		t.Fatal("the baseline did not apply")
	}
}

// blockTable holds ACCESS SHARE on a table, the way an open SELECT does.
func blockTable(t *testing.T, db *sql.DB, table string) (release func()) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`LOCK TABLE ` + table + ` IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// upWithin runs up and fails the test if it has not returned in time, so a
// mutation that makes it wait forever fails instead of hanging.
func upWithin(t *testing.T, d time.Duration, dsn string, fsys fstest.MapFS, release func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- up(context.Background(), dsn, fsys) }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		release()
		t.Fatalf("Up still waiting after %s: lock_timeout is not in force", d)
		return nil
	}
}

// T7. A normal file stops at MIGRATION_LOCK_TIMEOUT, undone in full. The
// second case runs the baseline and the file in one Up. Mutation: turn the
// baseline's SET LOCAL lock_timeout into a plain SET; lock_timeout = 0 then
// stays on the session, and the file waits forever.
func TestUpStopsOnLockTimeout(t *testing.T) {
	t.Setenv(LockTimeoutEnv, "1s")

	t.Run("after earlier files", func(t *testing.T) {
		dsn, db := scratchDB(t, "hardcount_migrate_t7a")
		fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
		if err := up(context.Background(), dsn, fsys); err != nil {
			t.Fatalf("Up: %v", err)
		}
		release := blockTable(t, db, "widgets")
		fsys["00182_widgets_note.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE widgets ADD COLUMN note text;\n-- +goose Down\nALTER TABLE widgets DROP COLUMN note;\n")}
		start := time.Now()
		err := upWithin(t, 20*time.Second, dsn, fsys, release)
		var lte *LockTimeoutError
		if !errors.As(err, &lte) || lte.Version != 182 || lte.Previous != 181 || lte.Timeout != time.Second {
			t.Fatalf("want LockTimeoutError{182, previous 181, 1s}, got %v", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("took %s to give up with a 1s lock timeout", time.Since(start))
		}
		for _, want := range []string{"waited 1s for a table lock", "is still at version 181", "nothing needs repair", LockTimeoutEnv, LockTimeoutRunbookPath} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message lacks %q: %v", want, err)
			}
		}
		if gooseRows(t, db, 182) != 0 {
			t.Fatal("the stopped file has a version row")
		}
		release()
		if err := up(context.Background(), dsn, fsys); err != nil {
			t.Fatalf("Up after the lock is gone: %v", err)
		}
		if gooseRows(t, db, 182) != 1 {
			t.Fatal("182 did not apply on the rerun")
		}
	})

	t.Run("baseline and file in one Up", func(t *testing.T) {
		dsn, db := scratchDB(t, "hardcount_migrate_t7b")
		mustExec(t, db, `CREATE SCHEMA side`)
		mustExec(t, db, `CREATE TABLE side.t (id int)`)
		release := blockTable(t, db, "side.t")
		fsys := files(map[string]string{"00181_side.sql": "-- +goose Up\nALTER TABLE side.t ADD COLUMN c int;\n-- +goose Down\nALTER TABLE side.t DROP COLUMN c;\n"})
		err := upWithin(t, 20*time.Second, dsn, fsys, release)
		var lte *LockTimeoutError
		if !errors.As(err, &lte) || lte.Version != 181 || lte.Previous != 180 {
			t.Fatalf("want LockTimeoutError{181, previous 180}, got %v", err)
		}
		if gooseRows(t, db, 180) != 1 || gooseRows(t, db, 181) != 0 {
			t.Fatal("want the baseline applied and 181 not")
		}
	})
}

// T8. An index build fails on duplicate rows and leaves an INVALID index;
// once the rows are fixed, a rerun builds it valid. Mutations: a template
// without DROP ... IF EXISTS (the rerun fails with 42P07), or with IF NOT
// EXISTS (the rerun keeps the INVALID index).
//
// T9. Between the two, a new Up with a normal file still stops at
// MIGRATION_LOCK_TIMEOUT. Mutation: reuse the pool across Up calls; the
// failed build's session keeps lock_timeout = 0.
func TestUpIndexBuildFailure(t *testing.T) {
	t.Setenv(LockTimeoutEnv, "1s")
	dsn, db := scratchDB(t, "hardcount_migrate_t8")
	ctx := context.Background()
	base := map[string]string{"00181_widgets.sql": widgetsFile}
	if err := up(ctx, dsn, files(base)); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO widgets VALUES (1), (2), (2)`)

	build := files(base)
	build["00182_widgets_id_unique.sql"] = indexFile("idx_widgets_id_unique", "CREATE UNIQUE INDEX CONCURRENTLY idx_widgets_id_unique ON widgets (id);")
	err := up(ctx, dsn, build)
	var fe *FileError
	if !errors.As(err, &fe) || fe.Version != 182 || !fe.NoTx {
		t.Fatalf("want a no-transaction FileError for 182, got %v", err)
	}
	for _, want := range []string{"23505", "detail: Key (id)=(2) is duplicated.", "constraint: idx_widgets_id_unique", "INVALID", "Running the upgrade again is safe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	if indexValid(t, db, "idx_widgets_id_unique") || gooseRows(t, db, 182) != 0 {
		t.Fatal("want an INVALID (or no) index and no row for 182")
	}

	// T9: the next command starts on new sessions.
	release := blockTable(t, db, "widgets")
	other := files(base)
	other["00183_widgets_note.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE widgets ADD COLUMN note text;\n-- +goose Down\nALTER TABLE widgets DROP COLUMN note;\n")}
	var lte *LockTimeoutError
	if err := upWithin(t, 20*time.Second, dsn, other, release); !errors.As(err, &lte) {
		t.Fatalf("T9: want LockTimeoutError, got %v", err)
	}
	release()

	mustExec(t, db, `DELETE FROM widgets WHERE id = 2`)
	mustExec(t, db, `INSERT INTO widgets VALUES (2)`)
	if err := up(ctx, dsn, build); err != nil {
		t.Fatalf("rerun after fixing the rows: %v", err)
	}
	if !indexValid(t, db, "idx_widgets_id_unique") {
		t.Fatal("the rerun left the index INVALID")
	}
	if n := gooseRows(t, db, 182); n != 1 {
		t.Fatalf("182: %d rows, want 1", n)
	}
}

// T10. Another session holds goose's lock for the whole wait. Mutation:
// drop the migrationSession wrapper; goose's bare "failed to acquire lock" comes
// back instead.
func TestUpLockWait(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_t10")
	fastLockWait(t, 2)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock(4097083626)`); err != nil {
		t.Fatal(err)
	}
	err = up(context.Background(), dsn, noFiles)
	var lw *LockWaitError
	if !errors.As(err, &lw) || lw.Waited != 2*time.Second {
		t.Fatalf("want LockWaitError after 2s, got %v", err)
	}
	if !strings.Contains(err.Error(), "running now") || !strings.Contains(err.Error(), "4097083626") {
		t.Errorf("message: %v", err)
	}
	if gooseRows(t, db, 180) != 0 || tableExists(t, db, "public.tenants") {
		t.Fatal("a command that never got the lock changed the schema")
	}
}

// T11. The boot check. Mutation: treat PlanEmpty as ready; the server
// would then start on an empty database.
//
// CheckSchemaReady reads the set this binary ships: migration 180 and the
// files in sql/. The counts and the "ahead" version below follow that set,
// so adding a migration file does not change what the test proves.
func TestCheckSchemaReady(t *testing.T) {
	ctx := context.Background()
	shipped, latest := shippedSet(t)
	behind := countMigrations(shipped) + " to apply; run `hardcount migrate`"

	t.Run("empty", func(t *testing.T) {
		_, db := scratchDB(t, "hardcount_migrate_t11a")
		if err := CheckSchemaReady(db); err == nil || !strings.Contains(err.Error(), behind) {
			t.Fatalf("got %v, want %q", err, behind)
		}
	})
	t.Run("up to date, then ahead", func(t *testing.T) {
		dsn, db := scratchDB(t, "hardcount_migrate_t11c")
		if err := up(ctx, dsn, migrationFiles()); err != nil {
			t.Fatal(err)
		}
		if err := CheckSchemaReady(db); err != nil {
			t.Fatalf("up to date: %v", err)
		}
		recordGoose(t, db, latest+1)
		if err := CheckSchemaReady(db); err != nil {
			t.Fatalf("ahead: want a WARN and no error, got %v", err)
		}
	})
}

// shippedSet is the set CheckSchemaReady reads: how many migrations an
// empty database needs (180 plus the files in sql/), and the highest
// version shipped. goose lists sources without connecting, so the handle
// never dials.
func shippedSet(t *testing.T) (count int, latest int64) {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	files, err := embeddedFiles(db, migrationFiles())
	if err != nil {
		t.Fatal(err)
	}
	return len(files), files[len(files)-1].Version
}

// T12. Status is read-only: it creates no goose_db_version and waits for
// no lock, so it answers while a migration runs. Mutation: build it on
// goose's Provider.Status, which creates the table and takes the lock.
func TestReadPlanIsReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, dsn string, db *sql.DB)
		state PlanState
	}{
		{"empty", func(*testing.T, string, *sql.DB) {}, PlanEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, db := scratchDB(t, "hardcount_migrate_t12_"+strings.ReplaceAll(strings.ReplaceAll(tc.name, " ", "_"), ".", ""))
			tc.build(t, dsn, db)
			holder, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Close() }()
			if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock(4097083626)`); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			pool, err := openPool(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pool.Close() }()
			p, err := readPlan(ctx, pool, noFiles)
			if err != nil {
				t.Fatalf("readPlan: %v", err)
			}
			if p.State != tc.state {
				t.Fatalf("state %q, want %q", p.State, tc.state)
			}
			if !strings.Contains(p.Running, fmt.Sprintf("pid %d", pid)) {
				t.Fatalf("running now = %q, want the holder (pid %d)", p.Running, pid)
			}
			if tableExists(t, db, versionTable) {
				t.Fatal("status created goose_db_version")
			}
		})
	}
}

// T14. Round trip after 180, and 180 itself never rolls back. Mutation:
// an empty Down for 180.
func TestDownRoundTrip(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_t14")
	ctx := context.Background()
	fsys := files(map[string]string{"00181_widgets.sql": widgetsFile})
	fsys["00182_widgets_id.sql"] = indexFile("idx_widgets_id", "CREATE INDEX CONCURRENTLY idx_widgets_id ON widgets (id);")
	if err := up(ctx, dsn, fsys); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if v, err := down(ctx, dsn, fsys, baselineVersion); err != nil || v != baselineVersion {
		t.Fatalf("DownTo(180): version %d, %v", v, err)
	}
	if tableExists(t, db, "public.widgets") {
		t.Fatal("181's Down left widgets")
	}
	if err := up(ctx, dsn, fsys); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if !indexValid(t, db, "idx_widgets_id") {
		t.Fatal("index missing after the second Up")
	}
	for v := int64(182); v > baselineVersion; v-- {
		if got, err := down(ctx, dsn, fsys, -1); err != nil || got != v-1 {
			t.Fatalf("Down from %d: version %d, %v", v, got, err)
		}
	}
	got, err := down(ctx, dsn, fsys, -1)
	if !errors.Is(err, errBaselineDown) || got != baselineVersion {
		t.Fatalf("Down from 180: version %d, %v; want the baseline refusal", got, err)
	}
	if gooseRows(t, db, 180) != 1 || !tableExists(t, db, "public.tenants") {
		t.Fatal("the refused Down changed the database")
	}
}

// A database with tables but no goose record for 180: a Velox 0.8.x
// database, or a backup restored without its migration table. It has
// no upgrade path. Status says so, the boot check refuses, and Up refuses
// before it takes the lock or creates anything; goose's own table included.
// Mutation: drop the refusal from decide; status then says "empty", and
// Up runs the baseline, which fails and leaves goose_db_version behind.
func TestTablesWithoutRecordAreRefused(t *testing.T) {
	ctx := context.Background()
	for i, tc := range []struct {
		name  string
		build func(t *testing.T, db *sql.DB)
	}{
		{"Velox 0.8.x database", buildV08},
		{"restored without its migration table", func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `DROP TABLE public.goose_db_version`)
		}},
		{"goose table with only row 0, left by an earlier attempt", func(t *testing.T, db *sql.DB) {
			buildV08(t, db)
			recordGoose(t, db)
		}},
		// v0.8.x created its tables unqualified, so an install could keep
		// them in the admin user's own schema, which Postgres's default
		// search_path ("$user", public) lists before public. public is
		// then empty, and only the search_path finds the tables.
		{"Velox 0.8.x database outside public", func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `DROP TABLE public.goose_db_version`)
			mustExec(t, db, `DO $$ BEGIN EXECUTE format('ALTER SCHEMA public RENAME TO %I', current_user); END $$`)
			mustExec(t, db, `CREATE SCHEMA public`)
			for _, ext := range []string{"pgcrypto", "citext", "btree_gist"} {
				mustExec(t, db, `ALTER EXTENSION `+ext+` SET SCHEMA public`)
			}
			mustExec(t, db, `CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`)
			mustExec(t, db, `INSERT INTO schema_migrations VALUES (180, false)`)
			if tableExists(t, db, "public.tenants") || !tableExists(t, db, "tenants") {
				t.Fatal("setup: tenants should be outside public, on the search_path")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, db := scratchDB(t, "hardcount_migrate_unrecorded_"+string(rune('a'+i)))
			if err := up(ctx, dsn, migrationFiles()); err != nil {
				t.Fatalf("Up: %v", err)
			}
			tc.build(t, db)
			before := fingerprint(t, db)
			hadGooseTable := tableExists(t, db, versionTable)

			pool, err := openPool(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pool.Close() }()
			p, err := readPlan(ctx, pool, migrationFiles())
			if err != nil {
				t.Fatalf("readPlan: %v", err)
			}
			if p.State != PlanUnrecorded || len(p.Pending) != 0 {
				t.Errorf("status: state %q with %d pending, want %q and none", p.State, len(p.Pending), PlanUnrecorded)
			}
			out := FormatPlan(p, "v0.9.1")
			if !strings.Contains(out, "tables, no migration record") || strings.Contains(out, `run "hardcount migrate"`) {
				t.Errorf("status output:\n%s", out)
			}

			if ready := CheckSchemaReady(db); !errors.Is(ready, ErrNoMigrationRecord) {
				t.Errorf("CheckSchemaReady: %v", ready)
			}

			err = up(ctx, dsn, migrationFiles())
			var fe *FileError
			if !errors.Is(err, ErrNoMigrationRecord) || errors.As(err, &fe) {
				t.Errorf("Up: want a refusal before any migration runs, got %v", err)
			} else {
				for _, want := range []string{"has tables but no migration record", "Nothing was changed", "Upgrading from Velox 0.8.x is not supported", RunbookPath} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Up's message lacks %q: %v", want, err)
					}
				}
			}
			if _, err := down(ctx, dsn, migrationFiles(), -1); !errors.Is(err, ErrNoMigrationRecord) {
				t.Errorf("Down: want ErrNoMigrationRecord, got %v", err)
			}
			if !hadGooseTable && tableExists(t, db, versionTable) {
				t.Error("a refused command created goose_db_version")
			}
			if after := fingerprint(t, db); after != before {
				t.Errorf("a refused command changed the database:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// A database a build of Velox, Hardcount's old name, made: goose has 180
// and every later file, but public.tenants gives vlx_ ids. v0.9.0 installs
// fresh only, so status, the boot check, Up and Down all refuse it and
// change nothing. The setup changes only what a Velox baseline wrote
// differently that the check reads: the tenants id default.
// Mutation: drop the veloxSchema case from decide; status then says "up to
// date", the boot check passes and Up returns nil.
func TestVeloxDatabaseIsRefused(t *testing.T) {
	ctx := context.Background()
	dsn, db := scratchDB(t, "hardcount_migrate_velox")
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := CheckSchemaReady(db); err != nil {
		t.Fatalf("a Hardcount database must pass the boot check: %v", err)
	}
	mustExec(t, db, `ALTER TABLE public.tenants ALTER COLUMN id SET DEFAULT ('vlx_ten_' || encode(gen_random_bytes(12), 'hex'))`)
	before := fingerprint(t, db)

	pool, err := openPool(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	p, err := readPlan(ctx, pool, migrationFiles())
	if err != nil {
		t.Fatalf("readPlan: %v", err)
	}
	if p.State != PlanVelox || len(p.Pending) != 0 || len(p.Ahead) != 0 {
		t.Errorf("status: state %q with %d pending, want %q and none", p.State, len(p.Pending), PlanVelox)
	}
	out := FormatPlan(p, "v0.9.1")
	if !strings.Contains(out, "made by Velox, hardcount migrate will not run") || !strings.Contains(out, VeloxRunbookPath) {
		t.Errorf("status output:\n%s", out)
	}
	if ready := CheckSchemaReady(db); !errors.Is(ready, ErrVeloxDatabase) {
		t.Errorf("CheckSchemaReady: want ErrVeloxDatabase, got %v", ready)
	}
	err = up(ctx, dsn, migrationFiles())
	if !errors.Is(err, ErrVeloxDatabase) {
		t.Errorf("Up: want ErrVeloxDatabase, got %v", err)
	} else {
		for _, want := range []string{"made by Velox", "Nothing was changed", "install fresh", VeloxRunbookPath} {
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
				t.Errorf("Up's message lacks %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("Up's message is not one line: %q", err)
		}
	}
	if _, err := down(ctx, dsn, migrationFiles(), -1); !errors.Is(err, ErrVeloxDatabase) {
		t.Errorf("Down: want ErrVeloxDatabase, got %v", err)
	}
	if after := fingerprint(t, db); after != before {
		t.Errorf("a refused command changed the database:\nbefore %s\nafter  %s", before, after)
	}
}

// Our own dev databases moved to goose and kept golang-migrate's
// schema_migrations table. With goose at 180, that table is never read,
// whatever it holds. Mutation: refuse a goose database that also has
// schema_migrations; the unmarked and dirty cases then fail.
func TestUpIgnoresOldRecordWhenGooseHas180(t *testing.T) {
	ctx := context.Background()
	for i, tc := range []struct {
		name  string
		build func(t *testing.T, db *sql.DB)
	}{
		{"frozen record with its comment", func(t *testing.T, db *sql.DB) {
			writeV08Record(t, db, 180, false)
			mustExec(t, db, `COMMENT ON TABLE public.schema_migrations IS 'hardcount: frozen at 180; the migration record moved to public.goose_db_version'`)
		}},
		{"record at 180 without the comment", func(t *testing.T, db *sql.DB) {
			writeV08Record(t, db, 180, false)
		}},
		{"dirty record at 1", func(t *testing.T, db *sql.DB) {
			writeV08Record(t, db, 1, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, db := scratchDB(t, "hardcount_migrate_old_record_"+string(rune('a'+i)))
			if err := up(ctx, dsn, migrationFiles()); err != nil {
				t.Fatalf("Up: %v", err)
			}
			tc.build(t, db)
			before := fingerprint(t, db)
			if err := up(ctx, dsn, migrationFiles()); err != nil {
				t.Fatalf("second Up: %v", err)
			}
			if after := fingerprint(t, db); after != before {
				t.Errorf("a no-op Up changed the database:\nbefore %s\nafter  %s", before, after)
			}
			if err := CheckSchemaReady(db); err != nil {
				t.Fatalf("CheckSchemaReady: %v", err)
			}
			pool, err := openPool(dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pool.Close() }()
			p, err := readPlan(ctx, pool, migrationFiles())
			if err != nil {
				t.Fatalf("readPlan: %v", err)
			}
			if p.State != PlanUpToDate {
				t.Fatalf("status: state %q, want %q", p.State, PlanUpToDate)
			}
		})
	}
}

// UpReport says what it did, for `hardcount migrate` to print: both files on
// an empty database, then nothing at the same version (#1245: the command
// said "migrations applied successfully" either way).
func TestUpReportCountsWhatRan(t *testing.T) {
	dsn, _ := scratchDB(t, "hardcount_migrate_report")
	widgets, version := pastShipped(t, "widgets.sql")
	fsys := files(map[string]string{widgets: widgetsFile})
	sum, err := upReport(context.Background(), dsn, fsys)
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if sum.Applied != 2 || sum.Version != version {
		t.Fatalf("first Up = %+v, want 2 applied at version %d (the baseline and %s)", sum, version, widgets)
	}
	sum, err = upReport(context.Background(), dsn, fsys)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if sum.Applied != 0 || sum.Version != version {
		t.Fatalf("second Up = %+v, want nothing applied at version %d", sum, version)
	}
}
