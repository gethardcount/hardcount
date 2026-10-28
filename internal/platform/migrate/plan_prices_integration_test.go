package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// planPricesVersion is 00212_plan_prices.sql.
const planPricesVersion = 212

// planPricesTables are the tables 00212 creates, drops or changes.
var planPricesTables = []string{
	"rating_rule_versions", "meter_pricing_rules", "customer_price_overrides", "plans", "meters",
	"invoice_line_items", "plan_prices", "plan_price_versions", "plan_price_version_clocks",
}

// schemaOf describes tables as the catalog holds them: each column with its
// type, nullness and default; each constraint, index, trigger and policy;
// and whether row-level security is on and forced. A table that does not
// exist reads "absent".
func schemaOf(t *testing.T, db *sql.DB, tables ...string) string {
	t.Helper()
	var out []string
	for _, table := range tables {
		if !tableExists(t, db, "public."+table) {
			out = append(out, table+": absent")
			continue
		}
		for _, q := range []string{
			`SELECT 'col ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod) || CASE WHEN a.attnotnull THEN ' not null' ELSE '' END
			        || COALESCE(' default ' || pg_get_expr(d.adbin, d.adrelid), '')
			   FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			  WHERE a.attrelid = ('public.' || $1)::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attname`,
			`SELECT 'con ' || conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = ('public.' || $1)::regclass ORDER BY conname`,
			`SELECT 'idx ' || indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename = $1 ORDER BY indexname`,
			`SELECT 'trg ' || tgname || ' ' || pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = ('public.' || $1)::regclass AND NOT tgisinternal ORDER BY tgname`,
			`SELECT 'pol ' || policyname || ' ' || COALESCE(qual, '') || ' ' || COALESCE(with_check, '') FROM pg_policies WHERE schemaname = 'public' AND tablename = $1 ORDER BY policyname`,
			`SELECT 'rls ' || relrowsecurity::text || ' ' || relforcerowsecurity::text FROM pg_class WHERE oid = ('public.' || $1)::regclass`,
			`SELECT 'grant ' || grantee || ' ' || string_agg(privilege_type, ',' ORDER BY privilege_type) FROM information_schema.role_table_grants
			  WHERE table_schema = 'public' AND table_name = $1 AND grantee = 'hardcount_app' GROUP BY grantee`,
		} {
			rows, err := db.Query(q, table)
			if err != nil {
				t.Fatalf("describe %s: %v", table, err)
			}
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				out = append(out, table+": "+line)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return strings.Join(out, "\n")
}

// planPricesSeed is a tenant, a meter, a plan with a price and a version, a
// customer with an override, and an invoice line priced by the price, in
// the schema after 00212.
const planPricesSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_pp', 'Plan prices');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_pp', 'ten_pp', 'pp', 'PP');
INSERT INTO meters (id, tenant_id, event_name, name, unit, aggregation) VALUES ('mtr_pp', 'ten_pp', 'calls', 'Calls', 'call', 'sum');
INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, base_amount_cents)
	VALUES ('pln_pp', 'ten_pp', 'pp', 'PP', 'USD', 'monthly', 0);
INSERT INTO plan_prices (id, tenant_id, plan_id, meter_id, dimension_match) VALUES ('prc_pp', 'ten_pp', 'pln_pp', 'mtr_pp', '{"model": "a"}');
INSERT INTO plan_price_versions (tenant_id, price_id, valid_from, mode, unit_amount_cents) VALUES ('ten_pp', 'prc_pp', '-infinity', 'per_unit', 2);
INSERT INTO customer_price_overrides (id, tenant_id, customer_id, price_id, mode, unit_amount_cents) VALUES ('cpo_pp', 'ten_pp', 'cus_pp', 'prc_pp', 'per_unit', 1);
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_pp', 'ten_pp', 'cus_pp', 'PP-1', 'draft', 'pending', 'USD', 0, 0, 0, now() - interval '30 days', now());
INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents, price_id, dimension_match)
	VALUES ('li_pp', 'ten_pp', 'inv_pp', 'usage', 'Calls', 4, 4, 'prc_pp', '{"model": "a"}');
`

// TestPlanPrices_UpAndDown: 00212 makes plan prices, their versions and
// their clock stamps, forces row-level security on them, points overrides
// and invoice lines at a price, and drops the rating rules, the meter
// pricing rules, the meter's default price and the plan's meter list
// (ADR-130 §1). The down puts back exactly the schema 00211 left (columns,
// defaults, constraints, indexes, triggers, policies and grants), with no
// rows in the pricing tables, so a rollback works on a development database
// and the next up runs again.
//
// Mutation: a down that leaves out an index, a trigger or a policy of a
// table it puts back turns this red.
func TestPlanPrices_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_plan_prices")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, planPricesVersion)); err != nil {
		t.Fatalf("up to before %d: %v", planPricesVersion, err)
	}
	before := schemaOf(t, db, planPricesTables...)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, table := range []string{"rating_rule_versions", "meter_pricing_rules"} {
		if tableExists(t, db, "public."+table) {
			t.Errorf("after the up, %s still exists", table)
		}
	}
	for _, c := range []struct{ table, column string }{{"plans", "meter_ids"}, {"meters", "rating_rule_version_id"}, {"invoice_line_items", "rating_rule_version_id"}, {"customer_price_overrides", "rule_key"}} {
		if columnExists(t, db, c.table, c.column) {
			t.Errorf("after the up, %s still has %s", c.table, c.column)
		}
	}
	for _, c := range []struct{ table, column string }{{"customer_price_overrides", "price_id"}, {"invoice_line_items", "price_id"}, {"invoice_line_items", "dimension_match"}} {
		if !columnExists(t, db, c.table, c.column) {
			t.Errorf("after the up, %s has no %s", c.table, c.column)
		}
	}
	for _, table := range []string{"plan_prices", "plan_price_versions", "plan_price_version_clocks", "customer_price_overrides"} {
		if got := maRows(t, db, `SELECT relrowsecurity::text || ' ' || relforcerowsecurity::text FROM pg_class WHERE oid = 'public.`+table+`'::regclass`); got != "true true" {
			t.Errorf("%s row-level security (enabled forced) = %s, want true true", table, got)
		}
	}
	if err := deadContractExec(ctx, db, planPricesSeed); err != nil {
		t.Fatalf("seed rows in the new schema: %v", err)
	}

	if _, err := down(ctx, dsn, migrationFiles(), planPricesVersion-1); err != nil {
		t.Fatalf("down to %d: %v", planPricesVersion-1, err)
	}
	if after := schemaOf(t, db, planPricesTables...); after != before {
		t.Errorf("the down did not put back the schema 00211 left:\n--- before 00212\n%s\n--- after the down\n%s", before, after)
	}
	if got := maRows(t, db, `SELECT (SELECT count(*) FROM rating_rule_versions) || ' ' || (SELECT count(*) FROM customer_price_overrides) || ' ' || (SELECT count(*) FROM plans)`); got != "0 0 1" {
		t.Errorf("after the down, rules overrides plans = %s, want 0 0 1 (the plan stays, with no meters)", got)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if !tableExists(t, db, "public.plan_prices") || tableExists(t, db, "public.rating_rule_versions") {
		t.Fatal("the second up did not apply")
	}
}

// TestPlanPrices_RefusesPricesItDoesNotMove: this release has no upgrade
// from 0.8.x, so 00212 moves no price. Over any rating rule, meter pricing
// rule, override, plan meter list or invoice line priced by a rule, it stops
// before changing anything, the error counts each kind of row, and its hint
// links a runbook section that exists.
//
// Mutation: drop the guard and the up succeeds (dropping the rows), red.
func TestPlanPrices_RefusesPricesItDoesNotMove(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_plan_prices_refuse")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, planPricesVersion)); err != nil {
		t.Fatalf("up to before %d: %v", planPricesVersion, err)
	}
	if err := deadContractExec(ctx, db, `
INSERT INTO tenants (id, name) VALUES ('ten_pp', 'Plan prices');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_pp', 'ten_pp', 'pp', 'PP');
INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, lifecycle_state, mode, unit_amount_cents) VALUES
	('rrv_a', 'ten_pp', 'a', 'A', 'active', 'per_unit', 1), ('rrv_b', 'ten_pp', 'b', 'B', 'active', 'per_unit', 2);
INSERT INTO meters (id, tenant_id, event_name, name, unit, aggregation, rating_rule_version_id) VALUES ('mtr_pp', 'ten_pp', 'calls', 'Calls', 'call', 'sum', 'rrv_a');
INSERT INTO meter_pricing_rules (id, tenant_id, meter_id, rating_rule_version_id, dimension_match) VALUES ('mpr_pp', 'ten_pp', 'mtr_pp', 'rrv_b', '{"model": "b"}');
INSERT INTO customer_price_overrides (id, tenant_id, customer_id, rating_rule_version_id, rule_key, mode, unit_amount_cents) VALUES
	('cpo_pp', 'ten_pp', 'cus_pp', 'rrv_a', 'a', 'per_unit', 1);
INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, base_amount_cents, meter_ids) VALUES
	('pln_a', 'ten_pp', 'a', 'A', 'USD', 'monthly', 0, '["mtr_pp"]'),
	('pln_b', 'ten_pp', 'b', 'B', 'USD', 'monthly', 0, '["mtr_pp"]'),
	('pln_c', 'ten_pp', 'c', 'C', 'USD', 'monthly', 0, '[]');
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_pp', 'ten_pp', 'cus_pp', 'PP-1', 'draft', 'pending', 'USD', 0, 0, 0, now() - interval '30 days', now());
INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents, rating_rule_version_id) VALUES
	('li_a', 'ten_pp', 'inv_pp', 'usage', 'A', 1, 1, 'rrv_a'),
	('li_b', 'ten_pp', 'inv_pp', 'base_fee', 'Base', 2, 2, NULL);
`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := schemaOf(t, db, planPricesTables...)
	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("the up succeeded over prices it does not move")
	}
	for _, want := range []string{
		"migration 00212", "2 rating rule version(s)", "1 meter pricing rule(s)", "1 customer price override(s)",
		"2 plan(s) with meters", "1 invoice line(s) priced by a rating rule", "install fresh",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	assertRunbookSectionExists(t, err.Error())
	if after := schemaOf(t, db, planPricesTables...); after != before {
		t.Error("the refused up changed the schema")
	}
}
