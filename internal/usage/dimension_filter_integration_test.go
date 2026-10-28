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

// TestListAndAggregate_DimensionFilter is the real-Postgres proof that the
// `dimensions` filter actually filters. The dashboard sent the param since
// the UsageEvents page shipped; the server ignored it and rendered
// unfiltered data as filtered — worse than no filter. The clause is JSONB
// containment (`properties @> …`), so it must AND across multiple pairs
// and honor value types (string vs boolean) exactly as ingest stored them.
func TestListAndAggregate_DimensionFilter(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Dim Filter")
	customerID := insertTestCustomer(t, db, tenantID, "cus_dim_filter")
	meterID := insertTestMeter(t, db, tenantID, "mtr_dim_filter", "tokens_dim")

	store := usage.NewPostgresStore(db)
	svc := usage.NewService(store)
	ingest := func(qty int64, dims map[string]any) {
		t.Helper()
		if _, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
			CustomerID: customerID, MeterID: meterID,
			Quantity: decimal.NewFromInt(qty), Dimensions: dims,
		}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	ingest(10, map[string]any{"model": "gpt-4o", "token_type": "input", "cached": true})
	ingest(20, map[string]any{"model": "gpt-4o", "token_type": "output"})
	ingest(30, map[string]any{"model": "claude-3.5", "token_type": "input"})
	ingest(40, nil)

	list := func(dims map[string]any) int {
		t.Helper()
		events, _, err := store.List(ctx, usage.ListFilter{
			TenantID: tenantID, CustomerID: customerID, Dimensions: dims,
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return len(events)
	}

	if got := list(map[string]any{"model": "gpt-4o"}); got != 2 {
		t.Errorf("model=gpt-4o: got %d events, want 2", got)
	}
	// Multi-pair = AND (one containment doc).
	if got := list(map[string]any{"model": "gpt-4o", "token_type": "input"}); got != 1 {
		t.Errorf("model=gpt-4o AND token_type=input: got %d, want 1", got)
	}
	// Boolean typing: true (JSON literal) matches; the STRING "true" must not.
	if got := list(map[string]any{"cached": true}); got != 1 {
		t.Errorf("cached=true (boolean): got %d, want 1", got)
	}
	if got := list(map[string]any{"cached": "true"}); got != 0 {
		t.Errorf("cached=\"true\" (string): got %d, want 0 — typed containment must not string-match a boolean", got)
	}
	if got := list(map[string]any{"model": "nonexistent"}); got != 0 {
		t.Errorf("no-match filter: got %d, want 0", got)
	}
	if got := list(nil); got != 4 {
		t.Errorf("no filter: got %d, want all 4", got)
	}

	// Aggregate rides the same WHERE builder — the stat cards must agree
	// with the events table on scope.
	agg, err := store.Aggregate(ctx, usage.ListFilter{
		TenantID: tenantID, CustomerID: customerID,
		Dimensions: map[string]any{"model": "gpt-4o"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.TotalEvents != 2 {
		t.Errorf("aggregate total_events: got %d, want 2", agg.TotalEvents)
	}
}
