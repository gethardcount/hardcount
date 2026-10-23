package pricing_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

func TestPostgresStore_RatingRules(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	// Create flat rule
	rule, err := store.CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "api_calls", Name: "API Calls", Version: 1,
		LifecycleState: domain.RatingRuleDraft, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(500),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if rule.ID == "" {
		t.Fatal("ID should be generated")
	}

	// Get
	got, err := store.GetRatingRule(ctx, tenantID, rule.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.FlatAmountCents.Equal(decimal.NewFromInt(500)) {
		t.Errorf("flat_amount: got %s", got.FlatAmountCents)
	}

	// Create graduated rule with tiers
	grad, err := store.CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "storage", Name: "Storage", Version: 1,
		LifecycleState: domain.RatingRuleActive, Mode: domain.PricingGraduated,
		Currency: "USD",
		GraduatedTiers: []domain.RatingTier{
			{UpTo: 100, UnitAmountCents: decimal.NewFromInt(10)},
			{UpTo: 0, UnitAmountCents: decimal.NewFromInt(5)},
		},
	})
	if err != nil {
		t.Fatalf("create graduated: %v", err)
	}

	// Verify tiers persisted
	gotGrad, _ := store.GetRatingRule(ctx, tenantID, grad.ID)
	if len(gotGrad.GraduatedTiers) != 2 {
		t.Fatalf("tiers: got %d, want 2", len(gotGrad.GraduatedTiers))
	}
	if gotGrad.GraduatedTiers[0].UpTo != 100 {
		t.Errorf("tier[0].up_to: got %d", gotGrad.GraduatedTiers[0].UpTo)
	}

	// List
	all, err := store.ListRatingRules(ctx, pricing.RatingRuleFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("list count: got %d, want 2", len(all))
	}

	// Version allocation happens in SQL (ADR-070): re-creating an
	// existing key ignores the caller's Version and lands as the key's
	// next version — a publish, never a unique violation.
	dup, err := store.CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "api_calls", Name: "Dup", Version: 1,
		LifecycleState: domain.RatingRuleDraft, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(10),
	})
	if err != nil {
		t.Fatalf("re-create existing key: %v", err)
	}
	if dup.Version != 2 {
		t.Errorf("re-created key version: got %d, want 2 (SQL MAX+1, caller's Version ignored)", dup.Version)
	}
}

func TestPostgresStore_Meters(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	meter, err := store.CreateMeter(ctx, tenantID, domain.Meter{
		Key: "api_calls", Name: "API Calls", Unit: "calls", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.GetMeter(ctx, tenantID, meter.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Key != "api_calls" {
		t.Errorf("key: got %q", got.Key)
	}

	// Unique key per tenant
	_, err = store.CreateMeter(ctx, tenantID, domain.Meter{
		Key: "api_calls", Name: "Dup", Unit: "x", Aggregation: "sum",
	})
	if err == nil {
		t.Fatal("expected unique violation for duplicate meter key")
	}

	// List
	meters, err := store.ListMeters(ctx, tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(meters) != 1 {
		t.Errorf("list: got %d, want 1", len(meters))
	}

	// RLS: different tenant can't see it
	tenant2 := testutil.CreateTestTenant(t, db, "Other")
	_, err = store.GetMeter(ctx, tenant2, meter.ID)
	if err != errs.ErrNotFound {
		t.Errorf("RLS: tenant2 should not see tenant1 meter, got %v", err)
	}
}

func TestPostgresStore_Plans(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	plan, err := store.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro", Name: "Pro Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 4900, MeterIDs: []string{"mtr_1", "mtr_2"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.GetPlan(ctx, tenantID, plan.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Code != "pro" {
		t.Errorf("code: got %q", got.Code)
	}
	if got.BaseAmountCents != 4900 {
		t.Errorf("base_amount: got %d", got.BaseAmountCents)
	}
	if len(got.MeterIDs) != 2 {
		t.Errorf("meter_ids: got %d, want 2", len(got.MeterIDs))
	}

	// Update
	got.Name = "Pro Plus"
	got.BaseAmountCents = 5900
	updated, err := store.UpdatePlan(ctx, tenantID, got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Pro Plus" {
		t.Errorf("updated name: got %q", updated.Name)
	}
	if updated.BaseAmountCents != 5900 {
		t.Errorf("updated base: got %d", updated.BaseAmountCents)
	}
}

// TestPostgresStore_MeterPricingRules covers the multi-dim meters
// foundation: the (meter, rating-rule) UNIQUE key drives upsert, the
// priority-DESC list ordering matches the runtime resolution order, the
// JSONB dimension_match round-trips, and RLS isolates tenants.
func TestPostgresStore_MeterPricingRules(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Multi-Dim Test")

	// Seed: one meter, two rating rules.
	rrv1, err := store.CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "tokens_default", Name: "Tokens default", Version: 1,
		LifecycleState: domain.RatingRuleActive, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("seed rrv1: %v", err)
	}
	rrv2, err := store.CreateRatingRule(ctx, tenantID, domain.RatingRuleVersion{
		RuleKey: "tokens_cached", Name: "Tokens cached", Version: 1,
		LifecycleState: domain.RatingRuleActive, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("seed rrv2: %v", err)
	}
	meter, err := store.CreateMeter(ctx, tenantID, domain.Meter{
		Key: "tokens", Name: "Tokens", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrv1.ID,
	})
	if err != nil {
		t.Fatalf("seed meter: %v", err)
	}

	// Insert default rule (no dimension match, low priority).
	defRule, err := store.UpsertMeterPricingRule(ctx, tenantID, domain.MeterPricingRule{
		MeterID: meter.ID, RatingRuleVersionID: rrv1.ID,
		DimensionMatch: map[string]any{}, AggregationMode: domain.AggSum, Priority: 0,
	}, nil)
	if err != nil {
		t.Fatalf("upsert default: %v", err)
	}
	if defRule.ID == "" || defRule.CreatedAt.IsZero() {
		t.Errorf("expected ID and created_at populated, got %+v", defRule)
	}

	// Insert specific rule, higher priority.
	specRule, err := store.UpsertMeterPricingRule(ctx, tenantID, domain.MeterPricingRule{
		MeterID: meter.ID, RatingRuleVersionID: rrv2.ID,
		DimensionMatch:  map[string]any{"model": "gpt-4", "cached": true},
		AggregationMode: domain.AggSum, Priority: 100,
	}, nil)
	if err != nil {
		t.Fatalf("upsert specific: %v", err)
	}

	// List ordering: priority DESC (specific first, default last).
	rules, err := store.ListMeterPricingRulesByMeter(ctx, tenantID, meter.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	if rules[0].ID != specRule.ID {
		t.Errorf("priority ordering: got %s first, want %s (specific)", rules[0].ID, specRule.ID)
	}
	if rules[1].ID != defRule.ID {
		t.Errorf("priority ordering: got %s last, want %s (default)", rules[1].ID, defRule.ID)
	}
	// Dimension match round-trip via JSONB.
	if got := rules[0].DimensionMatch["model"]; got != "gpt-4" {
		t.Errorf("dimension_match round-trip: got %v, want gpt-4", got)
	}
	if got := rules[0].DimensionMatch["cached"]; got != true {
		t.Errorf("dimension_match bool round-trip: got %v, want true", got)
	}

	// Upsert idempotency: re-issuing the same (meter, rrv) pair updates
	// the same row instead of inserting a duplicate.
	updated, err := store.UpsertMeterPricingRule(ctx, tenantID, domain.MeterPricingRule{
		MeterID: meter.ID, RatingRuleVersionID: rrv2.ID,
		DimensionMatch:  map[string]any{"model": "gpt-4-turbo"},
		AggregationMode: domain.AggMax, Priority: 200,
	}, nil)
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if updated.ID != specRule.ID {
		t.Errorf("upsert created a new row instead of updating: first=%s now=%s", specRule.ID, updated.ID)
	}
	if updated.AggregationMode != domain.AggMax || updated.Priority != 200 {
		t.Errorf("updated fields not persisted: mode=%s priority=%d", updated.AggregationMode, updated.Priority)
	}

	// Delete and verify.
	if err := store.DeleteMeterPricingRule(ctx, tenantID, defRule.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = store.GetMeterPricingRule(ctx, tenantID, defRule.ID)
	if err == nil || err != errs.ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
	left, _ := store.ListMeterPricingRulesByMeter(ctx, tenantID, meter.ID)
	if len(left) != 1 {
		t.Errorf("after delete: got %d rules, want 1", len(left))
	}
}

// TestPostgresStore_MeterPricingRules_RLS verifies tenant A cannot see
// or mutate tenant B's pricing rules.
func TestPostgresStore_MeterPricingRules_RLS(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantA := testutil.CreateTestTenant(t, db, "Tenant A")
	tenantB := testutil.CreateTestTenant(t, db, "Tenant B")

	// Seed minimal data in tenant A.
	rrvA, _ := store.CreateRatingRule(ctx, tenantA, domain.RatingRuleVersion{
		RuleKey: "a_rule", Name: "A", Version: 1,
		LifecycleState: domain.RatingRuleActive, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	})
	meterA, _ := store.CreateMeter(ctx, tenantA, domain.Meter{
		Key: "tokens", Name: "Tokens", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrvA.ID,
	})
	ruleA, err := store.UpsertMeterPricingRule(ctx, tenantA, domain.MeterPricingRule{
		MeterID: meterA.ID, RatingRuleVersionID: rrvA.ID,
		DimensionMatch: map[string]any{}, AggregationMode: domain.AggSum,
	}, nil)
	if err != nil {
		t.Fatalf("seed tenant A rule: %v", err)
	}

	// Tenant B cannot see A's rule by id.
	if _, err := store.GetMeterPricingRule(ctx, tenantB, ruleA.ID); err != errs.ErrNotFound {
		t.Errorf("RLS: tenant B should not see tenant A's rule, got err=%v", err)
	}

	// Tenant B's list-by-meter must not include A's rule.
	listed, err := store.ListMeterPricingRulesByMeter(ctx, tenantB, meterA.ID)
	if err != nil {
		t.Fatalf("list by meter under tenantB: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("RLS: tenantB saw %d rules under tenantA's meter, want 0", len(listed))
	}

	// Tenant B's delete must report not-found instead of removing it.
	if err := store.DeleteMeterPricingRule(ctx, tenantB, ruleA.ID); err != errs.ErrNotFound {
		t.Errorf("RLS: tenantB delete should be NotFound, got %v", err)
	}
	// And the rule still exists for tenant A.
	if _, err := store.GetMeterPricingRule(ctx, tenantA, ruleA.ID); err != nil {
		t.Errorf("rule must still exist for tenantA after RLS-blocked delete: %v", err)
	}
}
