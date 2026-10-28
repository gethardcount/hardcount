package billing_test

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/usage"
)

// TestCustomerUsage_PlanSwappedAwayThisPeriod_CountsInTotals (review round 3
// of #1386): the close bills a meter's usage on the plan a subscription
// swapped away from this period. Customer usage, the public cost dashboard's
// projected total and the margin report read the same rated rows, so each
// counts that usage in the plan's currency. The meter's currency comes from
// the plan the rater read for the row, not from the current items' plans.
//
// Mutation: take the meter's currency from the current items' plans only,
// and the meter has currency "", its 2000c leave the totals and the
// projected total, and the margin report names no revenue currency.
func TestCustomerUsage_PlanSwappedAwayThisPeriod_CountsInTotals(t *testing.T) {
	lf := newLiveFixture(t, "Customer Usage Swapped Plan", 0)

	tok, err := lf.pricingSvc.CreateMeter(lf.ctx, lf.tenantID, pricing.CreateMeterInput{
		EventName: "tok", Name: "Tok", Unit: "tok", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	planB, _, err := lf.pricingSvc.CreatePlan(lf.ctx, lf.tenantID, pricing.CreatePlanInput{
		Code: "pln_tok", Name: "Tok Plan", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Prices: []pricing.PriceInput{{MeterID: tok.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	lf.ingestUsage(t, lf.ctx, 20, 100) // 2000c of calls on the first plan
	if _, err := lf.subSvc.UpdateItem(lf.ctx, lf.tenantID, lf.subID, lf.itemID, subscription.UpdateItemInput{NewPlanID: planB.ID, Immediate: true}); err != nil {
		t.Fatalf("swap plan: %v", err)
	}

	customers := customer.NewPostgresStore(lf.db)
	view := usage.NewCustomerUsageService(lf.usageSvc, customers, lf.subStore, lf.pricingSvc)
	view.SetRater(lf.engine)

	windows := map[string]usage.CustomerUsagePeriod{
		"current period":  {},
		"explicit window": {From: lf.cycleStart, To: time.Now().UTC().Add(time.Hour)},
	}
	for name, period := range windows {
		got, err := view.Get(lf.ctx, lf.tenantID, lf.customerID, period)
		if err != nil {
			t.Fatalf("%s: customer usage: %v", name, err)
		}
		var calls *usage.CustomerUsageMeter
		for i := range got.Meters {
			if got.Meters[i].MeterID == lf.meterID {
				calls = &got.Meters[i]
			}
		}
		if calls == nil {
			t.Fatalf("%s: meter %s missing from %+v", name, lf.meterID, got.Meters)
		}
		if calls.Currency != "USD" || calls.TotalAmountCents != 2000 {
			t.Errorf("%s: calls meter currency=%q amount=%d, want USD 2000", name, calls.Currency, calls.TotalAmountCents)
		}
		if len(got.Totals) != 1 || got.Totals[0].Currency != "USD" || got.Totals[0].AmountCents != 2000 {
			t.Errorf("%s: totals = %+v, want [USD 2000]", name, got.Totals)
		}
	}

	proj, err := lf.dashboard.GetByToken(lf.ctx, lf.token)
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if got := proj.(usage.CostDashboardProjection).ProjectedTotalCents; got != 2000 {
		t.Errorf("dashboard projected_total_cents = %d, want 2000", got)
	}

	margin := usage.NewMarginAssembler(usage.NewPostgresStore(lf.db), view)
	rep, err := margin.Get(lf.ctx, lf.tenantID, lf.customerID, lf.cycleStart, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("margin: %v", err)
	}
	if rep.RevenueCurrency != "USD" || rep.RevenueCents != 2000 {
		t.Errorf("margin revenue = %q %d, want USD 2000", rep.RevenueCurrency, rep.RevenueCents)
	}
}
