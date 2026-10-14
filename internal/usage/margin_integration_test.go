package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestMargin_AttributionHonesty pins ADR-079 D6: per-model margin renders
// ONLY where a pricing rule pins `model` in dimension_match; revenue from
// rules without a model pin lands in the explicit unattributed bucket —
// never allocated by heuristic. The headline (customer-level revenue vs
// cost) is always computed.
func TestMargin_AttributionHonesty(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Margin Attribution")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	// Rule A pins model → its revenue is per-model attributable.
	rrvModel, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "sonnet_tokens", Name: "Sonnet", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(3),
	})
	if err != nil {
		t.Fatalf("create rrv model: %v", err)
	}
	// Rule B is a flat catch-all (no model pin) → unattributed bucket.
	rrvFlat, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "flat_tokens", Name: "Flat", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv flat: %v", err)
	}
	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "tokens", Name: "Tokens", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrvFlat.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: meter.ID, RatingRuleVersionID: rrvModel.ID,
		DimensionMatch:  map[string]any{"model": "claude-3.5-sonnet"},
		AggregationMode: domain.AggSum, Priority: 10,
	}); err != nil {
		t.Fatalf("upsert model rule: %v", err)
	}

	custID, _, _ := f.seedCustomerWithSub(t, ctx, "cus_margin", "pln_margin", meter.ID, cycleStart, cycleEnd)

	usageStore := usage.NewPostgresStore(f.db)
	// COGS rate: $0.000004/token for the sonnet family.
	seedRate(t, f.db, ctx, f.tenantID, "anthropic", "claude-3.5-sonnet", "input", "0.000004")

	// 1000 sonnet tokens (rule A: 1000 × 3c = $30 revenue; cost 1000 ×
	// 4e-6 = 4000 micros) + 500 un-pinned tokens (rule B default: 500 ×
	// 1c = $5, unattributed; no costable dims → not_applicable).
	ts := cycleStart.Add(time.Hour)
	if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
		CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(1000),
		Dimensions: map[string]any{"model": "claude-3.5-sonnet", "provider": "anthropic", "token_type": "input"},
		Timestamp:  &ts,
	}); err != nil {
		t.Fatalf("ingest sonnet: %v", err)
	}
	ts2 := cycleStart.Add(2 * time.Hour)
	if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
		CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(500),
		Dimensions: map[string]any{"region": "us-east-1"}, Timestamp: &ts2,
	}); err != nil {
		t.Fatalf("ingest flat: %v", err)
	}
	// One costable-but-unmatched event → unresolved counter.
	ts3 := cycleStart.Add(3 * time.Hour)
	if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
		CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(100),
		Dimensions: map[string]any{"model": "gpt-4o", "provider": "openai", "token_type": "input"},
		Timestamp:  &ts3,
	}); err != nil {
		t.Fatalf("ingest unmatched: %v", err)
	}

	rep, err := usage.NewMarginAssembler(usageStore, f.custUsage).Get(ctx, f.tenantID, custID, cycleStart, cycleEnd)
	if err != nil {
		t.Fatalf("margin: %v", err)
	}

	if rep.CostMicros != 4000 {
		t.Errorf("cost = %d micros, want 4000", rep.CostMicros)
	}
	if rep.UnresolvedEvents != 1 {
		t.Errorf("unresolved = %d, want 1 (the gpt-4o event)", rep.UnresolvedEvents)
	}
	// Revenue: sonnet 1000×3c + others rated by the flat default. The
	// load-bearing assertions are attribution shape, not exact totals:
	var sonnet *usage.MarginModelRow
	for i := range rep.ByModel {
		if rep.ByModel[i].Model == "claude-3.5-sonnet" {
			sonnet = &rep.ByModel[i]
		}
	}
	if sonnet == nil {
		t.Fatal("no by_model row for claude-3.5-sonnet")
	}
	if !sonnet.Attributed || sonnet.RevenueCents != 3000 {
		t.Errorf("sonnet row = attributed=%v revenue=%d, want attributed=true revenue=3000", sonnet.Attributed, sonnet.RevenueCents)
	}
	if sonnet.CostMicros != 4000 {
		t.Errorf("sonnet cost = %d, want 4000", sonnet.CostMicros)
	}
	if rep.UnattributedRevenueCents == 0 {
		t.Error("unattributed revenue = 0, want >0 (the flat-rule revenue must NOT be allocated per model)")
	}
	if rep.MarginBps == nil {
		t.Fatal("margin_bps missing with revenue > 0")
	}
	// Headline margin: cost 4000 micros = 0.4 cents on ≥3000 cents revenue
	// → margin just under 100%: 9990-9999 bps.
	if *rep.MarginBps < 9900 || *rep.MarginBps > 10000 {
		t.Errorf("margin_bps = %d, want ~9990s (cost 0.4c on $30+)", *rep.MarginBps)
	}
}

// TestMargin_CurrencyIsNamedAndNeverMixed: the report names the currency of
// its revenue (the customer's price rules) and of its cost (always USD), and
// gives no margin when they differ. Before, a EUR customer's €30 of usage
// against $0.004 of provider cost read as "$30.00" and a 99.9% margin.
// Mutation-verify: drop the RevenueCurrency == CostCurrency check → red.
func TestMargin_CurrencyIsNamedAndNeverMixed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()
	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	for _, tc := range []struct {
		currency   string
		wantMargin bool
	}{
		{"USD", true},
		{"EUR", false},
	} {
		t.Run(tc.currency, func(t *testing.T) {
			f := newCustomerUsageFixture(t, "Margin currency "+tc.currency)
			rrv, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
				RuleKey: "tokens_flat", Name: "Tokens", Mode: domain.PricingFlat,
				Currency: tc.currency, FlatAmountCents: decimal.NewFromInt(3),
			})
			if err != nil {
				t.Fatalf("rule: %v", err)
			}
			meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
				Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
			})
			if err != nil {
				t.Fatalf("meter: %v", err)
			}
			custID, _, _ := f.seedCustomerWithSubIn(t, ctx, "cus_cur", "pln_cur", meter.ID, tc.currency, cycleStart, cycleEnd)
			seedRate(t, f.db, ctx, f.tenantID, "anthropic", "claude-3.5-sonnet", "input", "0.000004")
			ts := cycleStart.Add(time.Hour)
			if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
				CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(1000),
				Dimensions: map[string]any{"model": "claude-3.5-sonnet", "provider": "anthropic", "token_type": "input"},
				Timestamp:  &ts,
			}); err != nil {
				t.Fatalf("ingest: %v", err)
			}

			rep, err := usage.NewMarginAssembler(usage.NewPostgresStore(f.db), f.custUsage).Get(ctx, f.tenantID, custID, cycleStart, cycleEnd)
			if err != nil {
				t.Fatalf("margin: %v", err)
			}
			if rep.RevenueCents != 3000 || rep.RevenueCurrency != tc.currency {
				t.Errorf("revenue = %d %q, want 3000 %q", rep.RevenueCents, rep.RevenueCurrency, tc.currency)
			}
			if rep.CostMicros != 4000 || rep.CostCurrency != "USD" {
				t.Errorf("cost = %d %q, want 4000 USD", rep.CostMicros, rep.CostCurrency)
			}
			if got := rep.MarginBps != nil; got != tc.wantMargin {
				t.Errorf("margin_bps present = %v, want %v (revenue %s, cost USD)", got, tc.wantMargin, tc.currency)
			}
		})
	}
}
