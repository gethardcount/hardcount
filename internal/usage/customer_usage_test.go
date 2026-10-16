package usage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// ---------- Fakes for the four CustomerUsageService collaborators ---------

type fakeCustomerLookup struct {
	customers map[string]domain.Customer
}

func (f *fakeCustomerLookup) Get(_ context.Context, tenantID, id string) (domain.Customer, error) {
	c, ok := f.customers[tenantID+"/"+id]
	if !ok {
		return domain.Customer{}, errs.New("not_found", "customer not found").WithCode("customer_not_found")
	}
	return c, nil
}

type fakeSubLister struct {
	subs []domain.Subscription
}

func (f *fakeSubLister) List(_ context.Context, filter subscription.ListFilter) ([]domain.Subscription, int, error) {
	out := []domain.Subscription{}
	for _, s := range f.subs {
		if s.TenantID != filter.TenantID {
			continue
		}
		if filter.CustomerID != "" && s.CustomerID != filter.CustomerID {
			continue
		}
		out = append(out, s)
	}
	return out, len(out), nil
}

type fakePricingReader struct {
	plans      map[string]domain.Plan
	meters     map[string]domain.Meter
	rules      map[string]domain.RatingRuleVersion
	pricingMap map[string][]domain.MeterPricingRule    // by meter id
	overrides  map[string]domain.CustomerPriceOverride // key: customerID+":"+ruleKey
}

// GetRuleByKeyAsOf mirrors the store's ADR-070 resolution against the
// fake's rules map: highest active version created at or before asOf,
// else the earliest active version. Fixture rules with zero CreatedAt
// are "always in force".
func (f *fakePricingReader) GetRuleByKeyAsOf(_ context.Context, _, ruleKey string, asOf time.Time) (domain.RatingRuleVersion, error) {
	var best, earliest domain.RatingRuleVersion
	foundBest, foundAny := false, false
	for _, r := range f.rules {
		if r.RuleKey != ruleKey {
			continue
		}
		if r.LifecycleState != "" && r.LifecycleState != domain.RatingRuleActive {
			continue
		}
		if !foundAny || r.Version < earliest.Version {
			earliest = r
			foundAny = true
		}
		if !r.CreatedAt.After(asOf) && (!foundBest || r.Version > best.Version) {
			best = r
			foundBest = true
		}
	}
	if foundBest {
		return best, nil
	}
	if foundAny {
		return earliest, nil
	}
	return domain.RatingRuleVersion{}, errs.ErrNotFound
}

func (f *fakePricingReader) GetOverrideByKeyAsOf(_ context.Context, _, customerID, ruleKey string, _ time.Time) (domain.CustomerPriceOverride, error) {
	if o, ok := f.overrides[customerID+":"+ruleKey]; ok {
		return o, nil
	}
	return domain.CustomerPriceOverride{}, errs.ErrNotFound
}

func (f *fakePricingReader) GetPlan(_ context.Context, _, id string) (domain.Plan, error) {
	if p, ok := f.plans[id]; ok {
		return p, nil
	}
	return domain.Plan{}, errors.New("plan not found")
}

func (f *fakePricingReader) GetMeter(_ context.Context, _, id string) (domain.Meter, error) {
	if m, ok := f.meters[id]; ok {
		return m, nil
	}
	return domain.Meter{}, errors.New("meter not found")
}

func (f *fakePricingReader) GetRatingRule(_ context.Context, _, id string) (domain.RatingRuleVersion, error) {
	if r, ok := f.rules[id]; ok {
		return r, nil
	}
	return domain.RatingRuleVersion{}, errors.New("rating rule not found")
}

func (f *fakePricingReader) ListMeterPricingRulesByMeter(_ context.Context, _, meterID string) ([]domain.MeterPricingRule, error) {
	return f.pricingMap[meterID], nil
}

// aggStore wraps memStore with a programmable AggregateByPricingRules return.
type aggStore struct {
	*memStore
	aggs map[string][]domain.RuleAggregation // by meter id
}

func (a *aggStore) AggregateByPricingRules(_ context.Context, _, _, meterID string, _ domain.AggregationMode, _, _ time.Time) ([]domain.RuleAggregation, error) {
	return a.aggs[meterID], nil
}

func newAggStore() *aggStore {
	return &aggStore{memStore: newMemStore(), aggs: map[string][]domain.RuleAggregation{}}
}

// ---------- resolvePeriod ---------

func TestResolvePeriod(t *testing.T) {
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	jun1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	leap1 := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	leapDec31End := time.Date(2028, 12, 31, 23, 59, 59, 999_999_000, time.UTC)

	subWithCycle := func(start, end time.Time) domain.Subscription {
		return domain.Subscription{
			ID:                        "sub_1",
			TenantID:                  "t1",
			Status:                    domain.SubscriptionActive,
			CurrentBillingPeriodStart: &start,
			CurrentBillingPeriodEnd:   &end,
		}
	}

	cases := []struct {
		name        string
		period      CustomerUsagePeriod
		subs        []domain.Subscription
		wantSource  string
		wantFrom    time.Time
		wantTo      time.Time
		wantErrCode string
	}{
		{
			name:       "default to current cycle",
			subs:       []domain.Subscription{subWithCycle(apr1, may1)},
			wantSource: "current_billing_cycle",
			wantFrom:   apr1,
			wantTo:     may1,
		},
		{
			name:        "default with no active sub returns coded error",
			subs:        nil,
			wantErrCode: "customer_has_no_subscription",
		},
		{
			name:       "explicit window",
			period:     CustomerUsagePeriod{From: apr1, To: may1},
			wantSource: "explicit",
			wantFrom:   apr1,
			wantTo:     may1,
		},
		{
			name:        "partial bounds (from only) → 400",
			period:      CustomerUsagePeriod{From: apr1},
			wantErrCode: "",
		},
		{
			name:        "partial bounds (to only) → 400",
			period:      CustomerUsagePeriod{To: may1},
			wantErrCode: "",
		},
		{
			name:        "from after to → 400",
			period:      CustomerUsagePeriod{From: may1, To: apr1},
			wantErrCode: "",
		},
		{
			name:        "window > 1 year → 400",
			period:      CustomerUsagePeriod{From: apr1, To: jun1.AddDate(1, 0, 0)},
			wantErrCode: "",
		},
		{
			// A leap year is one year. Date-only ?from=2028-01-01&to=2028-12-31
			// (to = last microsecond of Dec 31) spans 366 days minus 1µs, which
			// the old 365×24h cap rejected.
			name:       "leap calendar year 2028 is within the one-year cap",
			period:     CustomerUsagePeriod{From: leap1, To: leapDec31End},
			wantSource: "explicit",
			wantFrom:   leap1,
			wantTo:     leapDec31End,
		},
		{
			// A yearly billing cycle passed explicitly (as /margin does) is
			// exactly one calendar year: 366 days when it spans Feb 29.
			name:       "366-day yearly cycle passed explicitly is accepted",
			period:     CustomerUsagePeriod{From: leap1, To: leap1.AddDate(1, 0, 0)},
			wantSource: "explicit",
			wantFrom:   leap1,
			wantTo:     leap1.AddDate(1, 0, 0),
		},
		{
			name:        "one calendar year plus a microsecond → 422",
			period:      CustomerUsagePeriod{From: leap1, To: leap1.AddDate(1, 0, 0).Add(time.Microsecond)},
			wantErrCode: "",
		},
		{
			name: "multi-sub picks latest period start",
			subs: []domain.Subscription{
				subWithCycle(apr1, may1),
				subWithCycle(may1, jun1),
			},
			wantSource: "current_billing_cycle",
			wantFrom:   may1,
			wantTo:     jun1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, source, err := resolvePeriod(tc.period, tc.subs)
			if tc.wantSource == "" {
				if err == nil {
					t.Fatalf("expected error, got from=%v to=%v source=%s", from, to, source)
				}
				if tc.wantErrCode != "" && errs.Code(err) != tc.wantErrCode {
					t.Errorf("error code: got %q, want %q", errs.Code(err), tc.wantErrCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if source != tc.wantSource {
				t.Errorf("source: got %q, want %q", source, tc.wantSource)
			}
			if !from.Equal(tc.wantFrom) {
				t.Errorf("from: got %v, want %v", from, tc.wantFrom)
			}
			if !to.Equal(tc.wantTo) {
				t.Errorf("to: got %v, want %v", to, tc.wantTo)
			}
		})
	}
}

// ---------- Get end-to-end ---------

func TestCustomerUsageService_Get_SingleMeterFlat(t *testing.T) {
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	customers := &fakeCustomerLookup{customers: map[string]domain.Customer{
		"t1/cus_1": {ID: "cus_1", TenantID: "t1"},
	}}
	subs := &fakeSubLister{subs: []domain.Subscription{
		{
			ID:                        "sub_1",
			TenantID:                  "t1",
			CustomerID:                "cus_1",
			Status:                    domain.SubscriptionActive,
			CurrentBillingPeriodStart: &apr1,
			CurrentBillingPeriodEnd:   &may1,
			Items: []domain.SubscriptionItem{
				{ID: "itm_1", PlanID: "pln_1", Quantity: 1},
			},
		},
	}}
	pricing := &fakePricingReader{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "Pro", Currency: "USD", MeterIDs: []string{"mtr_1"}},
		},
		meters: map[string]domain.Meter{
			"mtr_1": {
				ID: "mtr_1", Key: "tokens", Name: "Tokens",
				Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: "rrv_1",
			},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_1": {
				ID: "rrv_1", RuleKey: "tokens_flat", Mode: domain.PricingFlat,
				Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
			},
		},
		pricingMap: map[string][]domain.MeterPricingRule{},
	}
	store := newAggStore()
	store.aggs["mtr_1"] = []domain.RuleAggregation{
		{RuleID: "", RatingRuleVersionID: "rrv_1", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(1000)},
	}
	usageSvc := NewService(store)

	svc := NewCustomerUsageService(usageSvc, customers, subs, pricing)
	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.CustomerID != "cus_1" {
		t.Errorf("customer_id: got %q", res.CustomerID)
	}
	if res.Period.Source != "current_billing_cycle" {
		t.Errorf("period source: got %q", res.Period.Source)
	}
	if len(res.Subscriptions) != 1 {
		t.Fatalf("subscriptions: got %d, want 1", len(res.Subscriptions))
	}
	if len(res.Meters) != 1 {
		t.Fatalf("meters: got %d, want 1", len(res.Meters))
	}
	m := res.Meters[0]
	if m.MeterKey != "tokens" || m.Currency != "USD" || m.TotalAmountCents != 1000 {
		t.Errorf("meter: got %+v", m)
	}
	if len(res.Totals) != 1 || res.Totals[0].Currency != "USD" || res.Totals[0].AmountCents != 1000 {
		t.Errorf("totals: got %+v", res.Totals)
	}
	if res.Warnings == nil {
		t.Errorf("warnings: must be []string{} (non-nil for empty array on wire), got nil")
	}
}

// The rule row carries the NOMINAL configured rate (what the invoice shows),
// not the effective amount÷qty — the screenshot case: a flat rule at
// 0.0015¢/token bills 1,750 tokens at 3¢ (2.625¢ rounded), whose effective rate
// 0.001714…¢ ≠ nominal 0.0015¢. Regression lock for Activity-panel / invoice
// unit-price consistency (ADR-054): the FE renders this backend value instead
// of deriving amount÷qty (which rounded 0.0015¢ down to $0.0000).
func TestCustomerUsageService_Get_RuleRowShowsNominalUnitRate(t *testing.T) {
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	customers := &fakeCustomerLookup{customers: map[string]domain.Customer{
		"t1/cus_1": {ID: "cus_1", TenantID: "t1"},
	}}
	subs := &fakeSubLister{subs: []domain.Subscription{
		{
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
			Status:                    domain.SubscriptionActive,
			CurrentBillingPeriodStart: &apr1,
			CurrentBillingPeriodEnd:   &may1,
			Items:                     []domain.SubscriptionItem{{ID: "itm_1", PlanID: "pln_1", Quantity: 1}},
		},
	}}
	pricing := &fakePricingReader{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "Pro", Currency: "USD", MeterIDs: []string{"mtr_1"}},
		},
		meters: map[string]domain.Meter{
			"mtr_1": {ID: "mtr_1", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: "rrv_1"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_1": {
				ID: "rrv_1", RuleKey: "tokens_out", Mode: domain.PricingFlat,
				Currency: "USD", FlatAmountCents: decimal.RequireFromString("0.0015"),
			},
		},
		pricingMap: map[string][]domain.MeterPricingRule{},
	}
	store := newAggStore()
	store.aggs["mtr_1"] = []domain.RuleAggregation{
		{RuleID: "", RatingRuleVersionID: "rrv_1", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(1750)},
	}

	svc := NewCustomerUsageService(NewService(store), customers, subs, pricing)
	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Meters) != 1 || len(res.Meters[0].Rules) != 1 {
		t.Fatalf("want 1 meter with 1 rule, got %d meters", len(res.Meters))
	}
	row := res.Meters[0].Rules[0]
	if row.AmountCents != 3 { // 1,750 × 0.0015¢ = 2.625¢ → 3¢
		t.Fatalf("amount_cents: got %d, want 3", row.AmountCents)
	}
	if row.UnitAmountDecimal == nil {
		t.Fatal("unit_amount_decimal: nil, want nominal 0.0015")
	}
	if *row.UnitAmountDecimal != "0.0015" {
		t.Errorf("unit_amount_decimal: got %q, want 0.0015 (nominal, not effective 0.0017142857…)", *row.UnitAmountDecimal)
	}
}

func TestCustomerUsageService_Get_MultiCurrencyTotals(t *testing.T) {
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	customers := &fakeCustomerLookup{customers: map[string]domain.Customer{
		"t1/cus_1": {ID: "cus_1", TenantID: "t1"},
	}}
	subs := &fakeSubLister{subs: []domain.Subscription{
		{
			ID:                        "sub_1",
			TenantID:                  "t1",
			CustomerID:                "cus_1",
			Status:                    domain.SubscriptionActive,
			CurrentBillingPeriodStart: &apr1,
			CurrentBillingPeriodEnd:   &may1,
			Items: []domain.SubscriptionItem{
				{ID: "itm_1", PlanID: "pln_usd", Quantity: 1},
				{ID: "itm_2", PlanID: "pln_eur", Quantity: 1},
			},
		},
	}}
	pricing := &fakePricingReader{
		plans: map[string]domain.Plan{
			"pln_usd": {ID: "pln_usd", Name: "US Pro", Currency: "USD", MeterIDs: []string{"mtr_usd"}},
			"pln_eur": {ID: "pln_eur", Name: "EU Pro", Currency: "EUR", MeterIDs: []string{"mtr_eur"}},
		},
		meters: map[string]domain.Meter{
			"mtr_usd": {ID: "mtr_usd", Key: "tokens_usd", Aggregation: "sum", RatingRuleVersionID: "rrv_usd"},
			"mtr_eur": {ID: "mtr_eur", Key: "tokens_eur", Aggregation: "sum", RatingRuleVersionID: "rrv_eur"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_usd": {ID: "rrv_usd", RuleKey: "u", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1)},
			"rrv_eur": {ID: "rrv_eur", RuleKey: "e", Mode: domain.PricingFlat, Currency: "EUR", FlatAmountCents: decimal.NewFromInt(2)},
		},
		pricingMap: map[string][]domain.MeterPricingRule{},
	}
	store := newAggStore()
	store.aggs["mtr_usd"] = []domain.RuleAggregation{
		{RatingRuleVersionID: "rrv_usd", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(100)},
	}
	store.aggs["mtr_eur"] = []domain.RuleAggregation{
		{RatingRuleVersionID: "rrv_eur", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(50)},
	}
	usageSvc := NewService(store)

	svc := NewCustomerUsageService(usageSvc, customers, subs, pricing)
	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Totals) != 2 {
		t.Fatalf("totals: got %d entries, want 2", len(res.Totals))
	}
	got := map[string]int64{}
	for _, t := range res.Totals {
		got[t.Currency] = t.AmountCents
	}
	if got["USD"] != 100 || got["EUR"] != 100 {
		t.Errorf("totals: got %+v", got)
	}
}

func TestCustomerUsageService_Get_CustomerNotFoundPropagates(t *testing.T) {
	customers := &fakeCustomerLookup{customers: map[string]domain.Customer{}}
	svc := NewCustomerUsageService(
		NewService(newMemStore()),
		customers,
		&fakeSubLister{},
		&fakePricingReader{plans: map[string]domain.Plan{}, meters: map[string]domain.Meter{}, rules: map[string]domain.RatingRuleVersion{}, pricingMap: map[string][]domain.MeterPricingRule{}},
	)
	_, err := svc.Get(context.Background(), "t1", "cus_missing", CustomerUsagePeriod{})
	if err == nil {
		t.Fatal("expected customer-not-found error")
	}
}

func TestCustomerUsageService_Get_RequiresCustomerID(t *testing.T) {
	svc := NewCustomerUsageService(
		NewService(newMemStore()),
		&fakeCustomerLookup{customers: map[string]domain.Customer{}},
		&fakeSubLister{},
		&fakePricingReader{},
	)
	_, err := svc.Get(context.Background(), "t1", "   ", CustomerUsagePeriod{})
	if err == nil {
		t.Fatal("expected validation error for blank customer id")
	}
	if !errors.Is(err, errs.ErrValidation) {
		t.Errorf("error not a validation error: %v", err)
	}
}

func TestCustomerUsageService_Get_MultiDimRulesEchoDimensionMatch(t *testing.T) {
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	customers := &fakeCustomerLookup{customers: map[string]domain.Customer{
		"t1/cus_1": {ID: "cus_1", TenantID: "t1"},
	}}
	subs := &fakeSubLister{subs: []domain.Subscription{
		{
			ID:                        "sub_1",
			TenantID:                  "t1",
			CustomerID:                "cus_1",
			Status:                    domain.SubscriptionActive,
			CurrentBillingPeriodStart: &apr1,
			CurrentBillingPeriodEnd:   &may1,
			Items: []domain.SubscriptionItem{
				{ID: "itm_1", PlanID: "pln_1", Quantity: 1},
			},
		},
	}}
	pricing := &fakePricingReader{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "AI Pro", Currency: "USD", MeterIDs: []string{"mtr_tokens"}},
		},
		meters: map[string]domain.Meter{
			"mtr_tokens": {ID: "mtr_tokens", Key: "tokens", Aggregation: "sum"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_in":  {ID: "rrv_in", RuleKey: "input", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(3)},
			"rrv_out": {ID: "rrv_out", RuleKey: "output", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(5)},
		},
		pricingMap: map[string][]domain.MeterPricingRule{
			"mtr_tokens": {
				{ID: "mpr_in", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_in", DimensionMatch: map[string]any{"operation": "input"}, Priority: 10},
				{ID: "mpr_out", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_out", DimensionMatch: map[string]any{"operation": "output"}, Priority: 10},
			},
		},
	}
	store := newAggStore()
	store.aggs["mtr_tokens"] = []domain.RuleAggregation{
		{RuleID: "mpr_in", RatingRuleVersionID: "rrv_in", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(100)},
		{RuleID: "mpr_out", RatingRuleVersionID: "rrv_out", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(50)},
	}
	usageSvc := NewService(store)

	svc := NewCustomerUsageService(usageSvc, customers, subs, pricing)
	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Meters) != 1 {
		t.Fatalf("meters: got %d, want 1", len(res.Meters))
	}
	rules := res.Meters[0].Rules
	if len(rules) != 2 {
		t.Fatalf("rules: got %d, want 2", len(rules))
	}
	for _, rule := range rules {
		if rule.DimensionMatch == nil {
			t.Errorf("rule %q dimension_match should be non-nil", rule.RuleKey)
		}
	}
	// Total = 100*3 + 50*5 = 550
	if res.Meters[0].TotalAmountCents != 550 {
		t.Errorf("total cents: got %d, want 550", res.Meters[0].TotalAmountCents)
	}
}

func TestCustomerUsageService_Get_FlatMeterOmitsDimensionMatch(t *testing.T) {
	// Ensure single-rule meters emit `dimension_match` omitted (omitempty),
	// matching the design RFC's "no dimension_match for flat single-rule".
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	customers := &fakeCustomerLookup{customers: map[string]domain.Customer{"t1/cus_1": {ID: "cus_1"}}}
	subs := &fakeSubLister{subs: []domain.Subscription{{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.SubscriptionActive,
		CurrentBillingPeriodStart: &apr1, CurrentBillingPeriodEnd: &may1,
		Items: []domain.SubscriptionItem{{ID: "itm_1", PlanID: "pln_1"}},
	}}}
	pricing := &fakePricingReader{
		plans:  map[string]domain.Plan{"pln_1": {ID: "pln_1", Name: "Flat", Currency: "USD", MeterIDs: []string{"mtr_1"}}},
		meters: map[string]domain.Meter{"mtr_1": {ID: "mtr_1", Key: "k", Aggregation: "sum", RatingRuleVersionID: "rrv_1"}},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_1": {ID: "rrv_1", RuleKey: "rk", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(7)},
		},
		pricingMap: map[string][]domain.MeterPricingRule{},
	}
	store := newAggStore()
	store.aggs["mtr_1"] = []domain.RuleAggregation{{RatingRuleVersionID: "rrv_1", Quantity: decimal.NewFromInt(10)}}
	svc := NewCustomerUsageService(NewService(store), customers, subs, pricing)

	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Meters) != 1 || len(res.Meters[0].Rules) != 1 {
		t.Fatalf("unexpected shape: %+v", res.Meters)
	}
	if res.Meters[0].Rules[0].DimensionMatch != nil {
		t.Errorf("dimension_match should be nil for flat rule, got %+v", res.Meters[0].Rules[0].DimensionMatch)
	}
}

// ---------- mapMeterAggregation ---------

func TestMapMeterAggregation(t *testing.T) {
	cases := map[string]domain.AggregationMode{
		"sum":     domain.AggSum,
		"count":   domain.AggCount,
		"max":     domain.AggMax,
		"last":    domain.AggLastDuringPeriod, // UI's "last" maps to period-bounded
		"":        domain.AggSum,              // default
		"unknown": domain.AggSum,              // safe default for new / unknown
	}
	for in, want := range cases {
		if got := mapMeterAggregation(in); got != want {
			t.Errorf("mapMeterAggregation(%q): got %q, want %q", in, got, want)
		}
	}
}

// ---------- Sanity: response wire-shape stability ---------

func TestCustomerUsageResult_EmptyArraysOnWire(t *testing.T) {
	// Build an empty result through the constructor path to assert the
	// JSON output emits "[]" not "null" for empty list fields. Mirrors
	// the regression test we added for /v1/recipes after the picker UI
	// hit null-iteration crashes.
	res := CustomerUsageResult{
		CustomerID:    "cus_x",
		Period:        CustomerUsagePeriodOut{Source: "explicit"},
		Subscriptions: []CustomerUsageSubscription{},
		Meters:        []CustomerUsageMeter{},
		Totals:        []CustomerUsageTotal{},
		Warnings:      []string{},
	}
	for _, want := range []string{`"subscriptions":[]`, `"meters":[]`, `"totals":[]`, `"warnings":[]`} {
		if !containsField(res, want) {
			t.Errorf("expected JSON to contain %q", want)
		}
	}
}

func containsField(res CustomerUsageResult, needle string) bool {
	b, _ := json.Marshal(res)
	return strings.Contains(string(b), needle)
}

// P10 (ADR-070 slice): the usage view must price with the SAME rule the
// invoice will bill. Pre-fix rateMeter priced the pinned version with
// NO override lookup — a negotiated customer's running-spend read list
// price, wrong for exactly the customers the spend-cap wedge targets.
//
// Mutation-verify: skip the override lookup in rateMeter — the
// overridden assertions fail.
func TestCustomerUsageService_Get_HonorsOverridesAndPeriodPin(t *testing.T) {
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mkSub := func(cust string) domain.Subscription {
		return domain.Subscription{
			ID: "sub_" + cust, TenantID: "t1", CustomerID: cust,
			Status:                    domain.SubscriptionActive,
			CurrentBillingPeriodStart: &apr1,
			CurrentBillingPeriodEnd:   &may1,
			Items:                     []domain.SubscriptionItem{{ID: "itm_" + cust, PlanID: "pln_1", Quantity: 1}},
		}
	}
	customers := &fakeCustomerLookup{customers: map[string]domain.Customer{
		"t1/cus_neg":  {ID: "cus_neg", TenantID: "t1"},
		"t1/cus_list": {ID: "cus_list", TenantID: "t1"},
	}}
	pricing := &fakePricingReader{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "Pro", Currency: "USD", MeterIDs: []string{"mtr_1"}},
		},
		meters: map[string]domain.Meter{
			"mtr_1": {ID: "mtr_1", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: "rrv_1"},
		},
		rules: map[string]domain.RatingRuleVersion{
			// v1 in force at period open (list 1c); v2 published MID-period
			// (5c) must NOT price this window (pin-at-period-open).
			"rrv_1": {ID: "rrv_1", RuleKey: "tokens_flat", Version: 1, LifecycleState: domain.RatingRuleActive,
				Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
				CreatedAt: apr1.Add(-24 * time.Hour)},
			"rrv_2": {ID: "rrv_2", RuleKey: "tokens_flat", Version: 2, LifecycleState: domain.RatingRuleActive,
				Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
				CreatedAt: apr1.Add(10 * 24 * time.Hour)},
		},
		overrides: map[string]domain.CustomerPriceOverride{
			// Negotiated 3c/token for cus_neg only.
			"cus_neg:tokens_flat": {ID: "cpo_1", CustomerID: "cus_neg", RuleKey: "tokens_flat",
				Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(3), Active: true},
		},
		pricingMap: map[string][]domain.MeterPricingRule{},
	}
	store := newAggStore()
	store.aggs["mtr_1"] = []domain.RuleAggregation{
		{RuleID: "", RatingRuleVersionID: "rrv_1", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(1000)},
	}
	usageSvc := NewService(store)

	// Negotiated customer: 1000 × 3c override — not 1c list (v1), not 5c (v2).
	subs := &fakeSubLister{subs: []domain.Subscription{mkSub("cus_neg")}}
	svc := NewCustomerUsageService(usageSvc, customers, subs, pricing)
	res, err := svc.Get(context.Background(), "t1", "cus_neg", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("negotiated: %v", err)
	}
	if res.Meters[0].TotalAmountCents != 3000 {
		t.Errorf("negotiated customer amount: got %d, want 3000 (override; 1000 = list v1 leak, 5000 = mid-period v2 leak)", res.Meters[0].TotalAmountCents)
	}

	// Non-overridden customer: v1 list price — the PERIOD-OPEN version,
	// never the mid-period v2.
	subs2 := &fakeSubLister{subs: []domain.Subscription{mkSub("cus_list")}}
	svc2 := NewCustomerUsageService(usageSvc, customers, subs2, pricing)
	res2, err := svc2.Get(context.Background(), "t1", "cus_list", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("list-price: %v", err)
	}
	if res2.Meters[0].TotalAmountCents != 1000 {
		t.Errorf("list customer amount: got %d, want 1000 (v1 at period open; 5000 means the mid-period publish repriced the window)", res2.Meters[0].TotalAmountCents)
	}
}
