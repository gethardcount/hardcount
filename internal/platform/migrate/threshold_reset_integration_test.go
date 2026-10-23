package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// thresholdResetVersion is the migration that drops
// subscriptions.billing_threshold_reset_cycle.
const thresholdResetVersion = 205

// thresholdResetSeed is a subscription in each threshold shape the column
// could describe, and an issued invoice. sub_amt asked for a new period at
// each fire with an amount threshold, sub_item with an item threshold only;
// sub_keep kept the period; sub_none has no threshold (the flag was inert).
const thresholdResetSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_tr', 'Threshold reset');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_tr', 'ten_tr', 'tr', 'TR');
INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, status) VALUES ('pln_tr', 'ten_tr', 'tr', 'TR', 'USD', 'monthly', 'active');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_threshold_amount_gte, billing_threshold_reset_cycle) VALUES
	('sub_amt', 'ten_tr', 'amt', 'A', 'cus_tr', 'active', 5000, true),
	('sub_item', 'ten_tr', 'item', 'I', 'cus_tr', 'active', NULL, true),
	('sub_keep', 'ten_tr', 'keep', 'K', 'cus_tr', 'active', 5000, false),
	('sub_none', 'ten_tr', 'none', 'N', 'cus_tr', 'active', NULL, true);
INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id) VALUES ('si_item', 'ten_tr', 'sub_item', 'pln_tr');
INSERT INTO subscription_item_thresholds (subscription_id, subscription_item_id, tenant_id, usage_gte) VALUES ('sub_item', 'si_item', 'ten_tr', 1000);
INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end, billing_reason)
	VALUES ('inv_tr', 'ten_tr', 'cus_tr', 'sub_amt', 'TR-1', 'finalized', 'pending', 'USD', 5100, 5100, 5100,
		now() - interval '20 days', now() - interval '5 days', 'threshold');
`

// TestThresholdResetColumnDropped: a spend threshold always keeps the
// billing period, so the migration drops billing_threshold_reset_cycle.
// A subscription that had it true keeps its thresholds (the amount and the
// item rows), and no invoice or line changes: the column was configuration,
// read only when a threshold fired. The down puts the column back as 00201
// left it, false on every row, so a rollback and a second up both work. The
// down goes to the version before, not one step, so a later migration does
// not take this one's place.
//
// Mutation: an up that keeps the column turns this red.
func TestThresholdResetColumnDropped(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_noreset_b_migrate")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, thresholdResetVersion)); err != nil {
		t.Fatalf("up to before %d: %v", thresholdResetVersion, err)
	}
	if err := deadContractExec(ctx, db, thresholdResetSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := thresholdResetSnapshot(t, db)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up: %v", err)
	}
	if columnExists(t, db, "subscriptions", "billing_threshold_reset_cycle") {
		t.Fatal("after the up, subscriptions still has billing_threshold_reset_cycle")
	}
	if after := thresholdResetSnapshot(t, db); after != before {
		t.Errorf("the up changed thresholds or invoices:\nbefore %s\nafter  %s", before, after)
	}

	if _, err := down(ctx, dsn, migrationFiles(), thresholdResetVersion-1); err != nil {
		t.Fatalf("down to %d: %v", thresholdResetVersion-1, err)
	}
	if !columnExists(t, db, "subscriptions", "billing_threshold_reset_cycle") {
		t.Fatal("after the down, billing_threshold_reset_cycle is not back")
	}
	if d := columnDefault(t, db, "subscriptions", "billing_threshold_reset_cycle"); d != "false" {
		t.Errorf("after the down, billing_threshold_reset_cycle defaults to %q, want false (as 00201 left it)", d)
	}
	var nullable string
	if err := db.QueryRow(`SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'subscriptions' AND column_name = 'billing_threshold_reset_cycle'`).Scan(&nullable); err != nil || nullable != "NO" {
		t.Errorf("after the down, billing_threshold_reset_cycle is_nullable = %q (%v), want NO", nullable, err)
	}
	var trues int
	if err := withBypass(ctx, db, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM subscriptions WHERE billing_threshold_reset_cycle`).Scan(&trues)
	}); err != nil || trues != 0 {
		t.Errorf("after the down, %d subscriptions (%v) have billing_threshold_reset_cycle true, want 0", trues, err)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if columnExists(t, db, "subscriptions", "billing_threshold_reset_cycle") {
		t.Fatal("after the second up, subscriptions still has billing_threshold_reset_cycle")
	}
}

// thresholdResetSnapshot is every seeded subscription's threshold amount and
// item thresholds, and every invoice and line, as text.
func thresholdResetSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	err := withBypass(context.Background(), db, func(tx *sql.Tx) error {
		for _, q := range []string{
			`SELECT id || ':' || COALESCE(billing_threshold_amount_gte::text, 'null') || ':' || status FROM subscriptions WHERE tenant_id = 'ten_tr' ORDER BY id`,
			`SELECT subscription_item_id || ':' || usage_gte::text FROM subscription_item_thresholds WHERE tenant_id = 'ten_tr' ORDER BY subscription_item_id`,
			`SELECT row_to_json(i)::text FROM invoices i WHERE tenant_id = 'ten_tr' ORDER BY id`,
			`SELECT row_to_json(l)::text FROM invoice_line_items l WHERE tenant_id = 'ten_tr' ORDER BY id`,
		} {
			rows, err := tx.Query(q)
			if err != nil {
				return err
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					_ = rows.Close()
					return err
				}
				b.WriteString(s + "\n")
			}
			_ = rows.Close()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return b.String()
}

// withBypass runs fn in a transaction with row-level security bypassed.
func withBypass(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
		return err
	}
	return fn(tx)
}
