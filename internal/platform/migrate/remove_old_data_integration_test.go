package migrate

import (
	"context"
	"testing"
)

// oldDataVersion is 00217_remove_publishable_keys_and_old_data.sql; 00218
// validates the CHECKs it narrows.
const oldDataVersion = 217

// TestRemoveOldData_UpNarrowsAndDownRestores: after 00217 and 00218 the
// publishable key type, the discount and tax line types and the
// manually_resolved resolution are refused by their CHECKs, the values
// that stay still go in, the coupon tables and invoices.stripe_invoice_id
// are gone, and every narrowed CHECK is validated. The Down puts the old
// schema back, each old value goes in again, and Up runs again. Mutation:
// an Up that keeps any of these turns this red.
func TestRemoveOldData_UpNarrowsAndDownRestores(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_d06b_migrate_old_data")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := deadContractExec(ctx, db, deadContractSeedNow); err != nil {
		t.Fatalf("seed: %v", err)
	}

	type stmt struct{ name, sql string }
	removed := []stmt{
		{"api key type publishable", `INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id)
			VALUES ('hc_key_pub', 'hc_pub_test_0000', 'h', 'publishable', 'Pub', 'ten_dc')`},
		{"line type discount", `INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description) VALUES ('ten_dc', 'inv_dc', 'discount', 'd')`},
		{"line type tax", `INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description) VALUES ('ten_dc', 'inv_dc', 'tax', 't')`},
		{"resolution manually_resolved", `UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'manually_resolved', resolved_at = now() WHERE id = 'drun_dc'`},
	}
	for _, s := range removed {
		if err := deadContractExec(ctx, db, s.sql); err == nil {
			t.Errorf("%s was accepted after 00217", s.name)
		}
	}

	kept := []stmt{
		{"api key types", `INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id) VALUES
			('hc_key_sec', 'hc_secret_test_0000', 'h1', 'secret', 'Sec', 'ten_dc'),
			('hc_key_plat', 'hc_platform_test_0000', 'h2', 'platform', 'Plat', 'ten_dc')`},
		{"line types", `INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description) VALUES
			('ten_dc', 'inv_dc', 'base_fee', 'b'), ('ten_dc', 'inv_dc', 'usage', 'u'), ('ten_dc', 'inv_dc', 'add_on', 'a')`},
		{"resolutions", `UPDATE invoice_dunning_runs SET state = 'escalated', resolution = 'retries_exhausted' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'active', resolution = 'action_failed' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'amount_below_minimum' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'invoice_not_collectible' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'payment_recovered' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'invoice_voided' WHERE id = 'drun_dc'`},
	}
	for _, s := range kept {
		if err := deadContractExec(ctx, db, s.sql); err != nil {
			t.Errorf("%s was refused after 00217: %v", s.name, err)
		}
	}

	for _, table := range []string{"coupons", "coupon_redemptions", "customer_discounts"} {
		if tableExists(t, db, "public."+table) {
			t.Errorf("00217 left the table %s", table)
		}
	}
	if columnExists(t, db, "invoices", "stripe_invoice_id") || indexExists(t, db, "idx_invoices_stripe_invoice_id") {
		t.Error("00217 left invoices.stripe_invoice_id or its unique index")
	}
	for _, c := range []string{"api_keys_key_type_check", "invoice_line_items_line_type_check", "invoice_dunning_runs_resolution_check"} {
		var validated bool
		if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = $1`, c).Scan(&validated); err != nil {
			t.Errorf("read %s: %v", c, err)
			continue
		}
		if !validated {
			t.Errorf("00218 left %s not validated", c)
		}
	}

	if _, err := down(ctx, dsn, migrationFiles(), oldDataVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", oldDataVersion, err)
	}
	for _, table := range []string{"coupons", "coupon_redemptions", "customer_discounts"} {
		if !tableExists(t, db, "public."+table) {
			t.Errorf("the Down did not put back the table %s", table)
			continue
		}
		var rls, force bool
		if err := db.QueryRow(`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, "public."+table).Scan(&rls, &force); err != nil || !rls || !force {
			t.Errorf("the Down put back %s without forced row-level security (%v, %v, %v)", table, rls, force, err)
		}
	}
	if !columnExists(t, db, "invoices", "stripe_invoice_id") || !indexExists(t, db, "idx_invoices_stripe_invoice_id") {
		t.Error("the Down did not put back invoices.stripe_invoice_id and its unique index")
	}
	// Each old value goes in again. Each statement runs in a transaction
	// that is rolled back, so the next Up finds no such row.
	restored := append([]stmt{}, removed...)
	restored = append(restored,
		stmt{"coupon rows", `INSERT INTO coupons (id, tenant_id, code) VALUES ('cpn_dc', 'ten_dc', 'DC');
			INSERT INTO coupon_redemptions (id, tenant_id, coupon_id, customer_id) VALUES ('crd_dc', 'ten_dc', 'cpn_dc', 'cus_dc');
			INSERT INTO customer_discounts (tenant_id, customer_id, coupon_id) VALUES ('ten_dc', 'cus_dc', 'cpn_dc')`},
		stmt{"stripe_invoice_id", `UPDATE invoices SET stripe_invoice_id = 'in_dc' WHERE id = 'inv_dc'`},
	)
	for _, s := range restored {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(s.sql); err != nil {
			t.Errorf("after the Down, %s was refused: %v", s.name, err)
		}
		_ = tx.Rollback()
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if tableExists(t, db, "public.coupons") || columnExists(t, db, "invoices", "stripe_invoice_id") {
		t.Error("the second Up left the coupon tables or invoices.stripe_invoice_id")
	}
}
