package migrate

import (
	"context"
	"database/sql"
	"io/fs"
	"strconv"
	"testing"
	"testing/fstest"
)

// pauseSetByVersion is 00193_subscription_pause_set_by.sql; 00194 validates
// its CHECKs.
const pauseSetByVersion = 193

// filesBefore is the embedded migration set without version v and later.
func filesBefore(t *testing.T, v int64) fstest.MapFS {
	t.Helper()
	all := migrationFiles()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(all, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n, err := strconv.ParseInt(e.Name()[:min(5, len(e.Name()))], 10, 64)
		if err != nil || n >= v {
			continue
		}
		data, err := fs.ReadFile(all, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: data}
	}
	return out
}

// T10. 00193 marks every pause set before it as the operator's (they were
// set under "until the operator resumes"), the pair CHECK refuses a pause
// with no set_by once 00194 validated it, and the Down removes the column.
func TestPauseSetBy_BackfillAndRollback(t *testing.T) {
	dsn, db := scratchDB(t, "velox_migrate_pause_set_by")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, pauseSetByVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", pauseSetByVersion, err)
	}
	// execErr runs one statement past RLS, as the app's bypass transactions do.
	execErr := func(q string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(q); err != nil {
			return err
		}
		return tx.Commit()
	}
	exec := func(q string) {
		t.Helper()
		if err := execErr(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('ten_a3', 'A3')`)
	exec(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_a3', 'ten_a3', 'a3', 'A3')`)
	exec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, pause_collection_behavior)
		VALUES ('sub_paused', 'ten_a3', 'paused', 'Paused', 'cus_a3', 'active', 'keep_as_draft'),
		       ('sub_running', 'ten_a3', 'running', 'Running', 'cus_a3', 'active', NULL)`)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	setBy := func(id string) sql.NullString {
		t.Helper()
		var v sql.NullString
		if err := db.QueryRow(`SELECT pause_collection_set_by FROM subscriptions WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := setBy("sub_paused"); !v.Valid || v.String != "operator" {
		t.Errorf("a pause set before the migration has set_by %v, want operator", v)
	}
	if v := setBy("sub_running"); v.Valid {
		t.Errorf("an unpaused subscription has set_by %q, want NULL", v.String)
	}
	var validated int
	if err := db.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname IN
		('subscriptions_pause_collection_set_by_check', 'subscriptions_pause_collection_set_by_pair_check') AND convalidated`).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	if validated != 2 {
		t.Errorf("%d of the two set_by CHECKs validated, want both", validated)
	}
	if err := execErr(`UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft' WHERE id = 'sub_running'`); err == nil {
		t.Error("a pause with no set_by was accepted, want the pair CHECK to refuse it")
	}

	if _, err := down(ctx, dsn, migrationFiles(), pauseSetByVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", pauseSetByVersion, err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'subscriptions' AND column_name = 'pause_collection_set_by')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the Down left pause_collection_set_by")
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
