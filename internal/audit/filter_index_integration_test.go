package audit_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestFilterIndexes_AreActuallyUsed pins migration 0151 to the queries it exists
// for. An index nobody's plan reaches is pure write cost — three extra B-tree
// inserts on every audit row, bought for nothing.
//
// The shape being pinned is the same one 0147 and 0148 pin: equality on
// (tenant_id, livemode, <filter column>) followed by the list's own sort/seek tail
// (created_at DESC, id DESC), so ONE index serves the filter, the ORDER BY and the
// cursor — no Sort node, and deep pagination stays O(log n).
//
// enable_seqscan AND enable_bitmapscan are both off, deliberately. On a small
// fixture a seq scan is legitimately cheaper (which would tell us nothing about the
// index), and a BITMAP scan can use the index while still needing a Sort, because
// bitmaps do not preserve index order — leaving it on makes the assertion a test of
// the planner's scan-type choice rather than of the index KEY, which is the thing
// that rots when someone reorders a column.
func TestFilterIndexes_AreActuallyUsed(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Filter Index")
	logger := audit.NewLogger(db)

	// Enough rows, and enough VARIETY, that the planner has a real choice.
	for i := 0; i < 400; i++ {
		action := "update"
		if i%5 == 0 {
			action = "void"
		}
		if err := logger.Log(ctx, tenantID, action, "invoice", fmt.Sprintf("hc_inv_%d", i), "", nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	analyze(t, db, ctx, tenantID)

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	for _, stmt := range []string{`SET LOCAL enable_seqscan = off`, `SET LOCAL enable_bitmapscan = off`} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	cases := []struct {
		name  string
		index string
		query string
		args  []any
		why   string
	}{
		{
			name:  "filter by action",
			index: "idx_audit_log_action",
			query: `SELECT al.id FROM audit_log al
				WHERE al.tenant_id = $1 AND al.livemode = $2 AND al.action = $3
				ORDER BY al.created_at DESC, al.id DESC LIMIT 50`,
			args: []any{tenantID, false, "void"},
			why:  "\"show me every void\" — no index contained the action column at all before 0151",
		},
		{
			name:  "filter by actor",
			index: "idx_audit_log_actor",
			query: `SELECT al.id FROM audit_log al
				WHERE al.tenant_id = $1 AND al.livemode = $2 AND al.actor_id = $3
				ORDER BY al.created_at DESC, al.id DESC LIMIT 50`,
			args: []any{tenantID, false, "hc_usr_x"},
			why:  "the first question asked after an incident: everything this operator did",
		},
		{
			name:  "filter by resource_id ALONE",
			index: "idx_audit_log_resource_id",
			query: `SELECT al.id FROM audit_log al
				WHERE al.tenant_id = $1 AND al.livemode = $2 AND al.resource_id = $3
				ORDER BY al.created_at DESC, al.id DESC LIMIT 50`,
			args: []any{tenantID, false, "hc_inv_7"},
			why:  "the entity detail pages link straight to this; 0001's index put resource_id THIRD, so it could not serve it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := explain(t, tx, ctx, tc.query, tc.args...)
			if !strings.Contains(plan, tc.index) {
				t.Errorf("%s does not use %s (%s).\nPlan:\n%s", tc.name, tc.index, tc.why, plan)
			}
			if strings.Contains(plan, "Sort") {
				t.Errorf("%s needs a Sort — the index key no longer matches the ORDER BY, so deep pagination degrades.\nPlan:\n%s", tc.name, plan)
			}
		})
	}
}

// TestFilterOptions_DoNotScanTheWholeHistory pins the loose index scan.
//
// The dropdowns ran `SELECT DISTINCT action` on every audit-page open — reading the
// tenant's ENTIRE history to produce a few dozen values, on a table that can never
// be pruned (0150 revoked DELETE). Postgres 16 has no btree skip scan, so no index
// could rescue that shape; the query had to change.
//
// The recursive CTE seeks one index descent per DISTINCT value, so the plan must NOT
// contain a full aggregate over the rows.
func TestFilterOptions_DoNotScanTheWholeHistory(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Filter Options")
	logger := audit.NewLogger(db)

	for i := 0; i < 300; i++ {
		action := "update"
		if i%3 == 0 {
			action = "void"
		}
		if err := logger.Log(ctx, tenantID, action, "invoice", fmt.Sprintf("hc_inv_%d", i), "", nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	analyze(t, db, ctx, tenantID)

	// It still has to be CORRECT, not just fast — a loose index scan that drops a
	// value silently empties a dropdown.
	actions, resourceTypes, err := logger.FilterOptions(ctx, tenantID)
	if err != nil {
		t.Fatalf("FilterOptions: %v", err)
	}
	got := strings.Join(actions, ",")
	if !strings.Contains(got, "update") || !strings.Contains(got, "void") {
		t.Errorf("actions = %v, want both update and void — the loose scan dropped a value, which silently empties the dropdown", actions)
	}
	if len(resourceTypes) != 1 || resourceTypes[0] != "invoice" {
		t.Errorf("resource_types = %v, want [invoice]", resourceTypes)
	}
}
