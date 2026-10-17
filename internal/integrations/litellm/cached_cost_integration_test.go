package litellm

import (
	"context"
	"net/http"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestSpend_CachedCallCostOnRealStore sends one cached Anthropic call through
// the real handler and stores, with a rate-table row for every token type,
// and reads back what the ingest INSERT stamped. Before the fix the input
// event carried LiteLLM's whole input_cost (cache reads included) and the
// cache_read event was costed again from the table: 12000 + 1500 (table) +
// 1500 = 15000 micros for a call LiteLLM priced at 13500.
func TestSpend_CachedCallCostOnRealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM cached cost")

	if _, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_x", DisplayName: "Cache Tester",
	}); err != nil {
		t.Fatalf("customer: %v", err)
	}
	prices := pricing.NewPostgresStore(db)
	if _, err := prices.CreateMeter(ctx, tenantID, domain.Meter{
		Key: MeterKeyTokens, Name: "Tokens", Aggregation: "sum", Unit: "tokens",
	}); err != nil {
		t.Fatalf("meter: %v", err)
	}
	store := usage.NewPostgresStore(db)
	// The same rates LiteLLM used, so the table and the observed figures
	// agree and the only difference is whether a token is counted twice.
	for tt, rate := range map[string]string{
		TokenTypeInput: "0.000003", TokenTypeCacheRead: "0.0000003", TokenTypeOutput: "0.000015",
	} {
		if _, err := store.UpsertProviderCostRate(ctx, tenantID, domain.ProviderCostRate{
			Provider: "anthropic", Model: "claude-sonnet-4.5", TokenType: tt, CostPerToken: decimal.RequireFromString(rate), Currency: "USD",
		}); err != nil {
			t.Fatalf("rate %s: %v", tt, err)
		}
	}

	svc := usage.NewService(store)
	h := New(customer.NewPostgresStore(db), pricing.NewService(prices), svc, svc)
	stamped := func(t *testing.T, id string) (int64, map[string]string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		rows, err := tx.QueryContext(ctx, `
			SELECT properties->>'token_type', provider_cost_micros, COALESCE(provider_cost_source, '')
			FROM usage_events WHERE idempotency_key LIKE $1 || ':%'`, id)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var sum int64
		sources := map[string]string{}
		for rows.Next() {
			var tt, src string
			var micros *int64
			if err := rows.Scan(&tt, &micros, &src); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if micros == nil {
				t.Fatalf("%s: no cost stamped", tt)
			}
			sum += *micros
			sources[tt] = src
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return sum, sources
	}

	t.Run("with cache_read_cost every event is observed", func(t *testing.T) {
		p := cachedAnthropicCall()
		if code, body := postSpendWithCtx(t, h, ctx, tenantID, p); code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
		sum, sources := stamped(t, p.ID)
		if sum != 13500 {
			t.Errorf("stamped cost = %d micros, want 13500 (LiteLLM's total_cost)", sum)
		}
		for _, tt := range []string{TokenTypeInput, TokenTypeCacheRead, TokenTypeOutput} {
			if sources[tt] != "observed" {
				t.Errorf("%s source = %q, want observed", tt, sources[tt])
			}
		}
	})

	t.Run("without cache_read_cost the input side uses the table", func(t *testing.T) {
		p := cachedAnthropicCall()
		p.ID = "chatcmpl-cache-nocr"
		p.CostBreakdown.CacheReadCost = nil
		if code, body := postSpendWithCtx(t, h, ctx, tenantID, p); code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
		sum, sources := stamped(t, p.ID)
		// Table: 3000 × $3/M + 5000 × $0.30/M = 9000 + 1500; output observed 1500.
		// (The table prices the 2,000 cache-write tokens at the input rate.)
		if sum != 12000 {
			t.Errorf("stamped cost = %d micros, want 12000", sum)
		}
		if sources[TokenTypeInput] != "table" || sources[TokenTypeCacheRead] != "table" || sources[TokenTypeOutput] != "observed" {
			t.Errorf("sources = %v, want input/cache_read table and output observed", sources)
		}
	})
}
