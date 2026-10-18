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

// capWindowFixture is a subscription with mtr_a on it all period (100 units)
// and one more item, itX, that carries meter on its plan only over [from, to):
// removed at to, or added at from. The meter has 100 units in that window and
// 200 over the invoice's whole window, so the invoice bills 200 units.
func capWindowFixture(capUnits int64, meter string, itX domain.ItemInterval, from, to time.Time, sub domain.Subscription) (*mockSubs, *mockUsage, *mockPricing) {
	sub.UsageCapUnits, sub.OverageAction = &capUnits, "block"
	sub.Items = []domain.SubscriptionItem{{ID: "it_cur", PlanID: "pln_cur", Quantity: 1}}
	if itX.EndsAt == nil {
		sub.Items = append(sub.Items, domain.SubscriptionItem{ID: itX.SubscriptionItemID, PlanID: itX.PlanID, Quantity: 1})
	}
	subs := &mockSubs{
		subs:         map[string]domain.Subscription{sub.ID: sub},
		cycleUpdated: make(map[string]bool),
		intervals:    []domain.ItemInterval{{SubscriptionItemID: "it_cur", PlanID: "pln_cur", Quantity: 1}, itX},
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_cur": {ID: "pln_cur", Name: "Current", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_a"}},
			"pln_x": {ID: "pln_x", Name: "Other", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{meter}},
		},
		meters: map[string]domain.Meter{},
		rules:  map[string]domain.RatingRuleVersion{},
	}
	usage := &mockUsage{}
	capMeters(pricing, usage)
	usage.totals[meter] = 200
	usage.perInterval = map[string]int64{mockIntervalKey(meter, from, to): 100}
	bucket := func(q int64) []domain.RuleAggregation {
		return []domain.RuleAggregation{{RuleID: "mpr_tok", RatingRuleVersionID: "rrv_tok", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(q)}}
	}
	usage.ruleAggs["mtr_tok"] = bucket(200)
	usage.ruleAggsByWindow = map[string][]domain.RuleAggregation{mockIntervalKey("mtr_tok", from, to): bucket(100)}
	return subs, usage, pricing
}

// The cap counts each meter over the windows the invoice bills it for, not
// over the whole period (#1261 review, finding 1). The invoice bills 100
// units of mtr_a and 100 of the other meter, so a cap of 250 is not passed
// and a cap of 150 scales both to 75. Counting the other meter's 200 units
// over the whole period scaled the invoice too far: 83.33 each at a cap of
// 250 and 50 each at 150.
func TestRunCycle_UsageCap_CountsOnlyTheWindowsItBills(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mar15 := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	others := []struct {
		name     string
		meter    string
		itX      domain.ItemInterval
		from, to time.Time
	}{
		{"own rating rule, item removed Mar 15", "mtr_b",
			domain.ItemInterval{SubscriptionItemID: "it_x", PlanID: "pln_x", Quantity: 1, EndsAt: &mar15}, periodStart, mar15},
		{"pricing rule, item removed Mar 15", "mtr_tok",
			domain.ItemInterval{SubscriptionItemID: "it_x", PlanID: "pln_x", Quantity: 1, EndsAt: &mar15}, periodStart, mar15},
		{"own rating rule, item added Mar 15", "mtr_b",
			domain.ItemInterval{SubscriptionItemID: "it_x", PlanID: "pln_x", Quantity: 1, StartsAt: mar15}, mar15, periodEnd},
	}
	caps := []struct {
		capUnits int64
		each     string
	}{
		{250, "100"},
		{150, "75"},
	}
	for _, o := range others {
		for _, c := range caps {
			t.Run(fmt.Sprintf("%s, cap %d", o.name, c.capUnits), func(t *testing.T) {
				subs, usage, pricing := capWindowFixture(c.capUnits, o.meter, o.itX, o.from, o.to, domain.Subscription{
					ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
					Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
					CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
					NextBillingAt: &periodEnd,
				})
				invoices := &mockInvoices{}
				engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

				if n, errs := engine.RunCycle(context.Background(), 50, nil); n != 1 || len(errs) != 0 {
					t.Fatalf("RunCycle = %d, %v; want 1 invoice", n, errs)
				}
				wantCapped(t, invoices.lineItems, c.capUnits, map[string]string{"mtr_a": c.each, o.meter: c.each})
			})
		}
	}
}

// The cancel's final invoice counts the same way: an item removed on Mar 10
// bills [Mar 1, Mar 10), and its usage after the removal does not count.
func TestBillFinalOnImmediateCancel_UsageCap_CountsOnlyTheWindowsItBills(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	removedAt := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		capUnits int64
		each     string
	}{{250, "100"}, {150, "75"}} {
		t.Run(fmt.Sprintf("cap %d", c.capUnits), func(t *testing.T) {
			itX := domain.ItemInterval{SubscriptionItemID: "it_x", PlanID: "pln_x", Quantity: 1, EndsAt: &removedAt}
			subs, usage, pricing := capWindowFixture(c.capUnits, "mtr_b", itX, periodStart, removedAt, domain.Subscription{
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "cap",
				Status:                    domain.SubscriptionCanceled,
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				CanceledAt: &cancelAt,
			})
			invoices := &mockInvoices{}
			engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

			if _, err := engine.BillFinalOnImmediateCancel(context.Background(), subs.subs["sub_1"]); err != nil {
				t.Fatalf("BillFinalOnImmediateCancel: %v", err)
			}
			wantCapped(t, invoices.lineItems, c.capUnits, map[string]string{"mtr_a": c.each, "mtr_b": c.each})
		})
	}
}

// lateEventsUsage is a usage store whose every read of a window after the
// first sees 60 more units, as if events landed between two reads of one
// invoice (the close runs inside the late-event grace window).
type lateEventsUsage struct {
	*mockUsage
	reads map[string]int
}

func (l *lateEventsUsage) late(key string) decimal.Decimal {
	n := l.reads[key]
	l.reads[key]++
	return decimal.NewFromInt(int64(60 * n))
}

func (l *lateEventsUsage) AggregateForBillingPeriodByAgg(ctx context.Context, tenantID, customerID string, meters map[string]string, from, to time.Time) (map[string]decimal.Decimal, error) {
	out, err := l.mockUsage.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID, meters, from, to)
	if err != nil {
		return nil, err
	}
	for id, q := range out {
		out[id] = q.Add(l.late("total|" + mockIntervalKey(id, from, to)))
	}
	return out, nil
}

func (l *lateEventsUsage) AggregateByPricingRules(ctx context.Context, tenantID, customerID, meterID string, mode domain.AggregationMode, from, to time.Time) ([]domain.RuleAggregation, error) {
	aggs, err := l.mockUsage.AggregateByPricingRules(ctx, tenantID, customerID, meterID, mode, from, to)
	if err != nil {
		return nil, err
	}
	extra := l.late("rules|" + mockIntervalKey(meterID, from, to))
	out := make([]domain.RuleAggregation, 0, len(aggs))
	for _, a := range aggs {
		a.Quantity = a.Quantity.Add(extra)
		out = append(out, a)
	}
	return out, nil
}

// The cap and the lines bill from the same reads (#1261 review, finding 2).
// When the cap read the usage apart from the lines, events landing between
// the two reads billed above the cap: the tokens meter counted 100 for the
// cap and billed 160 scaled by 2/3, 240 units against a cap of 200.
func TestUsageCap_EventsBetweenReadsCannotPassTheCap(t *testing.T) {
	const capUnits = int64(200)
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	plan := domain.Plan{ID: "pln_1", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_a", "mtr_b", "mtr_tok"}}
	want := map[string]string{"mtr_a": "66.666666666666", "mtr_b": "66.666666666666", "mtr_tok": "66.666666666666"}

	t.Run("period close", func(t *testing.T) {
		engine, subs, usage, pricing, invoices := setupEngine()
		sub := subs.subs["sub_1"]
		sub.UsageCapUnits, sub.OverageAction = func() *int64 { c := capUnits; return &c }(), "block"
		subs.subs["sub_1"] = sub
		pricing.plans["pln_1"] = plan
		capMeters(pricing, usage)
		engine.usage = &lateEventsUsage{mockUsage: usage, reads: map[string]int{}}

		if n, errs := engine.RunCycle(context.Background(), 50, nil); n != 1 || len(errs) != 0 {
			t.Fatalf("RunCycle = %d, %v; want 1 invoice", n, errs)
		}
		wantCapped(t, invoices.lineItems, capUnits, want)
	})
	t.Run("cancel", func(t *testing.T) {
		c := capUnits
		sub := domain.Subscription{
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "cap",
			Status:                    domain.SubscriptionCanceled,
			Items:                     []domain.SubscriptionItem{{ID: "it_1", PlanID: "pln_1", Quantity: 1}},
			CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
			CanceledAt: &cancelAt, UsageCapUnits: &c, OverageAction: "block",
		}
		pricing := &mockPricing{plans: map[string]domain.Plan{"pln_1": plan}, meters: map[string]domain.Meter{}, rules: map[string]domain.RatingRuleVersion{}}
		usage := &mockUsage{}
		capMeters(pricing, usage)
		invoices := &mockInvoices{}
		engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}},
			&lateEventsUsage{mockUsage: usage, reads: map[string]int{}}, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

		if _, err := engine.BillFinalOnImmediateCancel(context.Background(), sub); err != nil {
			t.Fatalf("BillFinalOnImmediateCancel: %v", err)
		}
		wantCapped(t, invoices.lineItems, capUnits, want)
	})
}

// graduated500 bills 10¢ a unit up to 500 units and 2¢ after.
func graduated500() domain.RatingRuleVersion {
	return domain.RatingRuleVersion{ID: "rrv_api", RuleKey: "api_calls", Version: 1, Mode: domain.PricingGraduated,
		GraduatedTiers: []domain.RatingTier{{UpTo: 500, UnitAmountCents: decimal.NewFromInt(10)}, {UpTo: 0, UnitAmountCents: decimal.NewFromInt(2)}}}
}

// fireLine is the threshold invoice's usage line: 700 units of mtr_api,
// F(700) = 500 x 10¢ + 200 x 2¢ = 5400¢.
func fireLine() domain.InvoiceLineItem {
	return domain.InvoiceLineItem{InvoiceID: "inv_thr", LineType: domain.LineTypeUsage, MeterID: "mtr_api", RatingRuleVersionID: "rrv_api",
		Quantity: 700, QuantityDecimal: decimal.NewFromInt(700), AmountCents: 5400, TotalAmountCents: 5400}
}

// After a threshold invoice billed 700 of the period's 1000 units, the close
// bills the last 300. Tiers continue from the fire (300 x 2¢ = 600¢) unless a
// block usage cap binds; then the close scales the 300 units and prices them
// from tier 0 (ADR-066). A cap the usage does not pass leaves the tiers on
// (#1261 review, finding 3).
var ladderCapCases = []struct {
	name     string
	capUnits int64 // 0: no cap
	qty      string
	amount   int64
}{
	{"no cap", 0, "300", 600},
	{"cap not passed", 2000, "300", 600},
	{"cap passed", 500, "150", 1500}, // 1000 units against 500: 300 x 0.5, at 10¢ from tier 0
}

func setCap(sub *domain.Subscription, capUnits int64) {
	if capUnits > 0 {
		sub.UsageCapUnits, sub.OverageAction = &capUnits, "block"
	}
}

func TestRunCycle_AfterThresholdFire_TiersContinueUnlessTheCapBinds(t *testing.T) {
	for _, tc := range ladderCapCases {
		t.Run(tc.name, func(t *testing.T) {
			subs, _, pricing, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000})
			pricing.rules["rrv_api"] = graduated500()
			seedThresholdInvoice(invoices, domain.InvoiceFinalized)
			invoices.lineItems = append(invoices.lineItems, fireLine())
			sub := subs.subs["sub_1"]
			setCap(&sub, tc.capUnits)
			subs.subs["sub_1"] = sub

			if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
				t.Fatalf("RunCycle errors: %v", errs)
			}
			_, usage := cycleInvoiceLines(t, invoices, invoices.invoices[1].ID)
			if len(usage) != 1 || !usage[0].QuantityDecimal.Equal(decimal.RequireFromString(tc.qty)) || usage[0].AmountCents != tc.amount {
				t.Fatalf("usage lines = %+v, want one line of %s units for %d¢", usage, tc.qty, tc.amount)
			}
		})
	}
}

// The cancel's final invoice after a fire takes the same switch: the fire
// billed [Mar 1, Mar 10), the cancel bills the 300 units of [Mar 10, Mar 15).
func TestBillFinalOnImmediateCancel_AfterThresholdFire_TiersContinueUnlessTheCapBinds(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	fireEnd := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	for _, tc := range ladderCapCases {
		t.Run(tc.name, func(t *testing.T) {
			sub := domain.Subscription{
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "ladder",
				Status:                    domain.SubscriptionCanceled,
				Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_1", Quantity: 1}},
				CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
				CanceledAt: &cancelAt,
			}
			setCap(&sub, tc.capUnits)
			pricing := &mockPricing{
				plans: map[string]domain.Plan{"pln_1": {ID: "pln_1", Name: "P", Currency: "USD", BillingInterval: domain.BillingMonthly,
					BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_api"}}},
				meters: map[string]domain.Meter{"mtr_api": {ID: "mtr_api", Key: "api_calls", Name: "API", Unit: "calls", Aggregation: "sum", RatingRuleVersionID: "rrv_api"}},
				rules:  map[string]domain.RatingRuleVersion{"rrv_api": graduated500()},
			}
			usage := &mockUsage{
				totals:      map[string]int64{"mtr_api": 1000}, // [Mar 1, Mar 15)
				perInterval: map[string]int64{mockIntervalKey("mtr_api", fireEnd, cancelAt): 300},
			}
			invoices := &mockInvoices{
				invoices: []domain.Invoice{{
					ID: "inv_thr", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1",
					InvoiceNumber: "VLX-THR-1", BillingReason: domain.BillingReasonThreshold,
					Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
					BillingPeriodStart: periodStart, BillingPeriodEnd: fireEnd,
				}},
				lineItems: []domain.InvoiceLineItem{fireLine()},
			}
			engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

			inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
			if err != nil {
				t.Fatalf("BillFinalOnImmediateCancel: %v", err)
			}
			_, lines := cycleInvoiceLines(t, invoices, inv.ID)
			if len(lines) != 1 || !lines[0].QuantityDecimal.Equal(decimal.RequireFromString(tc.qty)) || lines[0].AmountCents != tc.amount {
				t.Fatalf("usage lines = %+v, want one line of %s units for %d¢", lines, tc.qty, tc.amount)
			}
		})
	}
}

// The next-invoice preview takes the close's switch too (#1261 review,
// finding 4): it turned the tiers off for any cap, so a cap the usage does
// not pass showed 3000¢ for usage the close bills at 600¢. The preview does
// not scale usage by the cap (its warning says so), so a binding cap shows
// the 300 units from tier 0.
func TestPreview_AfterThresholdFire_TiersContinueUnlessTheCapBinds(t *testing.T) {
	previewAmount := map[string]int64{"no cap": 600, "cap not passed": 600, "cap passed": 3000}
	for _, tc := range ladderCapCases {
		t.Run(tc.name, func(t *testing.T) {
			subs, usage, pricing, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000})
			pricing.rules["rrv_api"] = graduated500()
			seedThresholdInvoice(invoices, domain.InvoiceFinalized)
			invoices.lineItems = append(invoices.lineItems, fireLine())
			fireEnd := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
			periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
			usage.ruleAggsByWindow = map[string][]domain.RuleAggregation{
				mockIntervalKey("mtr_api", fireEnd, periodEnd): {{AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(300)}},
			}
			sub := subs.subs["sub_1"]
			setCap(&sub, tc.capUnits)

			res, err := engine.Preview(context.Background(), sub)
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			var amount int64
			for _, l := range res.Lines {
				if l.LineType == "usage" {
					amount += l.AmountCents
				}
			}
			if amount != previewAmount[tc.name] {
				t.Errorf("preview usage = %d¢, want %d¢", amount, previewAmount[tc.name])
			}
		})
	}
}

// An upgrade mid-period between two plans that carry the same meters bills
// each meter once over the whole period, so the cap counts it once. For a max
// meter the peaks of the two halves (80 and 100) add up to more than the
// period's peak (100): counting the halves apart scaled the invoice too far,
// 53.571428571428 units each at a cap of 150 instead of 75.
func TestRunCycle_UsageCap_SameMeterAcrossAPlanSwapCountsOnce(t *testing.T) {
	capUnits := int64(150)
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mar15 := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	subs := &mockSubs{
		subs: map[string]domain.Subscription{"sub_1": {
			ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
			Items:  []domain.SubscriptionItem{{ID: "it_1", PlanID: "pln_pro", Quantity: 1}},
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
			CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
			NextBillingAt: &periodEnd,
			UsageCapUnits: &capUnits, OverageAction: "block",
		}},
		cycleUpdated: make(map[string]bool),
		// it_1 moved from Starter to Pro on Mar 15.
		intervals: []domain.ItemInterval{
			{SubscriptionItemID: "it_1", PlanID: "pln_starter", Quantity: 1, EndsAt: &mar15},
			{SubscriptionItemID: "it_1", PlanID: "pln_pro", Quantity: 1, StartsAt: mar15},
		},
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_starter": {ID: "pln_starter", Name: "Starter", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_a", "mtr_b"}},
			"pln_pro": {ID: "pln_pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_a", "mtr_b"}},
		},
		meters: map[string]domain.Meter{},
		rules:  map[string]domain.RatingRuleVersion{},
	}
	usage := &mockUsage{}
	capMeters(pricing, usage) // 100 units of each meter over the whole period
	maxMeter := pricing.meters["mtr_a"]
	maxMeter.Aggregation = "max"
	pricing.meters["mtr_a"] = maxMeter
	usage.perInterval = map[string]int64{
		mockIntervalKey("mtr_a", periodStart, mar15): 80,
		mockIntervalKey("mtr_a", mar15, periodEnd):   100,
		mockIntervalKey("mtr_b", periodStart, mar15): 40,
		mockIntervalKey("mtr_b", mar15, periodEnd):   60,
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	if n, errs := engine.RunCycle(context.Background(), 50, nil); n != 1 || len(errs) != 0 {
		t.Fatalf("RunCycle = %d, %v; want 1 invoice", n, errs)
	}
	wantCapped(t, invoices.lineItems, capUnits, map[string]string{"mtr_a": "75", "mtr_b": "75"})
	lines := map[string]int{}
	for _, li := range invoices.lineItems {
		if li.LineType != domain.LineTypeUsage {
			continue
		}
		lines[li.MeterID]++
		if li.BillingPeriodStart == nil || !li.BillingPeriodStart.Equal(periodStart) || li.BillingPeriodEnd == nil || !li.BillingPeriodEnd.Equal(periodEnd) {
			t.Errorf("%s line covers [%v, %v), want the whole period", li.MeterID, li.BillingPeriodStart, li.BillingPeriodEnd)
		}
	}
	if lines["mtr_a"] != 1 || lines["mtr_b"] != 1 {
		t.Errorf("usage lines per meter = %v, want one each", lines)
	}
}
