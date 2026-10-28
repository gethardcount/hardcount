package pricing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

func TestPostgresStore_Meters(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Test")

	meter, err := store.CreateMeter(ctx, tenantID, domain.Meter{
		EventName: "api_calls", Name: "API Calls", Unit: "calls", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.GetMeter(ctx, tenantID, meter.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EventName != "api_calls" {
		t.Errorf("event_name: got %q", got.EventName)
	}

	// Unique key per tenant
	_, err = store.CreateMeter(ctx, tenantID, domain.Meter{
		EventName: "api_calls", Name: "Dup", Unit: "x", Aggregation: "sum",
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
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetPlan(ctx, tenantID, plan.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Code != "pro" || got.BaseAmountCents != 4900 || got.BaseBillTiming != domain.BillInAdvance {
		t.Errorf("got %+v", got)
	}

	updated, err := store.UpdatePlan(ctx, tenantID, plan.ID, func(locked domain.Plan, used bool) (domain.Plan, error) {
		if used {
			t.Error("a new plan reads as used")
		}
		locked.Name = "Pro Plus"
		locked.BaseAmountCents = 5900
		return locked, nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Pro Plus" || updated.BaseAmountCents != 5900 {
		t.Errorf("updated: got %+v", updated)
	}
	if _, err := store.UpdatePlan(ctx, tenantID, "hc_pln_missing", func(p domain.Plan, _ bool) (domain.Plan, error) { return p, nil }); err != errs.ErrNotFound {
		t.Errorf("unknown plan: got %v, want ErrNotFound", err)
	}
}

// TestPostgresStore_PlanPrices: a plan's prices round-trip with their
// shapes (a graduated price's open last tier stays null), list in creation
// order, resolve in claim order (the most labels first), and are invisible
// to another tenant, with no tenant filter in the query: row-level security
// filters them.
func TestPostgresStore_PlanPrices(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Prices")
	other := testutil.CreateTestTenant(t, db, "Other")

	meter, err := store.CreateMeter(ctx, tenantID, domain.Meter{EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	upTo := int64(1000)
	shapes := []pricing.PriceChange{
		{Op: pricing.PriceAdd, MeterID: meter.ID, Shape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0003")}},
		{Op: pricing.PriceAdd, MeterID: meter.ID, Match: map[string]string{"model": "a"}, Shape: domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{
			{UpTo: &upTo, UnitAmountCents: decimal.Zero}, {UpTo: nil, UnitAmountCents: decimal.NewFromInt(2)},
		}}},
		{Op: pricing.PriceAdd, MeterID: meter.ID, Match: map[string]string{"model": "a", "token_type": "input"}, Shape: domain.PriceShape{Mode: domain.PricingPackage, PackageSize: 1000, PackageAmountCents: decimal.RequireFromString("0.2")}},
	}
	plan, prices, err := store.CreatePlanWithPrices(ctx, tenantID, domain.Plan{
		Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	}, shapes)
	if err != nil {
		t.Fatalf("create plan with prices: %v", err)
	}
	if len(prices) != 3 {
		t.Fatalf("prices = %d, want 3", len(prices))
	}

	listed, err := store.ListPrices(ctx, tenantID, []string{plan.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := listed[plan.ID]
	if len(got) != 3 || got[0].ID != prices[0].ID || got[1].ID != prices[1].ID || got[2].ID != prices[2].ID {
		t.Fatalf("listed %+v, want the three in creation order", got)
	}
	if !got[0].UnitAmountCents.Equal(decimal.RequireFromString("0.0003")) || len(got[0].DimensionMatch) != 0 {
		t.Errorf("per_unit price = %+v", got[0])
	}
	if len(got[1].Tiers) != 2 || got[1].Tiers[0].UpTo == nil || *got[1].Tiers[0].UpTo != 1000 || got[1].Tiers[1].UpTo != nil {
		t.Errorf("graduated tiers = %+v, want up_to 1000 then null", got[1].Tiers)
	}
	if got[2].PackageSize != 1000 || !got[2].PackageAmountCents.Equal(decimal.RequireFromString("0.2")) || got[2].DimensionMatch["token_type"] != "input" {
		t.Errorf("package price = %+v", got[2])
	}

	at, err := store.PlanPricesAt(ctx, tenantID, plan.ID, time.Now().UTC(), "")
	if err != nil {
		t.Fatalf("prices at: %v", err)
	}
	if len(at) != 3 || at[0].ID != prices[2].ID || at[1].ID != prices[1].ID || at[2].ID != prices[0].ID {
		t.Errorf("claim order = %v, want the 2-label price, the 1-label price, then {}", at)
	}
	if meters, err := store.PlanMeterIDs(ctx, tenantID, plan.ID); err != nil || len(meters) != 1 || meters[0] != meter.ID {
		t.Errorf("plan meters = %v, %v; want [%s]", meters, err, meter.ID)
	}

	// Row-level security: the other tenant reads no row of any of the
	// three tables, with no tenant filter in the query.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, other)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	for _, table := range []string{"plan_prices", "plan_price_versions", "plan_price_version_clocks"} {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("tenant B sees %d rows of %s", n, table)
		}
	}
	if byPlan, err := store.ListPrices(ctx, other, []string{plan.ID}); err != nil || len(byPlan[plan.ID]) != 0 {
		t.Errorf("tenant B lists %v, %v; want nothing", byPlan, err)
	}
}
