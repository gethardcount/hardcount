package migrate

import (
	"context"
	"testing"
)

// restoreCatchupVersion is 00223_restore_catchup.sql.
const restoreCatchupVersion = 223

// A backup made before 00223 restores without the restore marker table, so
// scripts/restore.sh creates it and writes the marker (ADR-135). 00223 then
// applies over that table: it keeps the marker, so serve still refuses to
// start, and leaves the limited app role read-only on it. The Down removes
// the table, and Up runs again.
func TestRestoreCatchup_UpOverRestoredMarker(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_restore_catchup")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, restoreCatchupVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", restoreCatchupVersion, err)
	}
	// What restore.sh runs on a database from before 00223, under the
	// default privileges that give hardcount_app every right on a new table.
	mustExec(t, db, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO hardcount_app`)
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS public.restore_catchup (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    set_at timestamptz NOT NULL DEFAULT now(),
    set_by text NOT NULL,
    anchor timestamptz
)`)
	mustExec(t, db, `INSERT INTO public.restore_catchup (set_by) VALUES ('restore.sh')`)
	mustExec(t, db, `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM hardcount_app`)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up over the restored marker: %v", err)
	}
	var setBy string
	if err := db.QueryRow(`SELECT set_by FROM restore_catchup`).Scan(&setBy); err != nil || setBy != "restore.sh" {
		t.Fatalf("marker after Up: %q, %v; want the restored marker kept", setBy, err)
	}
	var sel, del bool
	if err := db.QueryRow(`SELECT has_table_privilege('hardcount_app', 'public.restore_catchup', 'SELECT'),
		has_table_privilege('hardcount_app', 'public.restore_catchup', 'DELETE')`).Scan(&sel, &del); err != nil {
		t.Fatal(err)
	}
	if !sel || del {
		t.Fatalf("hardcount_app: select=%v delete=%v, want read only", sel, del)
	}

	if _, err := down(ctx, dsn, migrationFiles(), restoreCatchupVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", restoreCatchupVersion, err)
	}
	if tableExists(t, db, "public.restore_catchup") {
		t.Error("the Down left restore_catchup")
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
