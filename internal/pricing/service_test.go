package pricing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

func TestCreateMeter(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()

	t.Run("valid", func(t *testing.T) {
		m, err := svc.CreateMeter(ctx, "tenant1", CreateMeterInput{EventName: "api_calls", Name: "API Calls"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m.EventName != "api_calls" || m.Aggregation != "sum" {
			t.Errorf("got %+v, want event_name api_calls, aggregation sum", m)
		}
	})
	t.Run("missing event_name", func(t *testing.T) {
		if _, err := svc.CreateMeter(ctx, "tenant1", CreateMeterInput{Name: "test"}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("invalid aggregation", func(t *testing.T) {
		if _, err := svc.CreateMeter(ctx, "tenant1", CreateMeterInput{EventName: "x", Name: "x", Aggregation: "invalid"}); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestCreatePlan(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := context.Background()
	plan := func(code string, base int64, timing domain.BillTiming) CreatePlanInput {
		return CreatePlanInput{Code: code, Name: code, Currency: "usd", BillingInterval: domain.BillingMonthly, BaseAmountCents: base, BaseBillTiming: timing}
	}

	t.Run("valid", func(t *testing.T) {
		p, prices, err := svc.CreatePlan(ctx, "tenant1", plan("pro", 9900, domain.BillInAdvance))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.Code != "pro" || p.Status != domain.PlanActive || p.Currency != "USD" || p.BaseBillTiming != domain.BillInAdvance {
			t.Errorf("got %+v", p)
		}
		if len(prices) != 0 {
			t.Errorf("prices = %v, want none", prices)
		}
	})
	t.Run("missing code", func(t *testing.T) {
		if _, _, err := svc.CreatePlan(ctx, "tenant1", plan("", 0, "")); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("invalid billing interval", func(t *testing.T) {
		in := plan("weekly", 0, "")
		in.BillingInterval = "weekly"
		if _, _, err := svc.CreatePlan(ctx, "tenant1", in); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("duplicate code", func(t *testing.T) {
		_, _, _ = svc.CreatePlan(ctx, "tenant1", plan("dup", 0, ""))
		if _, _, err := svc.CreatePlan(ctx, "tenant1", plan("dup", 0, "")); err == nil {
			t.Fatal("expected error for duplicate code")
		}
	})
	t.Run("invalid status", func(t *testing.T) {
		in := plan("arch", 0, "")
		in.Status = "archived"
		if _, _, err := svc.CreatePlan(ctx, "tenant1", in); errs.Field(err) != "status" {
			t.Fatalf("got %v, want a refusal on status", err)
		}
	})
}

// TestCreatePlan_BaseBillTiming is ADR-130 §14: the timing is required when
// the base fee is above 0, in_arrears when the base is 0 and it is left out,
// and in_advance with a base of 0 is refused.
func TestCreatePlan_BaseBillTiming(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		base      int64
		timing    domain.BillTiming
		want      domain.BillTiming
		wantField string
	}{
		{"base above 0 without a timing", 500, "", "", "base_bill_timing"},
		{"base above 0 in_advance", 500, domain.BillInAdvance, domain.BillInAdvance, ""},
		{"base above 0 in_arrears", 500, domain.BillInArrears, domain.BillInArrears, ""},
		{"base 0 without a timing", 0, "", domain.BillInArrears, ""},
		{"base 0 in_arrears", 0, domain.BillInArrears, domain.BillInArrears, ""},
		{"base 0 in_advance", 0, domain.BillInAdvance, "", "base_bill_timing"},
		{"an unknown timing", 500, "nightly", "", "base_bill_timing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			p, _, err := NewService(store).CreatePlan(ctx, "t", CreatePlanInput{
				Code: "p", Name: "p", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseAmountCents: tc.base, BaseBillTiming: tc.timing,
			})
			if tc.wantField != "" {
				if errs.Field(err) != tc.wantField {
					t.Fatalf("got %v, want a refusal on %s", err, tc.wantField)
				}
				if len(store.plans) != 0 {
					t.Error("a refused plan was stored")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.BaseBillTiming != tc.want {
				t.Errorf("base_bill_timing = %q, want %q", p.BaseBillTiming, tc.want)
			}
		})
	}
}

// TestCreatePlan_PricesChecked: each price of POST /v1/plans is checked
// before anything is written, and a refusal names it by its index.
func TestCreatePlan_PricesChecked(t *testing.T) {
	ctx := context.Background()
	one := decimal.NewFromInt(1)
	perUnit := domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: one}
	long := strings.Repeat("k", 101)
	tooMany := map[string]string{}
	for i := 0; i < 17; i++ {
		tooMany[string(rune('a'+i))] = "v"
	}
	for _, tc := range []struct {
		name, field string
		price       PriceInput
	}{
		{"no meter", "prices[1].meter_id", PriceInput{PriceShape: perUnit}},
		{"17 labels", "prices[1].dimension_match", PriceInput{MeterID: "m", DimensionMatch: tooMany, PriceShape: perUnit}},
		{"an empty label name", "prices[1].dimension_match", PriceInput{MeterID: "m", DimensionMatch: map[string]string{" ": "v"}, PriceShape: perUnit}},
		{"an empty value", "prices[1].dimension_match", PriceInput{MeterID: "m", DimensionMatch: map[string]string{"model": ""}, PriceShape: perUnit}},
		{"a 101-character name", "prices[1].dimension_match", PriceInput{MeterID: "m", DimensionMatch: map[string]string{long: "v"}, PriceShape: perUnit}},
		{"a 101-character value", "prices[1].dimension_match", PriceInput{MeterID: "m", DimensionMatch: map[string]string{"model": long}, PriceShape: perUnit}},
		{"an unknown mode", "prices[1].mode", PriceInput{MeterID: "m", PriceShape: domain.PriceShape{Mode: "flat", UnitAmountCents: one}}},
		{"a negative rate", "prices[1].unit_amount_cents", PriceInput{MeterID: "m", PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(-1)}}},
		{"a package of size 0", "prices[1].package_size", PriceInput{MeterID: "m", PriceShape: domain.PriceShape{Mode: domain.PricingPackage, PackageAmountCents: one}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			_, _, err := NewService(store).CreatePlan(ctx, "t", CreatePlanInput{
				Code: "p", Name: "p", Currency: "USD", BillingInterval: domain.BillingMonthly,
				Prices: []PriceInput{{MeterID: "m", PriceShape: perUnit}, tc.price},
			})
			if got := errs.Field(err); got != tc.field {
				t.Fatalf("got %v (field %q), want a refusal on %s", err, got, tc.field)
			}
			if len(store.plans) != 0 || len(store.prices) != 0 {
				t.Error("a refused plan or price was stored")
			}
		})
	}
	// The limits are inclusive: 16 labels of 100 characters are taken.
	sixteen := map[string]string{}
	for i := 0; i < 16; i++ {
		sixteen[strings.Repeat(string(rune('a'+i)), 100)] = strings.Repeat("v", 100)
	}
	if _, prices, err := NewService(newMemStore()).CreatePlan(ctx, "t", CreatePlanInput{
		Code: "p", Name: "p", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Prices: []PriceInput{{MeterID: "m", DimensionMatch: sixteen, PriceShape: perUnit}},
	}); err != nil || len(prices) != 1 {
		t.Fatalf("16 labels of 100 characters: %v", err)
	}
}

// Zero rates are legal: a free dimension, or an included allowance ("first
// 1M tokens free, then $2/M", a $0 first graduated tier). Negative rates
// are refused: a discount is a credit (ADR-039).
func TestAddPrice_ZeroRatesAllowedNegativeRefused(t *testing.T) {
	ctx := context.Background()
	upTo := int64(1_000_000)
	for _, tc := range []struct {
		name  string
		shape domain.PriceShape
		ok    bool
	}{
		{"a free per_unit price", domain.PriceShape{Mode: domain.PricingPerUnit}, true},
		{"a free first tier", domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{
			{UpTo: &upTo, UnitAmountCents: decimal.Zero}, {UpTo: nil, UnitAmountCents: decimal.RequireFromString("0.0002")},
		}}, true},
		{"a free package", domain.PriceShape{Mode: domain.PricingPackage, PackageSize: 1000}, true},
		{"a negative per_unit rate", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(-1)}, false},
		{"a negative tier", domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{{UpTo: nil, UnitAmountCents: decimal.NewFromInt(-1)}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			plan, _ := seedPlanWithPrice(store, "t")
			meterID := store.prices["hc_prc_1"].MeterID
			before := len(store.writes)
			_, err := NewService(store).AddPrice(ctx, "t", plan.ID, PriceInput{MeterID: meterID, DimensionMatch: map[string]string{"model": "a"}, PriceShape: tc.shape})
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok && len(store.writes) != before {
				t.Error("a refused price reached the store")
			}
		})
	}
}

// TestEditPrice_CheckedBeforeTheStore: an edit's shape is checked as an
// add's is, and only the content reaches the store; the store answers when
// it applies.
func TestEditPrice_CheckedBeforeTheStore(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	plan, price := seedPlanWithPrice(store, "t")
	svc := NewService(store)
	upTo := int64(100)
	if _, err := svc.EditPrice(ctx, "t", plan.ID, price.ID, domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{
		{UpTo: &upTo, UnitAmountCents: decimal.NewFromInt(1)},
	}}); errs.Field(err) != "tiers" {
		t.Fatalf("a bounded last tier: %v, want a refusal on tiers", err)
	}
	if len(store.writes) != 0 {
		t.Fatal("a refused edit reached the store")
	}
	store.used[plan.ID] = true
	pw, err := svc.EditPrice(ctx, "t", plan.ID, price.ID, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(3)})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if pw.Effective != EffectiveNextPeriod || !pw.Price.UnitAmountCents.Equal(decimal.NewFromInt(3)) {
		t.Errorf("edit on a used plan answered %+v, want next_period at 3", pw)
	}
	if len(store.writes) != 1 || store.writes[0].Op != PriceEdit || store.writes[0].PriceID != price.ID {
		t.Errorf("store writes = %+v, want one edit of %s", store.writes, price.ID)
	}
}

func TestUpdatePlan(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore())
	created, _, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
		Code: "basic", Name: "Basic", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInArrears,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := int64(5900)
	updated, err := svc.UpdatePlan(ctx, "tenant1", created.ID, UpdatePlanInput{Name: "Basic Plus", BaseAmountCents: &base})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.Name != "Basic Plus" || updated.BaseAmountCents != 5900 {
		t.Errorf("got %+v", updated)
	}
	// B9: a PATCH can set the base fee to 0.
	zero := int64(0)
	if updated, err = svc.UpdatePlan(ctx, "tenant1", created.ID, UpdatePlanInput{BaseAmountCents: &zero}); err != nil || updated.BaseAmountCents != 0 {
		t.Fatalf("base to 0: %+v, %v", updated, err)
	}
}

// fakePlanUsage is a stub SubscriptionPlanUsageReader that returns a fixed
// live-sub count for any plan (ADR-034).
type fakePlanUsage struct {
	count int
	err   error
}

func (f *fakePlanUsage) CountLiveSubsByPlan(_ context.Context, _, _ string) (int, error) {
	return f.count, f.err
}

// TestUpdatePlan_EditRules is ADR-130 §5 for the plan's own fields: the base
// fee is fixed while a live subscription uses the plan (ADR-034, until PR-7),
// base_bill_timing is fixed once the plan is used, in_advance at a base of 0
// is refused, and name, description, tax_code and status change at once.
func TestUpdatePlan_EditRules(t *testing.T) {
	ctx := context.Background()
	setup := func(liveCount int, used bool, base int64, timing domain.BillTiming) (*Service, domain.Plan) {
		store := newMemStore()
		svc := NewService(store)
		svc.SetSubscriptionPlanUsageReader(&fakePlanUsage{count: liveCount})
		p, _, err := svc.CreatePlan(ctx, "tenant1", CreatePlanInput{
			Code: "guard", Name: "Guard", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: base, BaseBillTiming: timing,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		store.used[p.ID] = used
		return svc, p
	}
	base := func(v int64) *int64 { return &v }

	t.Run("never used, no live subscriptions: base fee and timing change", func(t *testing.T) {
		svc, p := setup(0, false, 4900, domain.BillInArrears)
		updated, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(9900), BaseBillTiming: domain.BillInAdvance})
		if err != nil {
			t.Fatalf("expected allowed: %v", err)
		}
		if updated.BaseAmountCents != 9900 || updated.BaseBillTiming != domain.BillInAdvance {
			t.Errorf("got %+v", updated)
		}
	})
	t.Run("live subscriptions: the base fee is fixed", func(t *testing.T) {
		svc, p := setup(1, true, 4900, domain.BillInArrears)
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(9900)})
		want := "1 subscription uses this plan, so its base fee is fixed. Create a new plan with the new fee instead."
		if err == nil || err.Error() != want || errs.Field(err) != "plan" {
			t.Fatalf("got %v, want %q on plan", err, want)
		}
		svc, p = setup(3, true, 4900, domain.BillInArrears)
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(9900)}); err == nil || !strings.HasPrefix(err.Error(), "3 subscriptions use this plan,") {
			t.Errorf("the refusal gives the count in plain words: %v", err)
		}
	})
	t.Run("live subscriptions: the same base fee passes", func(t *testing.T) {
		svc, p := setup(1, true, 4900, domain.BillInArrears)
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(4900)}); err != nil {
			t.Errorf("no-op base fee: %v", err)
		}
	})
	t.Run("used: base_bill_timing is fixed", func(t *testing.T) {
		svc, p := setup(0, true, 4900, domain.BillInArrears)
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseBillTiming: domain.BillInAdvance}); errs.Field(err) != "base_bill_timing" {
			t.Fatalf("got %v, want a refusal on base_bill_timing", err)
		}
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseBillTiming: domain.BillInArrears}); err != nil {
			t.Errorf("the timing it has: %v", err)
		}
	})
	t.Run("in_advance at a base of 0 is refused", func(t *testing.T) {
		svc, p := setup(0, false, 4900, domain.BillInAdvance)
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(0)}); errs.Field(err) != "base_amount_cents" {
			t.Fatalf("base to 0 on an in_advance plan: %v, want a refusal on base_amount_cents", err)
		}
		svc, p = setup(0, false, 0, "")
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseBillTiming: domain.BillInAdvance}); errs.Field(err) != "base_bill_timing" {
			t.Fatalf("in_advance on a base-0 plan: %v, want a refusal on base_bill_timing", err)
		}
	})
	// A used in_advance plan has a fixed timing, so its base fee cannot go to
	// 0, even with no live subscription left. The refusal used to say "set
	// base_bill_timing to in_arrears", which the timing lock then refused.
	t.Run("used in_advance plan: the refusal of a base of 0 points at a new plan", func(t *testing.T) {
		svc, p := setup(0, true, 4900, domain.BillInAdvance)
		want := "This plan bills its base fee in_advance, and a subscription has used it, so the base fee must stay above 0. To charge no base fee, create a new plan that bills in_arrears."
		_, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(0)})
		if err == nil || err.Error() != want || errs.Field(err) != "base_amount_cents" {
			t.Fatalf("got %v on %q, want %q on base_amount_cents", err, errs.Field(err), want)
		}
		// Asking for in_arrears as well is the timing change the lock refuses.
		_, err = svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(0), BaseBillTiming: domain.BillInArrears})
		if errs.Field(err) != "base_bill_timing" {
			t.Fatalf("got %v, want the timing refusal on base_bill_timing", err)
		}
		// A plan no subscription has used still gets the timing advice: it works there.
		svc, p = setup(0, false, 4900, domain.BillInAdvance)
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(0)}); err == nil || !strings.Contains(err.Error(), "set base_bill_timing to in_arrears") {
			t.Fatalf("unused plan: got %v, want the advice to set base_bill_timing to in_arrears", err)
		}
		if _, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{BaseAmountCents: base(0), BaseBillTiming: domain.BillInArrears}); err != nil {
			t.Fatalf("unused plan: base 0 with in_arrears: %v", err)
		}
	})
	t.Run("live subscriptions: display fields change at once", func(t *testing.T) {
		svc, p := setup(3, true, 4900, domain.BillInArrears)
		desc, tax := "New description", "txcd_10000000"
		updated, err := svc.UpdatePlan(ctx, "tenant1", p.ID, UpdatePlanInput{Name: "Renamed", Description: &desc, TaxCode: &tax, Status: "archived"})
		if err != nil {
			t.Fatalf("display-only mutation should be allowed: %v", err)
		}
		if updated.Name != "Renamed" || updated.Description != desc || updated.TaxCode != tax || updated.Status != domain.PlanArchived {
			t.Errorf("got %+v", updated)
		}
	})
	t.Run("LiveSubscriptionCount is the count the lock uses", func(t *testing.T) {
		svc, p := setup(3, true, 4900, domain.BillInArrears)
		if n, err := svc.LiveSubscriptionCount(ctx, "tenant1", p.ID); err != nil || n != 3 {
			t.Fatalf("LiveSubscriptionCount = %d, %v; want 3", n, err)
		}
		if n, err := NewService(newMemStore()).LiveSubscriptionCount(ctx, "tenant1", p.ID); err != nil || n != 0 {
			t.Fatalf("no reader wired: got %d, %v; want 0", n, err)
		}
	})
}

// A meter's aggregation changes until the meter has events, then it is
// fixed (ADR-130 §2). The real has-usage read is pinned against Postgres in
// meter_aggregation_lock_integration_test.go.
func TestUpdateMeter_AggregationFixedOnceTheMeterHasUsage(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	ctx := context.Background()
	m, err := svc.CreateMeter(ctx, "t1", CreateMeterInput{EventName: "gpu", Name: "GPU"})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	median := "median"
	if _, err := svc.UpdateMeter(ctx, "t1", m.ID, UpdateMeterInput{Aggregation: &median}); err == nil {
		t.Fatal("an unknown aggregation must be refused")
	}
	max := "max"
	if got, err := svc.UpdateMeter(ctx, "t1", m.ID, UpdateMeterInput{Aggregation: &max}); err != nil || got.Aggregation != "max" {
		t.Fatalf("before any event: got %q, err %v; want max", got.Aggregation, err)
	}
	store.hasUsage[m.ID] = true
	sum := "sum"
	_, err = svc.UpdateMeter(ctx, "t1", m.ID, UpdateMeterInput{Aggregation: &sum})
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != errs.CodeMeterHasUsage {
		t.Fatalf("with usage: want 409 %s, got %v", errs.CodeMeterHasUsage, err)
	}
	if !strings.Contains(err.Error(), `"gpu"`) || !strings.Contains(err.Error(), "max") {
		t.Errorf("the refusal must name the meter and its aggregation, got %q", err.Error())
	}
	if store.meters[m.ID].Aggregation != "max" {
		t.Errorf("aggregation = %q, want max", store.meters[m.ID].Aggregation)
	}
	name := "GPU seconds"
	if _, err := svc.UpdateMeter(ctx, "t1", m.ID, UpdateMeterInput{Name: &name, Aggregation: &max}); err != nil {
		t.Errorf("a rename that keeps the aggregation: %v", err)
	}
}

// TestCreateOverride_Checked: an override names a customer and a price and
// takes a price's shape, checked as a price's is (ADR-130 §6).
func TestCreateOverride_Checked(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	_, price := seedPlanWithPrice(store, "t1")
	svc := NewService(store)
	shape := domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(5)}

	if _, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{PriceID: price.ID, PriceShape: shape}); errs.Field(err) != "customer_id" {
		t.Errorf("no customer: %v, want a refusal on customer_id", err)
	}
	if _, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{CustomerID: "cus_1", PriceShape: shape}); errs.Field(err) != "price_id" {
		t.Errorf("no price: %v, want a refusal on price_id", err)
	}
	a, b := int64(100), int64(50)
	if _, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{CustomerID: "cus_1", PriceID: price.ID, PriceShape: domain.PriceShape{
		Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{{UpTo: &a, UnitAmountCents: decimal.NewFromInt(10)}, {UpTo: &b, UnitAmountCents: decimal.NewFromInt(5)}},
	}}); errs.Field(err) != "tiers" {
		t.Errorf("tiers that do not increase: %v, want a refusal on tiers", err)
	}
	if len(store.overrides) != 0 {
		t.Fatal("a refused override was stored")
	}
	o, err := svc.CreateOverride(ctx, "t1", CreateOverrideInput{CustomerID: "cus_1", PriceID: price.ID, PriceShape: shape, Reason: "  negotiated "})
	if err != nil {
		t.Fatalf("create override: %v", err)
	}
	if o.PriceID != price.ID || !o.UnitAmountCents.Equal(decimal.NewFromInt(5)) || o.Reason != "negotiated" {
		t.Errorf("override = %+v", o)
	}
}
