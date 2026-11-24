package migrate

import (
	"context"
	"testing"
)

// dropTestClockSchemaVersion is 00242_drop_test_clock_schema.sql.
const dropTestClockSchemaVersion = 242

// dropTestClockSchemaTables are the tables 00242 drops, or drops a column,
// index or constraint of.
var dropTestClockSchemaTables = []string{
	"test_clocks", "plan_price_version_clocks", "customers", "subscriptions",
	"email_outbox", "audit_log", "invoices", "credit_notes", "leader_leases",
}

// TestDropTestClockSchema_UpAndDown: 00242 leaves no table, column or lease
// role that names a test clock. The Down puts back exactly the schema 00241
// left (columns, defaults, constraints, indexes, triggers, policies, grants,
// the lease roles and the billing-failure comment), and Up runs again.
//
// Mutation: a Down that leaves out an index, a constraint or the lease row
// turns this red.
func TestDropTestClockSchema_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_drop_test_clock_schema")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, dropTestClockSchemaVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", dropTestClockSchemaVersion, err)
	}
	state := func() string {
		return schemaOf(t, db, dropTestClockSchemaTables...) + "\n" + maRows(t, db,
			`SELECT string_agg(role, ' ' ORDER BY role) || ' / ' || col_description('subscriptions'::regclass,
			        (SELECT attnum FROM pg_attribute WHERE attrelid = 'subscriptions'::regclass AND attname = 'billing_failed_for'))
			   FROM leader_leases`)
	}
	before := state()

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if left := maRows(t, db, `
		SELECT table_name || '.' || column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND column_name IN ('test_clock_id', 'sim_effective_at', 'is_simulated')
		UNION ALL SELECT relname::text FROM pg_class
		 WHERE relnamespace = 'public'::regnamespace AND relname IN ('test_clocks', 'plan_price_version_clocks')
		UNION ALL SELECT role FROM leader_leases WHERE role = 'testclock_catchup'`); left != "" {
		t.Errorf("after the Up, still there: %s", left)
	}

	if _, err := down(ctx, dsn, migrationFiles(), dropTestClockSchemaVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", dropTestClockSchemaVersion-1, err)
	}
	if after := state(); after != before {
		t.Errorf("the Down did not put back the schema 00241 left:\n--- before 00242\n%s\n--- after the Down\n%s", before, after)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
