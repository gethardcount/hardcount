package billing

// #1193: ingest accepts negative quantities as usage corrections, so a usage
// bucket's total over a billing window can be below zero. These tests pin the
// rule every invoice decision follows: that bucket bills 0, the rest of the
// invoice is built, and one subscription.net_negative_usage audit row,
// written on the decision's own transaction, names the meter and the amount.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

const netNegativeAction = "subscription.net_negative_usage"

// netNegativeRows returns the subscription.net_negative_usage rows the fake
// audit writer saw, and fails the test if any of them came through the
// post-commit Log instead of LogInTx.
func netNegativeRows(t *testing.T, a *fakeEngineAudit) []auditRow {
	t.Helper()
	var out []auditRow
	for i, r := range a.rows {
		if r.entry.Action != netNegativeAction {
			continue
		}
		if !a.inTx[i] {
			t.Errorf("%s row was written with Log, not LogInTx: it must ride the decision's transaction", netNegativeAction)
		}
		out = append(out, r)
	}
	return out
}

// noteMeters returns the meter_id → quantity pairs of one row's metadata.
func noteMeters(t *testing.T, r auditRow) map[string]string {
	t.Helper()
	out := map[string]string{}
	for id, m := range noteMeterEntries(t, r) {
		qty, _ := m["quantity"].(string)
		out[id] = qty
	}
	return out
}

// noteMeterEntries returns one row's meters entries by meter_id.
func noteMeterEntries(t *testing.T, r auditRow) map[string]map[string]any {
	t.Helper()
	ms, ok := r.entry.Metadata["meters"].([]map[string]any)
	if !ok {
		t.Fatalf("meters metadata: got %T, want []map[string]any", r.entry.Metadata["meters"])
	}
	out := map[string]map[string]any{}
	for _, m := range ms {
		id, _ := m["meter_id"].(string)
		out[id] = m
	}
	return out
}

// wantNoteWindow checks the window one meter's usage below zero was priced
// over, as the row records it.
func wantNoteWindow(t *testing.T, r auditRow, meterID string, from, to time.Time) {
	t.Helper()
	m, ok := noteMeterEntries(t, r)[meterID]
	if !ok {
		t.Fatalf("row has no entry for %s", meterID)
	}
	if got, want := m["from"], from.UTC().Format(time.RFC3339Nano); got != want {
		t.Errorf("%s from = %v, want %s (the window the invoice priced)", meterID, got, want)
	}
	if got, want := m["to"], to.UTC().Format(time.RFC3339Nano); got != want {
		t.Errorf("%s to = %v, want %s", meterID, got, want)
	}
}

// meterBilled sums the quantity and amount of one meter's invoice lines.
func meterBilled(lines []domain.InvoiceLineItem, meterID string) (decimal.Decimal, int64) {
	qty, amount := decimal.Zero, int64(0)
	for _, li := range lines {
		if li.MeterID == meterID {
			qty = qty.Add(li.QuantityDecimal)
			amount += li.AmountCents
		}
	}
	return qty, amount
}

// A single-rule meter whose period total is -50 bills nothing; the base fee
// and the other meter bill as usual, and the close is recorded.
func TestRunCycle_NetNegativeMeter_BillsZeroAndRecordsIt(t *testing.T) {
	engine, _, usage, pricing, invoices := setupEngine()
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	api := pricing.meters["mtr_api"]
	api.Key = "api_calls"
	pricing.meters["mtr_api"] = api
	usage.totals["mtr_api"] = -50

	n, errs := engine.RunCycle(context.Background(), 50, nil)
	if len(errs) > 0 {
		t.Fatalf("RunCycle errors: %v (a net-negative meter must not fail the close)", errs)
	}
	if n != 1 || len(invoices.invoices) != 1 {
		t.Fatalf("invoices: n=%d stored=%d, want 1", n, len(invoices.invoices))
	}
	inv := invoices.invoices[0]
	if want := int64(4900 + 250*2500); inv.SubtotalCents != want {
		t.Errorf("subtotal = %d, want %d (base + storage, api calls at 0)", inv.SubtotalCents, want)
	}
	for _, li := range invoices.lineItems {
		if li.MeterID == "mtr_api" {
			t.Errorf("api calls line written (%d units, %d¢): a meter below zero has no line", li.Quantity, li.AmountCents)
		}
	}

	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	r := rows[0]
	if r.entry.ResourceType != "subscription" || r.entry.ResourceID != "sub_1" {
		t.Errorf("row resource = %s/%s, want subscription/sub_1", r.entry.ResourceType, r.entry.ResourceID)
	}
	if got := noteMeters(t, r); len(got) != 1 || got["mtr_api"] != "-50" {
		t.Errorf("meters = %v, want mtr_api: -50", got)
	}
	if got := r.entry.Metadata["invoice_id"]; got != inv.ID {
		t.Errorf("invoice_id = %v, want %s", got, inv.ID)
	}
	if got := r.entry.Metadata["billing_reason"]; got != string(domain.BillingReasonSubscriptionCycle) {
		t.Errorf("billing_reason = %v, want subscription_cycle", got)
	}
}

// A multi-rule meter: the bucket above zero bills, the bucket below zero
// bills nothing and is recorded with its own quantity (not the meter's net).
func TestRunCycle_NetNegativeBucket_MultiRuleMeter(t *testing.T) {
	engine, _, usage, pricing, invoices := setupEngine()
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	pricing.meters["mtr_api"] = domain.Meter{ID: "mtr_api", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"}
	pricing.rules["rrv_in"] = domain.RatingRuleVersion{ID: "rrv_in", RuleKey: "tok_in", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)}
	pricing.rules["rrv_out"] = domain.RatingRuleVersion{ID: "rrv_out", RuleKey: "tok_out", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(2)}
	pricing.meterPricingRules = map[string][]domain.MeterPricingRule{
		"mtr_api": {
			{ID: "mpr_in", MeterID: "mtr_api", RatingRuleVersionID: "rrv_in", DimensionMatch: map[string]any{"type": "in"}},
			{ID: "mpr_out", MeterID: "mtr_api", RatingRuleVersionID: "rrv_out", DimensionMatch: map[string]any{"type": "out"}},
		},
	}
	usage.ruleAggs = map[string][]domain.RuleAggregation{
		"mtr_api": {
			{RuleID: "mpr_in", RatingRuleVersionID: "rrv_in", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(1000)},
			{RuleID: "mpr_out", RatingRuleVersionID: "rrv_out", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(-300)},
		},
	}
	usage.totals["mtr_api"] = 700

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("RunCycle errors: %v", errs)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices = %d, want 1", len(invoices.invoices))
	}
	var tokenLines []domain.InvoiceLineItem
	for _, li := range invoices.lineItems {
		if li.MeterID == "mtr_api" {
			tokenLines = append(tokenLines, li)
		}
	}
	if len(tokenLines) != 1 || tokenLines[0].RatingRuleVersionID != "rrv_in" || tokenLines[0].AmountCents != 1000 {
		t.Fatalf("token lines = %+v, want one rrv_in line of 1000¢", tokenLines)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-300" {
		t.Errorf("meters = %v, want mtr_api: -300", got)
	}
}

// A block-mode usage cap counts a meter below zero as 0, the way it bills.
// Counting its -50 lowered the cap's total from 150 to 100, so storage was
// scaled to 120 GB against a cap of 80. The row reports the -50 itself, not
// a cap-scaled share of it.
func TestRunCycle_NetNegativeMeter_UsageCapCountsItAsZero(t *testing.T) {
	engine, subs, usage, _, invoices := setupEngine()
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	sub := subs.subs["sub_1"]
	capUnits := int64(80)
	sub.UsageCapUnits = &capUnits
	sub.OverageAction = "block"
	subs.subs["sub_1"] = sub
	usage.totals["mtr_storage"] = 150
	usage.totals["mtr_api"] = -50

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("RunCycle errors: %v", errs)
	}
	var storage *domain.InvoiceLineItem
	for i, li := range invoices.lineItems {
		if li.MeterID == "mtr_storage" {
			storage = &invoices.lineItems[i]
		}
	}
	if storage == nil {
		t.Fatal("no storage line")
	}
	if !storage.QuantityDecimal.Equal(decimal.NewFromInt(80)) || storage.AmountCents != 80*2500 {
		t.Errorf("storage line = %s GB, %d¢; want 80 GB, %d¢ (the cap)", storage.QuantityDecimal, storage.AmountCents, 80*2500)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-50" {
		t.Errorf("meters = %v, want mtr_api: -50 (the cap does not scale usage below zero)", got)
	}
}

// A period whose only usage is below zero, with no base fee, builds no
// invoice; the period still advances and the close is still recorded.
func TestRunCycle_OnlyNetNegativeUsage_NoInvoice_StillRecorded(t *testing.T) {
	engine, subs, usage, pricing, invoices := setupEngine()
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	plan := pricing.plans["pln_1"]
	plan.BaseAmountCents = 0
	pricing.plans["pln_1"] = plan
	delete(usage.totals, "mtr_storage")
	usage.totals["mtr_api"] = -50

	n, errs := engine.RunCycle(context.Background(), 50, nil)
	if len(errs) > 0 {
		t.Fatalf("RunCycle errors: %v", errs)
	}
	if n != 0 || len(invoices.invoices) != 0 {
		t.Fatalf("invoices: n=%d stored=%d, want none (nothing to bill)", n, len(invoices.invoices))
	}
	if !subs.cycleUpdated["sub_1"] {
		t.Error("the period did not advance")
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if _, ok := rows[0].entry.Metadata["invoice_id"]; ok {
		t.Errorf("invoice_id set on a close that built no invoice: %v", rows[0].entry.Metadata["invoice_id"])
	}
}

// The cancel's final invoice follows the same rule, on both pricing paths.
func TestBillFinalOnImmediateCancel_NetNegativeUsage(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "neg",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "si_1", PlanID: "pln_neg", Quantity: 1,
		}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_neg": {ID: "pln_neg", Name: "Neg", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
				MeterIDs: []string{"mtr_tokens", "mtr_seats"}},
		},
		meters: map[string]domain.Meter{
			"mtr_tokens": {ID: "mtr_tokens", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"},
			"mtr_seats":  {ID: "mtr_seats", Key: "seats", Name: "Seats", Unit: "seats", Aggregation: "sum", RatingRuleVersionID: "rrv_seat"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_in":   {ID: "rrv_in", RuleKey: "tok_in", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)},
			"rrv_out":  {ID: "rrv_out", RuleKey: "tok_out", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(2)},
			"rrv_seat": {ID: "rrv_seat", RuleKey: "seat", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(5)},
		},
		meterPricingRules: map[string][]domain.MeterPricingRule{
			"mtr_tokens": {
				{ID: "mpr_in", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_in", DimensionMatch: map[string]any{"type": "in"}},
				{ID: "mpr_out", MeterID: "mtr_tokens", RatingRuleVersionID: "rrv_out", DimensionMatch: map[string]any{"type": "out"}},
			},
		},
	}
	usage := &mockUsage{
		totals: map[string]int64{"mtr_tokens": 300, "mtr_seats": -7},
		ruleAggs: map[string][]domain.RuleAggregation{
			"mtr_tokens": {
				{RuleID: "mpr_in", RatingRuleVersionID: "rrv_in", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(400)},
				{RuleID: "mpr_out", RatingRuleVersionID: "rrv_out", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(-100)},
			},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v (usage below zero must not fail the cancel)", err)
	}
	if inv.ID == "" || inv.SubtotalCents != 400 {
		t.Fatalf("final invoice = %q, subtotal %d; want one invoice of 400¢ (input tokens only)", inv.ID, inv.SubtotalCents)
	}
	if len(invoices.lineItems) != 1 || invoices.lineItems[0].RatingRuleVersionID != "rrv_in" {
		t.Fatalf("lines = %+v, want one rrv_in line", invoices.lineItems)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	got := noteMeters(t, rows[0])
	if len(got) != 2 || got["mtr_tokens"] != "-100" || got["mtr_seats"] != "-7" {
		t.Errorf("meters = %v, want mtr_tokens: -100 and mtr_seats: -7", got)
	}
	if got := rows[0].entry.Metadata["billing_reason"]; got != string(domain.BillingReasonSubscriptionCancel) {
		t.Errorf("billing_reason = %v, want subscription_cancel", got)
	}
	if got := rows[0].entry.Metadata["invoice_id"]; got != inv.ID {
		t.Errorf("invoice_id = %v, want %s", got, inv.ID)
	}
}

// capTokensMeter makes mtr_tok a meter priced by two pricing rules at 1¢ a
// token, with buckets of +150 (rrv_in) and -50 (rrv_out). Its total nets the
// two to +100; the invoice bills the +150 bucket only.
func capTokensMeter(pricing *mockPricing, usage *mockUsage) {
	pricing.meters["mtr_tok"] = domain.Meter{ID: "mtr_tok", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"}
	pricing.rules["rrv_in"] = domain.RatingRuleVersion{ID: "rrv_in", RuleKey: "tok_in", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)}
	pricing.rules["rrv_out"] = domain.RatingRuleVersion{ID: "rrv_out", RuleKey: "tok_out", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)}
	pricing.meterPricingRules = map[string][]domain.MeterPricingRule{
		"mtr_tok": {
			{ID: "mpr_in", MeterID: "mtr_tok", RatingRuleVersionID: "rrv_in", DimensionMatch: map[string]any{"type": "in"}},
			{ID: "mpr_out", MeterID: "mtr_tok", RatingRuleVersionID: "rrv_out", DimensionMatch: map[string]any{"type": "out"}},
		},
	}
	usage.ruleAggs = map[string][]domain.RuleAggregation{
		"mtr_tok": {
			{RuleID: "mpr_in", RatingRuleVersionID: "rrv_in", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(150)},
			{RuleID: "mpr_out", RatingRuleVersionID: "rrv_out", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(-50)},
		},
	}
	usage.totals["mtr_tok"] = 100
}

// A block usage cap totals what the invoice bills. A meter priced by pricing
// rules bills its buckets, so its bucket below zero counts as 0 in the cap's
// total too. Taking the meter's total (+100) instead billed 120 tokens
// against a cap of 80 (scale 80/100), and all 150 against a cap of 120 (the
// total was under it, so the cap never applied).
func TestRunCycle_NetNegativeBucket_UsageCapCountsWhatBills(t *testing.T) {
	for _, capUnits := range []int64{80, 120} {
		t.Run(fmt.Sprintf("cap %d", capUnits), func(t *testing.T) {
			engine, subs, usage, pricing, invoices := setupEngine()
			aud := &fakeEngineAudit{}
			engine.SetAuditLogger(aud)
			sub := subs.subs["sub_1"]
			sub.UsageCapUnits = &capUnits
			sub.OverageAction = "block"
			subs.subs["sub_1"] = sub
			plan := pricing.plans["pln_1"]
			plan.MeterIDs = []string{"mtr_tok"}
			pricing.plans["pln_1"] = plan
			usage.totals = map[string]int64{}
			capTokensMeter(pricing, usage)

			if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
				t.Fatalf("RunCycle errors: %v", errs)
			}
			qty, amount := meterBilled(invoices.lineItems, "mtr_tok")
			if !qty.Round(6).Equal(decimal.NewFromInt(capUnits)) || amount != capUnits {
				t.Errorf("block cap %d: tokens billed %s for %d¢, want %d for %d¢", capUnits, qty, amount, capUnits, capUnits)
			}
			rows := netNegativeRows(t, aud)
			if len(rows) != 1 {
				t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
			}
			if got := noteMeters(t, rows[0]); got["mtr_tok"] != "-50" {
				t.Errorf("meters = %v, want mtr_tok: -50 (the cap does not scale usage below zero)", got)
			}
		})
	}
}

// The cancel's final invoice: the same cap total as the period close.
func TestBillFinalOnImmediateCancel_NetNegativeBucket_UsageCapCountsWhatBills(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	capUnits := int64(80)
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "neg",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_1", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
		UsageCapUnits:             &capUnits,
		OverageAction:             "block",
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "P", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_tok"}},
		},
		meters: map[string]domain.Meter{},
		rules:  map[string]domain.RatingRuleVersion{},
	}
	usage := &mockUsage{totals: map[string]int64{}}
	capTokensMeter(pricing, usage)
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.SubtotalCents != 80 {
		t.Errorf("subtotal = %d, want 80 (80 tokens at the block cap)", inv.SubtotalCents)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_tok"] != "-50" {
		t.Errorf("meters = %v, want mtr_tok: -50 (the cap does not scale usage below zero)", got)
	}
}

// Cycle close, cap re-derivation in buildLineItems: a pricing-rule meter is
// priced from raw buckets x the re-derived capScale, so the re-derivation
// must count a meter below zero as 0 too.
func TestRunCycle_NetNegativeMeter_CapRederivationCountsItAsZero(t *testing.T) {
	engine, subs, usage, pricing, invoices := setupEngine()
	sub := subs.subs["sub_1"]
	capUnits := int64(80)
	sub.UsageCapUnits = &capUnits
	sub.OverageAction = "block"
	subs.subs["sub_1"] = sub
	plan := pricing.plans["pln_1"]
	plan.MeterIDs = []string{"mtr_api", "mtr_tok"}
	pricing.plans["pln_1"] = plan
	pricing.meters["mtr_tok"] = domain.Meter{ID: "mtr_tok", Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"}
	pricing.rules["rrv_in"] = domain.RatingRuleVersion{ID: "rrv_in", RuleKey: "tok_in", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)}
	pricing.meterPricingRules = map[string][]domain.MeterPricingRule{
		"mtr_tok": {{ID: "mpr_in", MeterID: "mtr_tok", RatingRuleVersionID: "rrv_in"}},
	}
	usage.ruleAggs = map[string][]domain.RuleAggregation{
		"mtr_tok": {{RuleID: "mpr_in", RatingRuleVersionID: "rrv_in", AggregationMode: domain.AggSum, Quantity: decimal.NewFromInt(150)}},
	}
	delete(usage.totals, "mtr_storage")
	usage.totals["mtr_tok"] = 150
	usage.totals["mtr_api"] = -50

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("RunCycle errors: %v", errs)
	}
	if billed, _ := meterBilled(invoices.lineItems, "mtr_tok"); !billed.Round(6).Equal(decimal.NewFromInt(80)) {
		t.Errorf("block cap 80: tokens billed %s, want 80", billed)
	}
}

// Cancel final invoice: its cap total counts a meter below zero as 0, and the
// row reports that meter's own quantity, not a cap-scaled share of it.
func TestBillFinalOnImmediateCancel_NetNegativeMeter_UsageCapCountsItAsZero(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	capUnits := int64(80)
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "neg",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_1", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
		UsageCapUnits:             &capUnits,
		OverageAction:             "block",
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "P", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_api", "mtr_storage"}},
		},
		meters: map[string]domain.Meter{
			"mtr_api":     {ID: "mtr_api", Key: "api_calls", Name: "API", Unit: "calls", Aggregation: "sum", RatingRuleVersionID: "rrv_api"},
			"mtr_storage": {ID: "mtr_storage", Key: "storage", Name: "Storage", Unit: "GB", Aggregation: "sum", RatingRuleVersionID: "rrv_storage"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api":     {ID: "rrv_api", RuleKey: "api", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)},
			"rrv_storage": {ID: "rrv_storage", RuleKey: "storage", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(100)},
		},
	}
	usage := &mockUsage{totals: map[string]int64{"mtr_storage": 150, "mtr_api": -50}}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)

	inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.SubtotalCents != 8000 {
		t.Errorf("subtotal = %d, want 8000 (80 GB at the block cap)", inv.SubtotalCents)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-50" {
		t.Errorf("meters = %v, want mtr_api: -50 (the cap does not scale usage below zero)", got)
	}
}

// After a spend-threshold invoice (reset_billing_cycle=false) the close
// prices usage from that invoice's end, so the -40 is the net of
// [Apr 10, May 1), not of the whole period. The row names that window for
// each meter: it is the range of events that netted below zero.
func TestCycleClose_AfterThresholdFire_NetNegativeRowNamesItsWindow(t *testing.T) {
	_, usage, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000, ResetBillingCycle: false})
	seedThresholdInvoice(invoices, domain.InvoiceFinalized)
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	fireEnd := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	usage.perInterval[mockIntervalKey("mtr_api", fireEnd, periodEnd)] = -40

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("RunCycle errors: %v", errs)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-40" {
		t.Errorf("meters = %v, want mtr_api: -40", got)
	}
	wantNoteWindow(t, rows[0], "mtr_api", fireEnd, periodEnd)
}

// The same window under a block cap: a sub-period quantity is scaled by the
// cap only when it bills, so the row keeps the -40.
func TestCycleClose_AfterThresholdFire_NetNegativeUnderCap_RowKeepsItsQuantity(t *testing.T) {
	subs, usage, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000, ResetBillingCycle: false})
	seedThresholdInvoice(invoices, domain.InvoiceFinalized)
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	sub := subs.subs["sub_1"]
	capUnits := int64(500) // the period's 1000 calls scale by 0.5
	sub.UsageCapUnits = &capUnits
	sub.OverageAction = "block"
	subs.subs["sub_1"] = sub
	fireEnd := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	usage.perInterval[mockIntervalKey("mtr_api", fireEnd, periodEnd)] = -40

	if _, errs := engine.RunCycle(context.Background(), 50, nil); len(errs) > 0 {
		t.Fatalf("RunCycle errors: %v", errs)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-40" {
		t.Errorf("meters = %v, want mtr_api: -40 (the cap does not scale usage below zero)", got)
	}
}

// The cancel after a threshold invoice prices [fire end, cancel). The row
// names that window and keeps the quantity under a block cap.
func TestBillFinalOnImmediateCancel_AfterThresholdFire_NetNegativeRow(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	fireEnd := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	capUnits := int64(80)
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "neg",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_1", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
		UsageCapUnits:             &capUnits,
		OverageAction:             "block",
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "P", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_api", "mtr_storage"}},
		},
		meters: map[string]domain.Meter{
			"mtr_api":     {ID: "mtr_api", Key: "api_calls", Name: "API", Unit: "calls", Aggregation: "sum", RatingRuleVersionID: "rrv_api"},
			"mtr_storage": {ID: "mtr_storage", Key: "storage", Name: "Storage", Unit: "GB", Aggregation: "sum", RatingRuleVersionID: "rrv_storage"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api":     {ID: "rrv_api", RuleKey: "api", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(1)},
			"rrv_storage": {ID: "rrv_storage", RuleKey: "storage", Version: 1, Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(100)},
		},
	}
	usage := &mockUsage{
		// [Mar 1, Mar 15): 150 GB, so the cap of 80 scales by 80/150.
		totals: map[string]int64{"mtr_storage": 150},
		perInterval: map[string]int64{
			mockIntervalKey("mtr_storage", fireEnd, cancelAt): 30,
			mockIntervalKey("mtr_api", fireEnd, cancelAt):     -40,
		},
	}
	invoices := &mockInvoices{invoices: []domain.Invoice{{
		ID: "inv_thr", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1",
		InvoiceNumber: "VLX-THR-1", BillingReason: domain.BillingReasonThreshold,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
		BillingPeriodStart: periodStart, BillingPeriodEnd: fireEnd,
	}}}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)

	if _, err := engine.BillFinalOnImmediateCancel(context.Background(), sub); err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-40" {
		t.Errorf("meters = %v, want mtr_api: -40 (the cap does not scale usage below zero)", got)
	}
	wantNoteWindow(t, rows[0], "mtr_api", fireEnd, cancelAt)
}

// A meter below zero in two windows (a mid-period plan change) is one entry:
// the sum of its quantities, and the window that spans them.
func TestNetNegativeUsage_AddSpansWindows(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 3, day, 0, 0, 0, 0, time.UTC) }
	n := netNegativeUsage{}
	n.add("mtr_api", "api_calls", decimal.NewFromInt(-20), d(15), d(31))
	n.add("mtr_api", "api_calls", decimal.NewFromInt(-30), d(1), d(10))
	m := n["mtr_api"]
	if !m.qty.Equal(decimal.NewFromInt(-50)) || !m.from.Equal(d(1)) || !m.to.Equal(d(31)) {
		t.Errorf("entry = %s over [%s, %s), want -50 over [%s, %s)", m.qty, m.from, m.to, d(1), d(31))
	}
}
