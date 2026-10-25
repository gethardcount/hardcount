package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// priceShapesVersion is 00210_price_shapes.sql; 00211 validates its CHECKs.
const priceShapesVersion = 210

// priceShapesParents is the rows a rating rule, an override and an invoice
// line hang off.
const priceShapesParents = `
INSERT INTO tenants (id, name) VALUES ('ten_ps', 'Price shapes');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_ps', 'ten_ps', 'ps', 'PS');
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_ps', 'ten_ps', 'cus_ps', 'PS-1', 'draft', 'pending', 'USD', 0, 0, 0, now() - interval '30 days', now());
`

// TestPriceShapes_UpAndDown: 00210 gives the price shapes ADR-130 §3's names
// on an empty database: mode per_unit (not flat), unit_amount_cents and tiers
// (not flat_amount_cents and graduated_tiers), a decimal package amount and
// no overage, on rating rules and overrides; a provider cost rate is
// unit_cost_cents. 00211 validates the new CHECKs. The down puts the old
// schema back and turns rows written since into the old words, so a
// rollback works on a development database; the next up then refuses
// those rows, and runs once they are gone.
//
// Mutation: an up that keeps flat in the mode CHECK, or a down that does
// not turn per_unit back into flat, turns this red.
func TestPriceShapes_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_price_shapes")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, c := range []struct{ table, column string }{
		{"rating_rule_versions", "flat_amount_cents"}, {"rating_rule_versions", "graduated_tiers"}, {"rating_rule_versions", "overage_unit_amount_cents"},
		{"customer_price_overrides", "flat_amount_cents"}, {"customer_price_overrides", "graduated_tiers"}, {"customer_price_overrides", "overage_unit_amount_cents"},
		{"provider_cost_rates", "cost_per_token"},
	} {
		if columnExists(t, db, c.table, c.column) {
			t.Errorf("after the up, %s still has %s", c.table, c.column)
		}
	}
	for _, c := range []struct{ table, column, typ string }{
		{"rating_rule_versions", "unit_amount_cents", "numeric"}, {"rating_rule_versions", "tiers", "jsonb"}, {"rating_rule_versions", "package_amount_cents", "numeric"},
		{"customer_price_overrides", "unit_amount_cents", "numeric"}, {"customer_price_overrides", "tiers", "jsonb"}, {"customer_price_overrides", "package_amount_cents", "numeric"},
		{"provider_cost_rates", "unit_cost_cents", "numeric(22,12)"},
	} {
		if got := columnType(t, db, c.table, c.column); got != c.typ {
			t.Errorf("after the up, %s.%s is %q, want %s", c.table, c.column, got, c.typ)
		}
	}
	for _, name := range []string{"rating_rule_versions_mode_check", "customer_price_overrides_mode_check", "provider_cost_rates_unit_cost_cents_check"} {
		var validated bool
		if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = $1`, name).Scan(&validated); err != nil || !validated {
			t.Errorf("%s validated = %v (%v), want true (00211)", name, validated, err)
		}
	}
	if err := deadContractExec(ctx, db, priceShapesParents); err != nil {
		t.Fatalf("seed parents: %v", err)
	}
	err := withBypass(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, mode) VALUES ('rrv_old', 'ten_ps', 'old', 'Old', 'flat')`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "rating_rule_versions_mode_check") {
		t.Errorf("a rule with mode flat: err %v, want the CHECK to refuse it", err)
	}
	if err := deadContractExec(ctx, db, `
INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, lifecycle_state, mode, unit_amount_cents) VALUES
	('rrv_unit', 'ten_ps', 'unit', 'Unit', 'active', 'per_unit', 0.0035);
INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, lifecycle_state, mode, tiers) VALUES
	('rrv_grad', 'ten_ps', 'grad', 'Grad', 'active', 'graduated', '[{"up_to": 100, "unit_amount_cents": "5"}, {"up_to": null, "unit_amount_cents": "1"}]');
INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, lifecycle_state, mode, package_size, package_amount_cents) VALUES
	('rrv_pkg', 'ten_ps', 'pkg', 'Pkg', 'active', 'package', 1000, 120.6);
INSERT INTO customer_price_overrides (id, tenant_id, customer_id, rating_rule_version_id, rule_key, mode, tiers) VALUES
	('cpo_grad', 'ten_ps', 'cus_ps', 'rrv_grad', 'grad', 'graduated', '[{"up_to": 50, "unit_amount_cents": "4"}, {"up_to": null, "unit_amount_cents": "2"}]');
INSERT INTO provider_cost_rates (id, tenant_id, livemode, provider, model, token_type, unit_cost_cents) VALUES
	('pcr_ps', 'ten_ps', true, 'anthropic', 'claude', 'input', 0.0003);
INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents, pricing_mode) VALUES
	('li_ps', 'ten_ps', 'inv_ps', 'usage', 'Calls', 4, 4, 'per_unit');
`); err != nil {
		t.Fatalf("seed rows in the new shape: %v", err)
	}

	if _, err := down(ctx, dsn, migrationFiles(), priceShapesVersion-1); err != nil {
		t.Fatalf("down to %d: %v", priceShapesVersion-1, err)
	}
	for _, c := range []struct{ table, column string }{
		{"rating_rule_versions", "flat_amount_cents"}, {"rating_rule_versions", "graduated_tiers"}, {"rating_rule_versions", "overage_unit_amount_cents"},
		{"customer_price_overrides", "flat_amount_cents"}, {"customer_price_overrides", "graduated_tiers"}, {"customer_price_overrides", "overage_unit_amount_cents"},
		{"provider_cost_rates", "cost_per_token"},
	} {
		if !columnExists(t, db, c.table, c.column) {
			t.Errorf("after the down, %s has no %s", c.table, c.column)
		}
	}
	if got := columnType(t, db, "rating_rule_versions", "package_amount_cents"); got != "bigint" {
		t.Errorf("after the down, package_amount_cents is %q, want bigint", got)
	}
	if got := columnType(t, db, "provider_cost_rates", "cost_per_token"); got != "numeric(20,12)" {
		t.Errorf("after the down, cost_per_token is %q, want numeric(20,12)", got)
	}
	for q, want := range map[string]string{
		`SELECT id || ':' || mode || ':' || flat_amount_cents::text || ':' || package_amount_cents::text || ':' || graduated_tiers::text FROM rating_rule_versions ORDER BY id`: `rrv_grad:graduated:0:0:[{"up_to": 100, "unit_amount_cents": "5"}, {"up_to": 0, "unit_amount_cents": "1"}] ` +
			`rrv_pkg:package:0:121:[] rrv_unit:flat:0.0035:0:[]`,
		`SELECT mode || ':' || graduated_tiers::text FROM customer_price_overrides`: `graduated:[{"up_to": 50, "unit_amount_cents": "4"}, {"up_to": 0, "unit_amount_cents": "2"}]`,
		`SELECT cost_per_token::text FROM provider_cost_rates`:                      `0.000003000000`,
		`SELECT pricing_mode FROM invoice_line_items`:                               `flat`,
	} {
		if got := maRows(t, db, q); got != want {
			t.Errorf("after the down, %s\n got %s\nwant %s", q, got, want)
		}
	}

	// The rows are the old shape now, so the next up refuses them.
	if err := up(ctx, dsn, migrationFiles()); err == nil || !strings.Contains(err.Error(), "migration 00210") {
		t.Fatalf("up over the rolled-back rows: err %v, want 00210 to refuse them", err)
	}
	if err := deadContractExec(ctx, db, `DELETE FROM invoice_line_items; DELETE FROM customer_price_overrides; DELETE FROM rating_rule_versions; DELETE FROM provider_cost_rates;`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if columnExists(t, db, "rating_rule_versions", "flat_amount_cents") || !columnExists(t, db, "provider_cost_rates", "unit_cost_cents") {
		t.Fatal("the second up did not apply")
	}
}

// TestPriceShapes_RefusesRowsItDoesNotConvert: every row of the rule,
// override and provider cost tables, and every invoice line labelled flat,
// would need converting to the new shapes. This release has no upgrade from
// 0.8.x, so 00210 converts nothing: it stops before changing anything, the
// error counts each kind of row, and its hint links a runbook section that
// exists.
//
// Mutation: drop the guard and the up succeeds, red.
func TestPriceShapes_RefusesRowsItDoesNotConvert(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_price_shapes_refuse")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, priceShapesVersion)); err != nil {
		t.Fatalf("up to before %d: %v", priceShapesVersion, err)
	}
	if err := deadContractExec(ctx, db, priceShapesParents+`
INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, lifecycle_state, mode, flat_amount_cents) VALUES
	('rrv_a', 'ten_ps', 'a', 'A', 'active', 'flat', 1), ('rrv_b', 'ten_ps', 'b', 'B', 'active', 'flat', 2);
INSERT INTO customer_price_overrides (id, tenant_id, customer_id, rating_rule_version_id, rule_key, mode, flat_amount_cents) VALUES
	('cpo_a', 'ten_ps', 'cus_ps', 'rrv_a', 'a', 'flat', 1);
INSERT INTO provider_cost_rates (id, tenant_id, livemode, provider, model, token_type, cost_per_token) VALUES
	('pcr_a', 'ten_ps', true, 'anthropic', 'claude', 'input', 0.000003),
	('pcr_b', 'ten_ps', true, 'anthropic', 'claude', 'output', 0.000015),
	('pcr_c', 'ten_ps', true, 'anthropic', 'claude', 'cache_read', 0.0000003);
INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents, pricing_mode) VALUES
	('li_a', 'ten_ps', 'inv_ps', 'usage', 'A', 1, 1, 'flat'),
	('li_b', 'ten_ps', 'inv_ps', 'usage', 'B', 2, 2, 'graduated');
`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("the up succeeded over rows it does not convert")
	}
	for _, want := range []string{
		"migration 00210", "2 rating rule version(s)", "1 customer price override(s)", "3 provider cost rate(s)",
		"1 invoice line(s) priced flat", "install fresh",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	assertRunbookSectionExists(t, err.Error())
	if !columnExists(t, db, "rating_rule_versions", "flat_amount_cents") || !columnExists(t, db, "provider_cost_rates", "cost_per_token") {
		t.Error("the refused up changed the schema")
	}
}

// columnType is a column's type as Postgres prints it, with its precision.
func columnType(t *testing.T, db *sql.DB, table, column string) string {
	t.Helper()
	var typ string
	if err := db.QueryRow(`SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = ('public.' || $1)::regclass AND a.attname = $2 AND NOT a.attisdropped`, table, column).Scan(&typ); err != nil {
		t.Fatalf("type of %s.%s: %v", table, column, err)
	}
	return typ
}
