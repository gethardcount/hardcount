package billing

// #1192: a block-mode usage cap scales an invoice's usage by one factor,
// worked out once from one total, on the period close and on the cancel's
// final invoice alike. Before the fix the close worked the factor out twice:
// once for meters on the current plans, and again (for pricing-rule meters
// and partial windows) only when the first result summed to exactly the cap.
// Decimal division rounds, so with three meters the sum missed the cap by
// 10^-16, the second factor stayed 1, and a pricing-rule meter billed every
// unit. Meters of an item removed mid-period were left out of the first total.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// capMeters adds three meters at 3¢ a unit with 100 units each in any window:
// mtr_a and mtr_b price by their own rating rule, mtr_tok by a pricing rule
// (the multi-dimension path, which reads its buckets raw and scales them).
func capMeters(pricing *mockPricing, usage *mockUsage) {
	for _, id := range []string{"a", "b", "tok"} {
		pricing.rules["rrv_"+id] = domain.RatingRuleVersion{ID: "rrv_" + id, RuleKey: id, Version: 1,
			Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(3)}
	}
	pricing.meters["mtr_a"] = domain.Meter{ID: "mtr_a", Key: "a", Name: "A", Unit: "u", Aggregation: "sum", RatingRuleVersionID: "rrv_a"}
	pricing.meters["mtr_b"] = domain.Meter{ID: "mtr_b", Key: "b", Name: "B", Unit: "u", Aggregation: "sum", RatingRuleVersionID: "rrv_b"}
	pricing.meters["mtr_tok"] = domain.Meter{ID: "mtr_tok", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"}
	pricing.meterPricingRules = map[string][]domain.MeterPricingRule{
		"mtr_tok": {{ID: "mpr_tok", MeterID: "mtr_tok", RatingRuleVersionID: "rrv_tok"}},
	}
	usage.totals = map[string]int64{"mtr_a": 100, "mtr_b": 100, "mtr_tok": 100}
	usage.ruleAggs = map[string][]domain.RuleAggregation{
		"mtr_tok": {{RuleID: "mpr_tok", RatingRuleVersionID: "rrv_tok", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(100)}},
	}
}

// wantCapped checks every meter bills want units, so all of them took the
// same factor, and that the billed units stay within the cap. Each quantity
// must fit the 12 decimal places an invoice line stores, so the stored sum is
// the sum checked here.
func wantCapped(t *testing.T, lines []domain.InvoiceLineItem, capUnits int64, want map[string]string) {
	t.Helper()
	sum := decimal.Zero
	for meterID, w := range want {
		qty, _ := meterBilled(lines, meterID)
		if !qty.Equal(decimal.RequireFromString(w)) {
			t.Errorf("%s billed %s units, want %s", meterID, qty, w)
		}
		if !qty.Equal(qty.Truncate(12)) {
			t.Errorf("%s billed %s units: more decimal places than an invoice line stores (12)", meterID, qty)
		}
		sum = sum.Add(qty)
	}
	if sum.GreaterThan(decimal.NewFromInt(capUnits)) {
		t.Errorf("billed %s units in all, over the cap of %d", sum, capUnits)
	}
}

// The probe from #1192: three meters of 100 units against a cap of 200. Each
// bills 200/3 units, rounded down to 12 places; the pricing-rule meter billed
// all 100 before the fix. A cap the usage does not pass bills every unit.
func TestRunCycle_UsageCap_OneFactorForEveryMeter(t *testing.T) {
	cases := []struct {
		capUnits    int64
		each        string
		subtotalCts int64
	}{
		{200, "66.666666666666", 600}, // 66.666666666666 x 3¢ rounds to 200¢ a meter
		{300, "100", 900},             // the cap is reached, not passed
		{1000, "100", 900},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("cap %d", tc.capUnits), func(t *testing.T) {
			engine, subs, usage, pricing, invoices := setupEngine()
			sub := subs.subs["sub_1"]
			sub.UsageCapUnits, sub.OverageAction = &tc.capUnits, "block"
			subs.subs["sub_1"] = sub
			pricing.plans["pln_1"] = domain.Plan{ID: "pln_1", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_a", "mtr_b", "mtr_tok"}}
			capMeters(pricing, usage)

			if n, errs := engine.RunCycle(context.Background(), 50, nil); n != 1 || len(errs) != 0 {
				t.Fatalf("RunCycle = %d, %v; want 1 invoice", n, errs)
			}
			wantCapped(t, invoices.lineItems, tc.capUnits, map[string]string{"mtr_a": tc.each, "mtr_b": tc.each, "mtr_tok": tc.each})
			if got := invoices.invoices[0].SubtotalCents; got != tc.subtotalCts {
				t.Errorf("subtotal = %d¢, want %d¢", got, tc.subtotalCts)
			}
		})
	}
}

// An item removed mid-period still bills its usage on the close, so the cap
// counts it. Before the fix the first total left it out: with the current
// meters over the cap the close used two factors (75 units of mtr_a, 50 of
// the others, 175 against a cap of 150); with only the removed meter taking
// the usage over the cap, the cap was not applied at all (300 against 250).
func TestRunCycle_UsageCap_ItemRemovedMidPeriod(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	removedAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		capUnits int64
		each     string
	}{
		{150, "50"},
		{250, "83.333333333333"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("cap %d", tc.capUnits), func(t *testing.T) {
			subs := &mockSubs{
				subs: map[string]domain.Subscription{"sub_1": {
					ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
					Items:  []domain.SubscriptionItem{{ID: "it_cur", PlanID: "pln_cur", Quantity: 1}},
					Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
					CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
					NextBillingAt: &periodEnd,
					UsageCapUnits: &tc.capUnits, OverageAction: "block",
				}},
				cycleUpdated: make(map[string]bool),
				// it_old (meter mtr_b) was removed on Mar 15.
				intervals: []domain.ItemInterval{
					{SubscriptionItemID: "it_cur", PlanID: "pln_cur", Quantity: 1},
					{SubscriptionItemID: "it_old", PlanID: "pln_old", Quantity: 1, EndsAt: &removedAt},
				},
			}
			pricing := &mockPricing{
				plans: map[string]domain.Plan{
					"pln_cur": {ID: "pln_cur", Name: "Current", Currency: "USD", BillingInterval: domain.BillingMonthly,
						BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_a", "mtr_tok"}},
					"pln_old": {ID: "pln_old", Name: "Removed", Currency: "USD", BillingInterval: domain.BillingMonthly,
						BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_b"}},
				},
				meters: map[string]domain.Meter{},
				rules:  map[string]domain.RatingRuleVersion{},
			}
			usage := &mockUsage{}
			capMeters(pricing, usage) // mtr_b's 100 units all fall before the removal
			invoices := &mockInvoices{}
			engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

			if n, errs := engine.RunCycle(context.Background(), 50, nil); n != 1 || len(errs) != 0 {
				t.Fatalf("RunCycle = %d, %v; want 1 invoice", n, errs)
			}
			wantCapped(t, invoices.lineItems, tc.capUnits, map[string]string{"mtr_a": tc.each, "mtr_b": tc.each, "mtr_tok": tc.each})
			for _, li := range invoices.lineItems {
				if li.MeterID == "mtr_b" && (li.BillingPeriodEnd == nil || !li.BillingPeriodEnd.Equal(removedAt)) {
					t.Errorf("mtr_b line ends %v, want the removal %v", li.BillingPeriodEnd, removedAt)
				}
			}
		})
	}
}

// The cancel's final invoice uses the same factor as the period close, with
// and without an item removed before the cancel.
func TestBillFinalOnImmediateCancel_UsageCap_SameFactorAsTheClose(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	removedAt := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		capUnits  int64
		removed   bool
		each      string
		subtotalC int64
	}{
		{"three meters", 200, false, "66.666666666666", 600},
		{"item removed before the cancel", 250, true, "83.333333333333", 750},
		{"cap not passed", 1000, false, "100", 900},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := domain.Subscription{
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "cap",
				Status:                    domain.SubscriptionCanceled,
				Items:                     []domain.SubscriptionItem{{ID: "it_cur", PlanID: "pln_cur", Quantity: 1}},
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				CanceledAt:                &cancelAt,
				UsageCapUnits:             &tc.capUnits,
				OverageAction:             "block",
			}
			curMeters := []string{"mtr_a", "mtr_b", "mtr_tok"}
			subs := &mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}
			if tc.removed {
				curMeters = []string{"mtr_a", "mtr_tok"}
				subs.intervals = []domain.ItemInterval{
					{SubscriptionItemID: "it_cur", PlanID: "pln_cur", Quantity: 1},
					{SubscriptionItemID: "it_old", PlanID: "pln_old", Quantity: 1, EndsAt: &removedAt},
				}
			}
			pricing := &mockPricing{
				plans: map[string]domain.Plan{
					"pln_cur": {ID: "pln_cur", Name: "Current", Currency: "USD", BillingInterval: domain.BillingMonthly,
						BaseBillTiming: domain.BillInArrears, MeterIDs: curMeters},
					"pln_old": {ID: "pln_old", Name: "Removed", Currency: "USD", BillingInterval: domain.BillingMonthly,
						BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_b"}},
				},
				meters: map[string]domain.Meter{},
				rules:  map[string]domain.RatingRuleVersion{},
			}
			usage := &mockUsage{}
			capMeters(pricing, usage)
			invoices := &mockInvoices{}
			engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

			inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
			if err != nil {
				t.Fatalf("BillFinalOnImmediateCancel: %v", err)
			}
			wantCapped(t, invoices.lineItems, tc.capUnits, map[string]string{"mtr_a": tc.each, "mtr_b": tc.each, "mtr_tok": tc.each})
			if inv.SubtotalCents != tc.subtotalC {
				t.Errorf("subtotal = %d¢, want %d¢", inv.SubtotalCents, tc.subtotalC)
			}
		})
	}
}
