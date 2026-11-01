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

// TestProviderCostStamp_ProviderAndTokenTypeAnyCase: PUT /v1/provider-costs
// stores provider and token_type in lower case, whatever the operator typed.
// The ingest stamp compared the event's labels to them as sent, so usage sent
// with "provider": "Anthropic" or "token_type": "Input" never matched a rate
// and was never costed: its cost was missing from the margin report for good,
// and no rate the operator could save would match it.
//
// Mutation: compare the event's provider (or token_type) without lower(),
// and the event is left unresolved.
func TestProviderCostStamp_ProviderAndTokenTypeAnyCase(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Provider Cost Case")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_case", DisplayName: "Case"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	meter, err := pricing.NewPostgresStore(db).CreateMeter(ctx, tenantID, domain.Meter{
		EventName: "tokens", Name: "Tokens", Aggregation: "sum", Unit: "tokens",
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	// As the save path stores it (provider and token_type lower-cased).
	seedRate(t, db, ctx, tenantID, "anthropic", "claude-sonnet-4.5", "input", "0.0003")
	store := usage.NewPostgresStore(db)

	for _, dims := range []map[string]any{
		{"provider": "Anthropic", "model": "claude-sonnet-4.5", "token_type": "input"},
		{"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "Input"},
		{"provider": "ANTHROPIC", "model": "claude-sonnet-4.5", "token_type": "INPUT"},
	} {
		ev, err := store.Ingest(ctx, tenantID, domain.UsageEvent{
			CustomerID: cust.ID, MeterID: meter.ID, Quantity: decimal.NewFromInt(1000),
			Dimensions: dims, Timestamp: time.Now(),
		})
		if err != nil {
			t.Fatalf("ingest %v: %v", dims, err)
		}
		// 1000 tokens × 0.0003 cents = 0.3 cents = 3000 micros.
		if ev.ProviderCostSource != "table" || ev.ProviderCostMicros == nil || *ev.ProviderCostMicros != 3000 {
			t.Errorf("%v: stamp = %v/%q, want 3000/table", dims, ev.ProviderCostMicros, ev.ProviderCostSource)
		}
		// The labels are stored as sent.
		if ev.Dimensions["provider"] != dims["provider"] || ev.Dimensions["token_type"] != dims["token_type"] {
			t.Errorf("%v: stored dims = %v, want the labels as sent", dims, ev.Dimensions)
		}
	}
}
