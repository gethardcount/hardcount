package migrate

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

// meterOneAggregationVersion is 00208_meter_one_aggregation.sql; 00209
// validates its CHECK.
const meterOneAggregationVersion = 208

// meterOneAggregationSeed is a sum, a max and a last meter, each with a
// pricing rule whose mode is its meter's (last_during_period is a last
// meter's), and a held usage row for the max meter's key.
const meterOneAggregationSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_ma', 'Meter aggregation');
INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency, flat_amount_cents)
	VALUES ('rrv_ma', 'ten_ma', 'ma', 'MA', 1, 'active', 'flat', 'USD', 1);
INSERT INTO meters (id, tenant_id, key, name, aggregation) VALUES
	('mtr_sum', 'ten_ma', 'calls', 'Calls', 'sum'),
	('mtr_max', 'ten_ma', 'gpus', 'GPUs', 'max'),
	('mtr_last', 'ten_ma', 'seats', 'Seats', 'last');
INSERT INTO meter_pricing_rules (id, tenant_id, meter_id, rating_rule_version_id, dimension_match, aggregation_mode, priority) VALUES
	('mpr_sum', 'ten_ma', 'mtr_sum', 'rrv_ma', '{"model": "a"}', 'sum', 100),
	('mpr_max', 'ten_ma', 'mtr_max', 'rrv_ma', '{}', 'max', 100),
	('mpr_last', 'ten_ma', 'mtr_last', 'rrv_ma', '{}', 'last_during_period', 100);
INSERT INTO usage_held_events (id, tenant_id, source, external_customer_id, meter_key, quantity, idempotency_key, event_at, reason)
	VALUES ('hue_ma', 'ten_ma', 'litellm', 'ext_ma', 'gpus', 3, 'idem_ma', now(), 'customer_not_found');
`

// TestMeterOneAggregation_UpAndDown: a meter has one aggregation (ADR-130
// §2). 00208 drops the pricing rule's aggregation_mode when every rule's
// mode is its meter's, renames meters.key and the held rows' meter_key to
// event_name, and adds a CHECK on the meter's aggregation that 00209
// validates. The down puts each rule's mode back from its meter's
// aggregation and the old names back, so a rollback and a second up work.
// The down goes to the version before, not one step, so a later migration
// does not take this one's place. The test stops before 00210, which
// refuses a database that holds rating rules (TestPriceShapes_*).
//
// Mutation: an up that keeps aggregation_mode, or a down that does not
// restore the rule modes, turns this red.
func TestMeterOneAggregation_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_meteragg_b_migrate")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, meterOneAggregationVersion)); err != nil {
		t.Fatalf("up to before %d: %v", meterOneAggregationVersion, err)
	}
	if err := deadContractExec(ctx, db, meterOneAggregationSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := up(ctx, dsn, filesBefore(t, priceShapesVersion)); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, c := range []struct{ table, column string }{
		{"meter_pricing_rules", "aggregation_mode"}, {"meters", "key"}, {"usage_held_events", "meter_key"},
	} {
		if columnExists(t, db, c.table, c.column) {
			t.Errorf("after the up, %s still has %s", c.table, c.column)
		}
	}
	if got := maRows(t, db, `SELECT id || ':' || event_name || ':' || aggregation FROM meters ORDER BY id`); got != "mtr_last:seats:last mtr_max:gpus:max mtr_sum:calls:sum" {
		t.Errorf("after the up, meters = %q", got)
	}
	if got := maRows(t, db, `SELECT event_name FROM usage_held_events`); got != "gpus" {
		t.Errorf("after the up, the held row's event_name = %q, want gpus", got)
	}
	var validated bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = 'meters_aggregation_check'`).Scan(&validated); err != nil || !validated {
		t.Errorf("meters_aggregation_check validated = %v (%v), want true (00209)", validated, err)
	}
	err := withBypass(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO meters (id, tenant_id, event_name, name, aggregation) VALUES ('mtr_bad', 'ten_ma', 'bad', 'Bad', 'last_ever')`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "meters_aggregation_check") {
		t.Errorf("a meter with aggregation last_ever: err %v, want the CHECK to refuse it", err)
	}
	err = withBypass(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO meters (id, tenant_id, event_name, name) VALUES ('mtr_dup', 'ten_ma', 'calls', 'Dup')`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "meters_tenant_id_livemode_event_name_key") {
		t.Errorf("a second meter named calls: err %v, want the renamed unique constraint to refuse it", err)
	}

	if _, err := down(ctx, dsn, filesBefore(t, priceShapesVersion), meterOneAggregationVersion-1); err != nil {
		t.Fatalf("down to %d: %v", meterOneAggregationVersion-1, err)
	}
	for _, c := range []struct{ table, column string }{
		{"meter_pricing_rules", "aggregation_mode"}, {"meters", "key"}, {"usage_held_events", "meter_key"},
	} {
		if !columnExists(t, db, c.table, c.column) {
			t.Errorf("after the down, %s has no %s", c.table, c.column)
		}
	}
	if got := maRows(t, db, `SELECT id || ':' || aggregation_mode FROM meter_pricing_rules ORDER BY id`); got != "mpr_last:last_during_period mpr_max:max mpr_sum:sum" {
		t.Errorf("after the down, rule modes = %q, want each meter's aggregation", got)
	}
	if got := maRows(t, db, `SELECT key FROM meters ORDER BY id`); got != "seats gpus calls" {
		t.Errorf("after the down, meter keys = %q", got)
	}
	if got := maRows(t, db, `SELECT conname FROM pg_constraint WHERE conname IN ('meters_aggregation_check', 'meters_tenant_id_livemode_key_key', 'meter_pricing_rules_aggregation_mode_check') ORDER BY conname`); got != "meter_pricing_rules_aggregation_mode_check meters_tenant_id_livemode_key_key" {
		t.Errorf("after the down, constraints = %q", got)
	}

	if err := up(ctx, dsn, filesBefore(t, priceShapesVersion)); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if columnExists(t, db, "meter_pricing_rules", "aggregation_mode") || !columnExists(t, db, "meters", "event_name") {
		t.Fatal("the second up did not apply")
	}
}

// TestMeterOneAggregation_RefusesRulesThatAggregateDifferently: a rule
// whose mode is not its meter's aggregation would bill differently under
// one aggregation per meter, so 00208 stops before changing anything and
// counts them. A last_ever rule is one: no meter aggregation means it. This
// release has no upgrade from 0.8.x, so nothing is converted.
//
// Mutation: drop the guard and the up succeeds, red.
func TestMeterOneAggregation_RefusesRulesThatAggregateDifferently(t *testing.T) {
	for name, rules := range map[string]string{
		"a max rule on a sum meter":        `('mpr_x', 'ten_ma', 'mtr_sum', 'rrv_mx', '{}', 'max', 100)`,
		"a last_ever rule on a last meter": `('mpr_x', 'ten_ma', 'mtr_last', 'rrv_mx', '{}', 'last_ever', 100)`,
	} {
		t.Run(name, func(t *testing.T) {
			dsn, db := scratchDB(t, "velox_test_meteragg_b_migrate_refuse")
			ctx := context.Background()
			if err := up(ctx, dsn, filesBefore(t, meterOneAggregationVersion)); err != nil {
				t.Fatalf("up to before %d: %v", meterOneAggregationVersion, err)
			}
			if err := deadContractExec(ctx, db, meterOneAggregationSeed+
				`INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency, flat_amount_cents)
					VALUES ('rrv_mx', 'ten_ma', 'mx', 'MX', 1, 'active', 'flat', 'USD', 1);
				INSERT INTO meter_pricing_rules (id, tenant_id, meter_id, rating_rule_version_id, dimension_match, aggregation_mode, priority) VALUES `+rules+`;`); err != nil {
				t.Fatalf("seed: %v", err)
			}
			err := up(ctx, dsn, migrationFiles())
			if err == nil {
				t.Fatal("the up succeeded over a rule that aggregates differently from its meter")
			}
			for _, want := range []string{"migration 00208", "1 pricing rule(s) whose aggregation_mode is not their meter's aggregation", "install fresh"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if !columnExists(t, db, "meter_pricing_rules", "aggregation_mode") || !columnExists(t, db, "meters", "key") {
				t.Error("the refused up changed the schema")
			}
		})
	}
}

// TestMeterOneAggregation_RefusesMetersWithAnotherAggregation: until 00208
// a meter's aggregation had no CHECK, so a row written by raw SQL could hold
// an empty string or a value billing does not know. 00208 adds the CHECK and 00209
// validates it, so 00208 stops first and counts those meters. The HINT links
// the runbook section, and that section must explain this line too.
//
// Mutation: drop the meters count from the guard and the up stops in 00209
// with a bare CHECK error, red.
func TestMeterOneAggregation_RefusesMetersWithAnotherAggregation(t *testing.T) {
	const section = "docs/ops/runbook/migration-failed-midway.md#00208-found-rows-it-cannot-keep-as-they-bill"
	for name, agg := range map[string]string{"an empty aggregation": "", "an unknown aggregation": "average"} {
		t.Run(name, func(t *testing.T) {
			dsn, db := scratchDB(t, "velox_test_meteragg_f2_migrate_meters")
			ctx := context.Background()
			if err := up(ctx, dsn, filesBefore(t, meterOneAggregationVersion)); err != nil {
				t.Fatalf("up to before %d: %v", meterOneAggregationVersion, err)
			}
			if err := deadContractExec(ctx, db, meterOneAggregationSeed+
				`INSERT INTO meters (id, tenant_id, key, name, aggregation) VALUES ('mtr_odd', 'ten_ma', 'odd', 'Odd', '`+agg+`');`); err != nil {
				t.Fatalf("seed: %v", err)
			}
			err := up(ctx, dsn, migrationFiles())
			if err == nil {
				t.Fatal("the up succeeded over a meter whose aggregation is not sum, count, max or last")
			}
			for _, want := range []string{"migration 00208", "1 meter(s) whose aggregation is not sum, count, max or last", section} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "pricing rule(s)") {
				t.Errorf("error %q counts pricing rules; the seed's rules match their meters", err)
			}
			if !columnExists(t, db, "meter_pricing_rules", "aggregation_mode") || !columnExists(t, db, "meters", "key") {
				t.Error("the refused up changed the schema")
			}
		})
	}

	runbook, err := os.ReadFile("../../../docs/ops/runbook/migration-failed-midway.md")
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	_, after, ok := strings.Cut(string(runbook), "\n## 00208 found rows it cannot keep as they bill\n")
	if !ok {
		t.Fatalf("the runbook has no section %s", section)
	}
	body, _, _ := strings.Cut(after, "\n## ")
	for _, want := range []string{"pricing rule(s) whose aggregation_mode is not their meter's aggregation", "meter(s) whose aggregation is not sum, count, max or last", "FROM meters WHERE aggregation NOT IN"} {
		if !strings.Contains(body, want) {
			t.Errorf("the runbook section does not show %q", want)
		}
	}
}

// maRows is a query's rows, each one text column, joined by spaces, read
// with row-level security bypassed.
func maRows(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	var out []string
	err := withBypass(context.Background(), db, func(tx *sql.Tx) error {
		rows, err := tx.Query(q)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return strings.Join(out, " ")
}
