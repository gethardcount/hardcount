package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// TestUsageEvents_DimensionsPersistAndJSONBSupersetMatch is the
// end-to-end contract test for FEAT-week2 multi-dim meters. It writes
// usage events with dimensions on the properties JSONB column, then
// verifies:
//
//  1. Dimensions round-trip through the store (JSON marshal/scan).
//  2. The Postgres `@>` superset operator finds events whose properties
//     are a superset of the filter — this is the operator pricing-rule
//     resolution will use at finalize time.
//  3. The GIN index created in migration 0054 is the access path the
//     planner picks for `@>` queries (sanity check that the index
//     wasn't accidentally pruned).
//
// Without this test, a future schema change (e.g. dropping the GIN
// index, switching JSONB → JSON) could silently regress the rule-
// dispatch hot path.
func TestUsageEvents_DimensionsPersistAndJSONBSupersetMatch(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := usage.NewPostgresStore(db)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Dimensions Test")
	customerID := insertTestCustomer(t, db, tenantID, "cus_dim_1")
	meterID := insertTestMeter(t, db, tenantID, "mtr_dim_1", "tokens")

	type evt struct {
		props map[string]any
		qty   int64
	}
	events := []evt{
		{props: map[string]any{"model": "gpt-4", "operation": "input", "cached": false}, qty: 100},
		{props: map[string]any{"model": "gpt-4", "operation": "input", "cached": true}, qty: 50},
		{props: map[string]any{"model": "gpt-4", "operation": "output", "cached": false}, qty: 25},
		{props: map[string]any{"model": "claude-3", "operation": "input", "cached": false}, qty: 200},
	}

	svc := usage.NewService(store)
	for i, e := range events {
		if _, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
			CustomerID: customerID, MeterID: meterID,
			Quantity: decimal.NewFromInt(e.qty), Dimensions: e.props,
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	// Direct JSONB superset query — the same shape pricing-rule
	// resolution will use at finalize time. {model:gpt-4} should
	// match the first three events but not the claude-3 one.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	var count int
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM usage_events
		 WHERE meter_id = $1
		   AND properties @> '{"model":"gpt-4"}'::jsonb
	`, meterID).Scan(&count)
	if err != nil {
		t.Fatalf("count gpt-4: %v", err)
	}
	if count != 3 {
		t.Errorf("model=gpt-4 superset match: got %d, want 3", count)
	}

	// Refine the filter — both model and operation. Should match the
	// two input events but not the output.
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM usage_events
		 WHERE meter_id = $1
		   AND properties @> '{"model":"gpt-4","operation":"input"}'::jsonb
	`, meterID).Scan(&count)
	if err != nil {
		t.Fatalf("count gpt-4 input: %v", err)
	}
	if count != 2 {
		t.Errorf("model=gpt-4 op=input superset match: got %d, want 2", count)
	}

	// Empty filter '{}' matches every event (this is what the default
	// rule will use).
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM usage_events
		 WHERE meter_id = $1
		   AND properties @> '{}'::jsonb
	`, meterID).Scan(&count)
	if err != nil {
		t.Fatalf("count all: %v", err)
	}
	if count != len(events) {
		t.Errorf("empty filter (default rule): got %d, want %d", count, len(events))
	}

	// Sanity-check the GIN index exists and is named as the migration
	// declared. We don't assert it's used (planner choice depends on
	// row count) — just that schema state matches the contract.
	var idxName string
	err = tx.QueryRowContext(ctx, `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = current_schema()
		   AND tablename = 'usage_events'
		   AND indexname = 'idx_usage_events_properties_gin'
	`).Scan(&idxName)
	if err != nil {
		t.Fatalf("GIN index missing — was migration 0054 reverted? err=%v", err)
	}
}
