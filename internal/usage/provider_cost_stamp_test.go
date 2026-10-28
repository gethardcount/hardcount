package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// seedRate inserts a provider_cost_rates row through a tenant-scoped tx so
// RLS + the set_livemode trigger run exactly as production writes will.
func seedRate(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, provider, model, tokenType, unitCostCents string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO provider_cost_rates (tenant_id, provider, model, token_type, unit_cost_cents)
		VALUES ($1, $2, $3, $4, $5::numeric)
	`, tenantID, provider, model, tokenType, unitCostCents); err != nil {
		t.Fatalf("seed rate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestProviderCostStamp pins ADR-079 D2/D3: the ingest INSERT stamps
// provider_cost_micros from the rate in force at ingest, with model_raw
// exact-match beating the family fallback, honest NULL for unmatched token
// events, and 'not_applicable' for events with no costable dims. Uses
// mapper-shaped dims (the panel's rule: test against what LiteLLM actually
// stamps, not hand-invented keys).
func TestProviderCostStamp(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Provider Cost Stamp")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_cogs", DisplayName: "COGS Tester",
	})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	meter, err := pricing.NewPostgresStore(db).CreateMeter(ctx, tenantID, domain.Meter{
		EventName: "tokens", Name: "Tokens", Aggregation: "sum", Unit: "tokens",
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	store := usage.NewPostgresStore(db)

	// Family-keyed rate + a raw-id-keyed rate at a DIFFERENT price: the
	// raw key must win for events carrying that snapshot id.
	seedRate(t, db, ctx, tenantID, "anthropic", "claude-3.5-sonnet", "input", "0.0003")
	seedRate(t, db, ctx, tenantID, "anthropic", "claude-3-5-sonnet-20241022", "input", "0.0004")

	ingest := func(dims map[string]any, qty int64) domain.UsageEvent {
		t.Helper()
		ev, err := store.Ingest(ctx, tenantID, domain.UsageEvent{
			CustomerID: cust.ID, MeterID: meter.ID,
			Quantity: decimal.NewFromInt(qty), Dimensions: dims,
			Timestamp: time.Now(),
		})
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		return ev
	}

	// 1. model_raw exact match wins over the family row: 1000 tokens ×
	//    0.000004 $/token = $0.004 = 4000 micros.
	ev := ingest(map[string]any{
		"provider": "anthropic", "model": "claude-3.5-sonnet",
		"model_raw": "claude-3-5-sonnet-20241022", "token_type": "input",
	}, 1000)
	if ev.ProviderCostSource != "table" || ev.ProviderCostMicros == nil || *ev.ProviderCostMicros != 4000 {
		t.Fatalf("raw-key stamp = %v/%s, want 4000/table", ev.ProviderCostMicros, ev.ProviderCostSource)
	}

	// 2. Family fallback when model_raw has no row of its own.
	ev = ingest(map[string]any{
		"provider": "anthropic", "model": "claude-3.5-sonnet",
		"model_raw": "claude-3-5-sonnet-20250601", "token_type": "input",
	}, 1000)
	if ev.ProviderCostMicros == nil || *ev.ProviderCostMicros != 3000 {
		t.Fatalf("family fallback stamp = %v, want 3000", ev.ProviderCostMicros)
	}

	// 3. Costable dims, no matching rate → NULL micros, empty source
	//    ('unresolved' — the actionable add-a-rate signal).
	ev = ingest(map[string]any{
		"provider": "openai", "model": "gpt-4o", "token_type": "input",
	}, 500)
	if ev.ProviderCostMicros != nil || ev.ProviderCostSource != "" {
		t.Fatalf("unresolved = %v/%q, want nil/empty", ev.ProviderCostMicros, ev.ProviderCostSource)
	}

	// 4. No costable dims (a non-token meter event) → not_applicable, so
	//    the honesty counter isn't drowned by structurally-uncostable rows.
	ev = ingest(map[string]any{"region": "us-east-1"}, 42)
	if ev.ProviderCostSource != "not_applicable" || ev.ProviderCostMicros != nil {
		t.Fatalf("not_applicable = %v/%q, want nil/'not_applicable'", ev.ProviderCostMicros, ev.ProviderCostSource)
	}

	// 5. Snapshot semantics: editing the rate does NOT rewrite the stamp.
	{
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE provider_cost_rates SET unit_cost_cents = 0.0009
			WHERE tenant_id = $1 AND model = 'claude-3-5-sonnet-20241022'
		`, tenantID); err != nil {
			t.Fatalf("edit rate: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	// The earlier event keeps 4000; a NEW event gets the new rate.
	fresh := ingest(map[string]any{
		"provider": "anthropic", "model": "claude-3.5-sonnet",
		"model_raw": "claude-3-5-sonnet-20241022", "token_type": "input",
	}, 1000)
	if fresh.ProviderCostMicros == nil || *fresh.ProviderCostMicros != 9000 {
		t.Fatalf("post-edit stamp = %v, want 9000", fresh.ProviderCostMicros)
	}

	// 6. Fractional rounding: 333 tokens × 0.0004¢ = 1332 micros exactly;
	//    and a half-case rounds up (ROUND half away from zero):
	//    125 tokens × 0.0004¢ = 500 micros — pick a case with a .5:
	//    0.0003¢/tok family rate × 500 tokens = 1500 micros (exact).
	//    Sub-micro: 1 token × 0.0003¢ = 3 micros. 1 token at a rate of
	//    0.00005¢ would be 0.5 → 1 micro (half-up).
	seedRate(t, db, ctx, tenantID, "anthropic", "claude-tiny", "input", "0.00005")
	ev = ingest(map[string]any{
		"provider": "anthropic", "model": "claude-tiny", "token_type": "input",
	}, 1)
	if ev.ProviderCostMicros == nil || *ev.ProviderCostMicros != 1 {
		t.Fatalf("half-up rounding = %v, want 1 micro", ev.ProviderCostMicros)
	}
}

// TestProviderCostStamp_LivemodeIsolation pins the panel's fold 9: rate rows
// are livemode-scoped via the set_livemode trigger + RLS policy — a rate
// entered in test mode must not cost live-mode events.
func TestProviderCostStamp_LivemodeIsolation(t *testing.T) {
	db := testutil.SetupTestDB(t)
	testCtx := postgres.WithLivemode(context.Background(), false)
	liveCtx := postgres.WithLivemode(context.Background(), true)

	tenantID := testutil.CreateTestTenant(t, db, "COGS Livemode")
	cust, err := customer.NewPostgresStore(db).Create(testCtx, tenantID, domain.Customer{
		ExternalID: "cus_cogs_mode", DisplayName: "Mode Tester",
	})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	meter, err := pricing.NewPostgresStore(db).CreateMeter(testCtx, tenantID, domain.Meter{
		EventName: "tokens", Name: "Tokens", Aggregation: "sum", Unit: "tokens",
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}

	// Rate created under TEST mode (trigger stamps livemode=false).
	seedRate(t, db, testCtx, tenantID, "openai", "gpt-4o", "input", "0.0002")

	store := usage.NewPostgresStore(db)
	dims := map[string]any{"provider": "openai", "model": "gpt-4o", "token_type": "input"}

	// Test-mode event: costed.
	ev, err := store.Ingest(testCtx, tenantID, domain.UsageEvent{
		CustomerID: cust.ID, MeterID: meter.ID,
		Quantity: decimal.NewFromInt(1000), Dimensions: dims, Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("test-mode ingest: %v", err)
	}
	if ev.ProviderCostMicros == nil || *ev.ProviderCostMicros != 2000 {
		t.Fatalf("test-mode stamp = %v, want 2000", ev.ProviderCostMicros)
	}

	// Live-mode event: the test-mode rate must be invisible → unresolved.
	ev, err = store.Ingest(liveCtx, tenantID, domain.UsageEvent{
		CustomerID: cust.ID, MeterID: meter.ID,
		Quantity: decimal.NewFromInt(1000), Dimensions: dims, Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatalf("live-mode ingest: %v", err)
	}
	if ev.ProviderCostMicros != nil {
		t.Fatalf("live-mode stamp = %v, want nil (test rate must not leak)", *ev.ProviderCostMicros)
	}
}
