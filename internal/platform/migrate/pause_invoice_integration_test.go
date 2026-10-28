package migrate

import (
	"context"
	"database/sql"
	"io/fs"
	"strconv"
	"testing"
	"testing/fstest"
)

// pauseInvoiceVersion is 00193_subscription_pause_invoice.sql; 00194
// validates its CHECK.
const pauseInvoiceVersion = 193

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

// T10. 00193 leaves every pause set before it as the operator's (no
// invoice: they were set under "until the operator resumes"), its CHECK
// refuses an invoice on a subscription with no pause, and the Down removes
// the column.
func TestPauseInvoice_UpAndRollback(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_pause_invoice")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, pauseInvoiceVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", pauseInvoiceVersion, err)
	}
	// execErr runs one statement past RLS, as the app's bypass transactions do.
	execErr := func(q string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', 'on', true)`); err != nil {
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
	var inv sql.NullString
	if err := db.QueryRow(`SELECT pause_collection_invoice_id FROM subscriptions WHERE id = 'sub_paused'`).Scan(&inv); err != nil {
		t.Fatal(err)
	}
	if inv.Valid {
		t.Errorf("a pause set before the migration has invoice %q, want none: it stays the operator's", inv.String)
	}
	var validated bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = 'subscriptions_pause_collection_invoice_id_check'`).Scan(&validated); err != nil {
		t.Fatal(err)
	}
	if !validated {
		t.Error("00194 left the CHECK not validated")
	}
	if err := execErr(`UPDATE subscriptions SET pause_collection_invoice_id = 'inv_x' WHERE id = 'sub_running'`); err == nil {
		t.Error("an invoice on a subscription with no pause was accepted, want the CHECK to refuse it")
	}
	exec(`UPDATE subscriptions SET pause_collection_invoice_id = 'inv_x' WHERE id = 'sub_paused'`)

	if _, err := down(ctx, dsn, migrationFiles(), pauseInvoiceVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", pauseInvoiceVersion, err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'subscriptions' AND column_name = 'pause_collection_invoice_id')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the Down left pause_collection_invoice_id")
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
