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
	plans  map[string]domain.Plan
	meters map[string]domain.Meter
}

func (f *fakePricingReader) GetPlan(_ context.Context, _, id string) (domain.Plan, error) {
	p, ok := f.plans[id]
	if !ok {
		return domain.Plan{}, errs.ErrNotFound
	}
	return p, nil
}

func (f *fakePricingReader) GetMeter(_ context.Context, _, id string) (domain.Meter, error) {
	m, ok := f.meters[id]
	if !ok {
		return domain.Meter{}, errs.ErrNotFound
	}
	return m, nil
}

// fakeRater answers each subscription's rated usage, and records which
// read the service asked for.
type fakeRater struct {
	current map[string]domain.RatedSubscription
	window  map[string]domain.RatedSubscription
	calls   []string
}

func (f *fakeRater) RateCurrentPeriod(_ context.Context, sub domain.Subscription) (domain.RatedSubscription, error) {
	f.calls = append(f.calls, "current:"+sub.ID)
	return f.current[sub.ID], nil
}

func (f *fakeRater) RateWindow(_ context.Context, sub domain.Subscription, _, _ time.Time) (domain.RatedSubscription, error) {
	f.calls = append(f.calls, "window:"+sub.ID)
	return f.window[sub.ID], nil
}

func TestResolvePeriod(t *testing.T) {
	apr1 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	may1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	jun1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	leap1 := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	leapDec31End := time.Date(2028, 12, 31, 23, 59, 59, 999_999_000, time.UTC)

	oct1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	oct10 := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	oct24 := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	nov1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	nov24 := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

	subWithCycle := func(start, end time.Time) domain.Subscription {
		return domain.Subscription{
			ID:                        "sub_1",
			TenantID:                  "t1",
			Status:                    domain.SubscriptionActive,
			CurrentBillingPeriodStart: &start,
			CurrentBillingPeriodEnd:   &end,
		}
	}
	trialingWithCycle := func(start, end time.Time) domain.Subscription {
		sub := subWithCycle(start, end)
		sub.ID = "sub_trial"
		sub.Status = domain.SubscriptionTrialing
		return sub
	}

	cases := []struct {
		name        string
		period      CustomerUsagePeriod
		subs        []domain.Subscription
		wantSource  string
		wantFrom    time.Time
		wantTo      time.Time
		wantErrCode string
		now         time.Time // customer's effective now; zero → jun1
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
			// Sub A active Oct 1 → Nov 1. On Oct 10 sub B starts a 14-day
			// trial; its stored period is the first PAID one, Oct 24 → Nov 24.
			// Until Oct 24 the default must stay on A's running cycle.
			name: "future trial cycle is skipped while another cycle runs",
			subs: []domain.Subscription{
				subWithCycle(oct1, nov1),
				trialingWithCycle(oct24, nov24),
			},
			now:        oct10,
			wantSource: "current_billing_cycle",
			wantFrom:   oct1,
			wantTo:     nov1,
		},
		{
			name: "trial cycle is picked once it has started",
			subs: []domain.Subscription{
				subWithCycle(oct1, nov1),
				trialingWithCycle(oct24, nov24),
			},
			now:        oct24,
			wantSource: "current_billing_cycle",
			wantFrom:   oct24,
			wantTo:     nov24,
		},
		{
			// No cycle has started: keep the long-standing pick (latest start).
			name:       "only a future trial cycle falls back to it",
			subs:       []domain.Subscription{trialingWithCycle(oct24, nov24)},
			now:        oct10,
			wantSource: "current_billing_cycle",
			wantFrom:   oct24,
			wantTo:     nov24,
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
			now := tc.now
			if now.IsZero() {
				now = jun1 // after every default-case cycle start above
			}
			from, to, source, err := resolvePeriod(tc.period, tc.subs, func() time.Time { return now })
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

// usageFixture is one customer with one active subscription on plan pln_1
// (USD) whose plan prices meter mtr_tokens.
func usageFixture(rated domain.RatedSubscription) (*CustomerUsageService, *fakeRater, *fakeSubLister) {
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	subs := &fakeSubLister{subs: []domain.Subscription{{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Status: domain.SubscriptionActive,
		CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: &end,
		Items: []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_1"}},
	}}}
	pricing := &fakePricingReader{
		plans:  map[string]domain.Plan{"pln_1": {ID: "pln_1", Name: "Pro", Currency: "USD"}},
		meters: map[string]domain.Meter{"mtr_tokens": {ID: "mtr_tokens", EventName: "tokens", Name: "Tokens", Unit: "tokens"}},
	}
	rater := &fakeRater{current: map[string]domain.RatedSubscription{"sub_1": rated}, window: map[string]domain.RatedSubscription{"sub_1": rated}}
	svc := NewCustomerUsageService(NewService(newMemStore()), &fakeCustomerLookup{customers: map[string]domain.Customer{"t1/cus_1": {ID: "cus_1"}}}, subs, pricing)
	svc.SetRater(rater)
	return svc, rater, subs
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// The customer page prices usage through billing's rater (ADR-130 §10):
// with no window, each subscription's current period, exactly as its close
// would bill it; with an explicit window, an estimate. Rows are by price,
// with the plan, and usage no price took is a no_price row outside the
// totals.
func TestCustomerUsage_RowsByPrice(t *testing.T) {
	svc, rater, _ := usageFixture(domain.RatedSubscription{
		MeterIDs: []string{"mtr_tokens"},
		Usage: []domain.RatedUsage{
			{PlanID: "pln_1", MeterID: "mtr_tokens", PriceID: "prc_a", DimensionMatch: map[string]string{"model": "a"}, Quantity: d("100"), AmountCents: 300, UnitAmountDecimal: d("3")},
			{PlanID: "pln_1", MeterID: "mtr_tokens", PriceID: "prc_a", DimensionMatch: map[string]string{"model": "a"}, Quantity: d("50"), AmountCents: 150, UnitAmountDecimal: d("3")},
			{PlanID: "pln_1", MeterID: "mtr_tokens", PriceID: "prc_all", Quantity: d("10"), AmountCents: 10, UnitAmountDecimal: d("1")},
			{PlanID: "pln_1", MeterID: "mtr_tokens", Quantity: d("7")},
			{PlanID: "pln_1", MeterID: "mtr_tokens", PriceID: "prc_all", Quantity: d("-2"), BelowZero: true},
		},
	})
	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rater.calls) != 1 || rater.calls[0] != "current:sub_1" {
		t.Errorf("rater calls = %v, want the current period of sub_1", rater.calls)
	}
	if len(res.Meters) != 1 {
		t.Fatalf("meters = %+v", res.Meters)
	}
	m := res.Meters[0]
	if m.EventName != "tokens" || m.Currency != "USD" {
		t.Errorf("meter = %+v, want tokens in the plan's USD", m)
	}
	if !m.TotalQuantity.Equal(d("160")) || m.TotalAmountCents != 460 {
		t.Errorf("totals = %s / %d, want 160 / 460 (the no_price and below-zero rows left out)", m.TotalQuantity, m.TotalAmountCents)
	}
	if len(m.Prices) != 3 {
		t.Fatalf("price rows = %+v, want prc_a (two windows added), prc_all and no_price", m.Prices)
	}
	a := m.Prices[0]
	if a.PriceID != "prc_a" || a.PlanID != "pln_1" || !a.Quantity.Equal(d("150")) || a.AmountCents != 450 || a.UnitAmountDecimal == nil || *a.UnitAmountDecimal != "3" || a.DimensionMatch["model"] != "a" {
		t.Errorf("prc_a row = %+v", a)
	}
	if np := m.Prices[2]; !np.NoPrice || np.PriceID != "" || !np.Quantity.Equal(d("7")) || np.AmountCents != 0 {
		t.Errorf("no_price row = %+v", np)
	}
	if len(res.Totals) != 1 || res.Totals[0].AmountCents != 460 {
		t.Errorf("totals = %+v, want USD 460", res.Totals)
	}
	var sawNoPrice, sawBelowZero bool
	for _, w := range res.Warnings {
		sawNoPrice = sawNoPrice || strings.Contains(w, "matches no price")
		sawBelowZero = sawBelowZero || strings.Contains(w, "below zero")
	}
	if !sawNoPrice || !sawBelowZero {
		t.Errorf("warnings = %v, want the no_price and the below-zero ones", res.Warnings)
	}
}

// An explicit window is an estimate (ADR-130 §10): it reads RateWindow, and
// period.source "explicit" says so. The estimate is not a warning: warnings
// are problems found working out the usage, and the dashboard shows them as
// problems.
func TestCustomerUsage_ExplicitWindowIsAnEstimate(t *testing.T) {
	svc, rater, _ := usageFixture(domain.RatedSubscription{MeterIDs: []string{"mtr_tokens"}})
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{From: from, To: from.AddDate(0, 0, 10)})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rater.calls) != 1 || rater.calls[0] != "window:sub_1" {
		t.Errorf("rater calls = %v, want the window read", rater.calls)
	}
	if res.Period.Source != "explicit" {
		t.Errorf("source = %q, want explicit", res.Period.Source)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none: the window has no problem, and source says it is an estimate", res.Warnings)
	}
	if len(res.Meters) != 1 || len(res.Meters[0].Prices) != 0 {
		t.Errorf("a meter with no usage is listed with no rows: %+v", res.Meters)
	}
}

func TestCustomerUsage_RequiresTheRater(t *testing.T) {
	svc, _, _ := usageFixture(domain.RatedSubscription{})
	svc.SetRater(nil)
	if _, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{}); err == nil {
		t.Error("Get with no rater wired: nil error, want one")
	}
}

func TestCustomerUsage_CustomerNotFoundPropagates(t *testing.T) {
	svc, _, _ := usageFixture(domain.RatedSubscription{})
	if _, err := svc.Get(context.Background(), "t1", "cus_missing", CustomerUsagePeriod{}); err == nil {
		t.Error("unknown customer: nil error")
	}
	if _, err := svc.Get(context.Background(), "t1", " ", CustomerUsagePeriod{}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("empty customer id: %v, want a validation error", err)
	}
}

// The customer-facing cost dashboard never shows the no_price row: a
// pricing gap is operator information. Its meter totals also leave it out.
func TestCostDashboard_ProjectionExcludesNoPriceRows(t *testing.T) {
	svc, _, subs := usageFixture(domain.RatedSubscription{
		MeterIDs: []string{"mtr_tokens"},
		Usage: []domain.RatedUsage{
			{PlanID: "pln_1", MeterID: "mtr_tokens", PriceID: "prc_all", Quantity: d("1000"), AmountCents: 1000, UnitAmountDecimal: d("1")},
			{PlanID: "pln_1", MeterID: "mtr_tokens", Quantity: d("5")},
		},
	})
	lookup := fakeTokenLookup{cust: domain.Customer{ID: "cus_1", TenantID: "t1", Livemode: false}}
	a := NewCostDashboardAssembler(lookup, svc, subs, &fakeCredits{})
	a.SetLiveBalanceReader(&fakeLive{})
	got, err := a.GetByToken(context.Background(), "vlx_pcd_whatever")
	if err != nil {
		t.Fatalf("GetByToken: %v", err)
	}
	proj := got.(CostDashboardProjection)
	if len(proj.Usage) != 1 || len(proj.Usage[0].Prices) != 1 || proj.Usage[0].TotalQuantity != "1000" {
		t.Fatalf("public usage = %+v, want the one priced row", proj.Usage)
	}
	raw, _ := json.Marshal(proj)
	for _, leak := range []string{"prc_all", "pln_1", "price_id", "plan_id", "rule_key"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("public projection carries %q: %s", leak, raw)
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

// A row can name a plan the subscription swapped away from this period: the
// close still bills it. Its plan comes from the rater, not the items, so the
// meter has the plan's currency and counts in the totals; the no_price
// warning names that plan. A row naming a plan nobody returned fails loud
// rather than drop the meter from the totals.
func TestCustomerUsage_RowOnAPlanSwappedAway(t *testing.T) {
	old := domain.Plan{ID: "pln_old", Name: "Old", Currency: "USD"}
	svc, _, _ := usageFixture(domain.RatedSubscription{
		MeterIDs: []string{"mtr_tokens"},
		Plans:    map[string]domain.Plan{"pln_1": {ID: "pln_1", Name: "Pro", Currency: "USD"}, "pln_old": old},
		Usage: []domain.RatedUsage{
			{PlanID: "pln_old", MeterID: "mtr_tokens", PriceID: "prc_old", Quantity: d("7"), AmountCents: 21, UnitAmountDecimal: d("3")},
			{PlanID: "pln_old", MeterID: "mtr_tokens", Quantity: d("2")},
		},
	})
	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m := res.Meters[0]; m.Currency != "USD" || m.TotalAmountCents != 21 {
		t.Errorf("meter = %+v, want USD 21", m)
	}
	if len(res.Totals) != 1 || res.Totals[0].Currency != "USD" || res.Totals[0].AmountCents != 21 {
		t.Errorf("totals = %+v, want [USD 21]", res.Totals)
	}
	var named bool
	for _, w := range res.Warnings {
		named = named || strings.Contains(w, `plan "Old"`)
	}
	if !named {
		t.Errorf("warnings = %v, want the no_price one naming plan Old", res.Warnings)
	}

	svc, _, _ = usageFixture(domain.RatedSubscription{
		MeterIDs: []string{"mtr_tokens"},
		Usage:    []domain.RatedUsage{{PlanID: "pln_gone", MeterID: "mtr_tokens", PriceID: "prc_x", Quantity: d("1"), AmountCents: 1}},
	})
	if _, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{}); err == nil || !strings.Contains(err.Error(), "pln_gone") {
		t.Errorf("Get with a row on an unknown plan: err = %v, want one naming pln_gone", err)
	}
}
