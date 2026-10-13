package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// unmatchedFixtures builds a tenant with one matrix-priced meter (no
// default binding — the ADR-044 tokens shape) whose aggregation returns
// one matched bucket (1000 units @ 1¢) and one UNCLAIMED bucket (500
// units matching no rule). This is the exact reproduction from the
// 2026-07-20 FLOW B2b walkthrough: a mislabeled dimension value billed
// nothing with only a server-log WARN as evidence.
func unmatchedFixtures() (*CustomerUsageService, *fakeSubLister) {
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
			"pln_1": {ID: "pln_1", Name: "AI API", Currency: "USD", MeterIDs: []string{"mtr_1"}},
		},
		meters: map[string]domain.Meter{
			// NO RatingRuleVersionID: matrix-priced meters carry no
			// default binding, so unclaimed events price nowhere.
			"mtr_1": {ID: "mtr_1", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_1": {
				ID: "rrv_1", RuleKey: "sonnet_input", Mode: domain.PricingFlat,
				Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
			},
		},
		pricingMap: map[string][]domain.MeterPricingRule{
			"mtr_1": {{
				ID: "mpr_1", MeterID: "mtr_1", RatingRuleVersionID: "rrv_1",
				DimensionMatch: map[string]any{"model": "claude-3.5-sonnet", "token_type": "input"},
			}},
		},
	}
	store := newAggStore()
	store.aggs["mtr_1"] = []domain.RuleAggregation{
		{RuleID: "mpr_1", RatingRuleVersionID: "rrv_1", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(1000)},
		// The unclaimed bucket — events whose properties matched no rule.
		{RuleID: "", RatingRuleVersionID: "", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(500)},
	}
	return NewCustomerUsageService(NewService(store), customers, subs, pricing), subs
}

// TestCustomerUsageService_Get_SurfacesUnmatchedBucket locks the operator
// surface: the unclaimed bucket is a first-class row (unmatched=true,
// amount 0), totals keep mirroring the invoice (matched only), and the
// warnings channel names the meter.
func TestCustomerUsageService_Get_SurfacesUnmatchedBucket(t *testing.T) {
	t.Parallel()
	svc, _ := unmatchedFixtures()

	res, err := svc.Get(context.Background(), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Meters) != 1 {
		t.Fatalf("meters: got %d, want 1", len(res.Meters))
	}
	m := res.Meters[0]

	var matched, unmatched *CustomerUsageRule
	for i := range m.Rules {
		if m.Rules[i].Unmatched {
			unmatched = &m.Rules[i]
		} else {
			matched = &m.Rules[i]
		}
	}
	if unmatched == nil {
		t.Fatal("the unclaimed bucket must surface as an unmatched rule row — this is the silent-revenue-leak fix")
	}
	if !unmatched.Quantity.Equal(decimal.NewFromInt(500)) || unmatched.AmountCents != 0 {
		t.Errorf("unmatched row: qty=%s amount=%d, want 500/0", unmatched.Quantity, unmatched.AmountCents)
	}
	if matched == nil || matched.AmountCents != 1000 {
		t.Fatalf("matched row must still rate normally: %+v", matched)
	}

	// Totals mirror the INVOICE: unmatched volume is excluded.
	if !m.TotalQuantity.Equal(decimal.NewFromInt(1000)) || m.TotalAmountCents != 1000 {
		t.Errorf("meter totals must exclude unmatched volume: qty=%s cents=%d, want 1000/1000",
			m.TotalQuantity, m.TotalAmountCents)
	}

	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "no pricing rule") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings must name the unmatched usage; got %v", res.Warnings)
	}
}

// TestCostDashboard_ProjectionExcludesUnmatchedRows locks the boundary:
// the CUSTOMER-facing cost dashboard never shows the unmatched row —
// a pricing-config gap is operator information, not something to
// disclose (or explain) on a public page.
func TestCostDashboard_ProjectionExcludesUnmatchedRows(t *testing.T) {
	t.Parallel()
	svc, subs := unmatchedFixtures()

	lookup := fakeTokenLookup{cust: domain.Customer{ID: "cus_1", TenantID: "t1", Livemode: false}}
	a := NewCostDashboardAssembler(lookup, svc, subs, &fakeCredits{})
	a.SetLiveBalanceReader(&fakeLive{})

	got, err := a.GetByToken(context.Background(), "vlx_pcd_whatever")
	if err != nil {
		t.Fatalf("GetByToken: %v", err)
	}
	proj, ok := got.(CostDashboardProjection)
	if !ok {
		t.Fatalf("projection type: %T", got)
	}
	if len(proj.Usage) != 1 {
		t.Fatalf("usage meters: got %d, want 1", len(proj.Usage))
	}
	rules := proj.Usage[0].Rules
	if len(rules) != 1 {
		t.Fatalf("public projection must carry ONLY the matched rule, got %d rows: %+v", len(rules), rules)
	}
	if rules[0].RuleKey == "" {
		t.Error("the surviving public rule row must be the matched one")
	}
	// The public meter totals also exclude unmatched volume (same
	// invoice-mirroring rule as the operator view).
	if proj.Usage[0].TotalQuantity != "1000" {
		t.Errorf("public total quantity: got %s, want 1000", proj.Usage[0].TotalQuantity)
	}
}
