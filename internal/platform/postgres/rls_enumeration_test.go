package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// rlsFenceAllowlist names tables that carry a tenant_id column but are
// DELIBERATELY not RLS-fenced, each with the written reason. Ships EMPTY —
// that is the goal state. Any future entry must carry a reason (and ideally an
// ADR/comment reference), so an exception becomes an explicit reviewed decision
// instead of a silent omission. A stale entry (table gone, or fenced after all)
// fails the test so the list can't rot.
var rlsFenceAllowlist = map[string]string{}

// TestRLSIsolation_EveryTenantTableIsFenced discovers EVERY table carrying a
// tenant_id column from information_schema — no hand-maintained table list —
// and asserts each is fully RLS-fenced: ENABLE + FORCE + a tenant_isolation
// policy. This converts three past incident classes from silent to CI-red:
//
//   - a brand-new tenant table with NO RLS at all (dashboard_sessions, created
//     in 0066, unfenced until 0124 — invisible to the existing hardcoded-list
//     tests precisely because nobody remembered to list it);
//   - ENABLE without FORCE, which still exempts the table OWNER — the 0111
//     stripe_provider_credentials gap (self-managed deployments often connect
//     as the owning role, silently bypassing RLS);
//   - a missing tenant_isolation policy (the 0113 feature_flag_overrides gap).
//
// The hardcoded-list tests in rls_isolation_test.go stay: they verify the
// policies BEHAVE (cross-tenant reads blocked, livemode partitioning). This
// test verifies the fence EXISTS everywhere it must.
func TestRLSIsolation_EveryTenantTableIsFenced(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT c.table_name
		FROM information_schema.columns c
		JOIN information_schema.tables tb
		  ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		WHERE c.table_schema = 'public'
		  AND c.column_name  = 'tenant_id'
		  AND tb.table_type  = 'BASE TABLE'
		ORDER BY c.table_name`)
	if err != nil {
		t.Fatalf("discover tenant_id tables: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}

	// Vacuity guard: a broken discovery query returning nothing would pass
	// every per-table assertion below by never running one. The schema has
	// ~50 tenant tables; anything under 30 means discovery itself broke.
	if len(tables) < 30 {
		t.Fatalf("discovered only %d tenant_id tables — the discovery query is broken (expected 30+): %v", len(tables), tables)
	}

	seen := map[string]bool{}
	for _, table := range tables {
		seen[table] = true
		if reason, ok := rlsFenceAllowlist[table]; ok {
			t.Logf("allowlisted (deliberately unfenced): %s — %s", table, reason)
			continue
		}

		var relrowsecurity, relforcerowsecurity bool
		if err := tx.QueryRowContext(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class
			 WHERE relname = $1 AND relnamespace = 'public'::regnamespace`,
			table,
		).Scan(&relrowsecurity, &relforcerowsecurity); err != nil {
			t.Errorf("%s: read pg_class: %v", table, err)
			continue
		}
		if !relrowsecurity {
			t.Errorf("%s: carries tenant_id but ROW LEVEL SECURITY is not ENABLED — add the 0006-pattern fence (ENABLE + FORCE + tenant_isolation) in the table's migration, or allowlist it here with a written reason", table)
		}
		if !relforcerowsecurity {
			t.Errorf("%s: RLS is ENABLED but not FORCED — the table OWNER bypasses the policy (the 0111 stripe_provider_credentials gap); add FORCE ROW LEVEL SECURITY", table)
		}

		var policyCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_policies
			 WHERE schemaname = 'public' AND tablename = $1 AND policyname = 'tenant_isolation'`,
			table,
		).Scan(&policyCount); err != nil {
			t.Errorf("%s: read pg_policies: %v", table, err)
			continue
		}
		if policyCount != 1 {
			t.Errorf("%s: expected exactly one tenant_isolation policy, got %d (the 0113 feature_flag_overrides gap)", table, policyCount)
		}
	}

	// Keep the allowlist honest: an entry for a table that no longer exists
	// (or was renamed) is stale documentation and must be removed.
	for table := range rlsFenceAllowlist {
		if !seen[table] {
			t.Errorf("allowlist entry %q matches no tenant_id table — remove the stale entry", table)
		}
	}
}
