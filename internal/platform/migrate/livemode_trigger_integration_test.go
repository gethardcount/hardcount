package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// livemodeTriggerVersion is 00212_livemode_trigger_requires_mode.sql.
const livemodeTriggerVersion = 212

// insertCustomerWithMode inserts one customer under bypass RLS. A non-nil
// mode is set as app.livemode first; nil leaves it unset, which is why this
// does not use withBypass (that sets the mode). The INSERT always asks for
// livemode=true, so a stored false shows the trigger overrode it.
func insertCustomerWithMode(ctx context.Context, db *sql.DB, id string, mode *string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
		return err
	}
	if mode != nil {
		if _, err := tx.Exec(`SELECT set_config('app.livemode', $1, true)`, *mode); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO customers (id, tenant_id, external_id, display_name, livemode)
		VALUES ($1, 'ten_lm', $1, $1, true)`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// TestLivemodeTrigger_RequiresMode: after 00212 the set_livemode trigger
// stamps 'on' as live and 'off' as test, whatever the INSERT asked for, and
// refuses an insert whose transaction left app.livemode unset or empty.
// The empty case is the pooled connection: once a transaction has set the
// value locally, later transactions on that connection read ”. The down
// puts back the old rule, under which an unset mode was stamped live.
//
// Mutation: an up that keeps IS DISTINCT FROM 'off', or treats ” as live,
// turns this red.
func TestLivemodeTrigger_RequiresMode(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_livemode_trigger")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := deadContractExec(ctx, db, `INSERT INTO tenants (id, name) VALUES ('ten_lm', 'Livemode trigger')`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	on, off, empty := "on", "off", ""

	if err := insertCustomerWithMode(ctx, db, "cus_on", &on); err != nil {
		t.Fatalf("insert with mode on: %v", err)
	}
	if err := insertCustomerWithMode(ctx, db, "cus_off", &off); err != nil {
		t.Fatalf("insert with mode off: %v", err)
	}
	if got := maRows(t, db, `SELECT id || ':' || livemode::text FROM customers ORDER BY id`); got != "cus_off:false cus_on:true" {
		t.Errorf("stamped rows: got %q, want cus_off:false cus_on:true", got)
	}

	for name, mode := range map[string]*string{"unset": nil, "empty": &empty} {
		err := insertCustomerWithMode(ctx, db, "cus_"+name, mode)
		if err == nil || !strings.Contains(err.Error(), "needs app.livemode set to on or off") {
			t.Errorf("insert with mode %s: err %v, want the trigger to refuse it", name, err)
		}
	}

	// The pooled-connection shape: one connection, a transaction that sets
	// the mode locally, then a bypass insert that does not.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_reused', 'ten_lm', 'reused', 'Reused')`)
	_ = tx.Rollback()
	if err == nil || !strings.Contains(err.Error(), "needs app.livemode set to on or off") {
		t.Errorf("insert on a reused connection with no mode: err %v, want the trigger to refuse it", err)
	}

	if _, err := down(ctx, dsn, migrationFiles(), livemodeTriggerVersion-1); err != nil {
		t.Fatalf("down to %d: %v", livemodeTriggerVersion-1, err)
	}
	if err := insertCustomerWithMode(ctx, db, "cus_old_rule", nil); err != nil {
		t.Fatalf("after the down, insert with mode unset: %v", err)
	}
	if got := maRows(t, db, `SELECT livemode::text FROM customers WHERE id = 'cus_old_rule'`); got != "true" {
		t.Errorf("after the down, an unset mode stamped %q, want true (the old rule)", got)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := insertCustomerWithMode(ctx, db, "cus_again", nil); err == nil {
		t.Error("after the second up, an insert with mode unset succeeded, want the trigger to refuse it")
	}
}
