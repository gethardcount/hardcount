package billing

// #1193: ingest accepts negative quantities as usage corrections, so a usage
// bucket's total over a billing window can be below zero. These tests pin the
// rule every invoice decision follows: that bucket bills 0, the rest of the
// invoice is built, and one subscription.net_negative_usage audit row,
// written on the decision's own transaction, names the meter and the amount.

import (
	"context"
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
	ms, ok := r.entry.Metadata["meters"].([]map[string]any)
	if !ok {
		t.Fatalf("meters metadata: got %T, want []map[string]any", r.entry.Metadata["meters"])
	}
	out := map[string]string{}
	for _, m := range ms {
		id, _ := m["meter_id"].(string)
		qty, _ := m["quantity"].(string)
		out[id] = qty
	}
	return out
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
