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
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
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

// A single-rule meter whose period total is -50 bills nothing; the base fee
// and the other meter bill as usual, and the close is recorded.
func TestRunCycle_NetNegativeMeter_BillsZeroAndRecordsIt(t *testing.T) {
	engine, _, usage, pricing, invoices := setupEngine()
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	api := pricing.meters["mtr_api"]
	api.EventName = "api_calls"
	pricing.meters["mtr_api"] = api
	usage.totals["mtr_api"] = -50

	n, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil)
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
	pricing.meters["mtr_api"] = domain.Meter{ID: "mtr_api", EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"}
	pricing.prices["pln_1"] = []domain.PlanPrice{
		priceWith("prc_in", "mtr_api", map[string]string{"type": "in"}, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}),
		priceWith("prc_out", "mtr_api", map[string]string{"type": "out"}, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(2)}),
		pricing.prices["pln_1"][1], // storage
	}
	usage.priceAggs = map[string][]domain.ClaimedUsage{
		"mtr_api": {
			{PriceID: "prc_in", Quantity: decimal.NewFromInt(1000)},
			{PriceID: "prc_out", Quantity: decimal.NewFromInt(-300)},
		},
	}
	usage.totals["mtr_api"] = 700

	if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
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
	if len(tokenLines) != 1 || tokenLines[0].PriceID != "prc_in" || tokenLines[0].AmountCents != 1000 {
		t.Fatalf("token lines = %+v, want one prc_in line of 1000¢", tokenLines)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-300" {
		t.Errorf("meters = %v, want mtr_api: -300", got)
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

	n, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil)
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
				BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
		},
		meters: map[string]domain.Meter{
			"mtr_tokens": {ID: "mtr_tokens", EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum"},
			"mtr_seats":  {ID: "mtr_seats", EventName: "seats", Name: "Seats", Unit: "seats", Aggregation: "sum"},
		},
		prices: map[string][]domain.PlanPrice{
			"pln_neg": {priceWith("prc_in", "mtr_tokens", map[string]string{"type": "in"}, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}),
				priceWith("prc_out", "mtr_tokens", map[string]string{"type": "out"}, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(2)}),
				priceOf("prc_seat", "mtr_seats", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(5)})},
		},
	}
	usage := &mockUsage{
		totals: map[string]int64{"mtr_tokens": 300, "mtr_seats": -7},
		priceAggs: map[string][]domain.ClaimedUsage{
			"mtr_tokens": {
				{PriceID: "prc_in", Quantity: decimal.NewFromInt(400)},
				{PriceID: "prc_out", Quantity: decimal.NewFromInt(-100)},
			},
		},
	}
	invoices := &mockInvoices{}
	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)

	inv, err := engine.BillFinalOnImmediateCancel(postgres.WithLivemode(context.Background(), true), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v (usage below zero must not fail the cancel)", err)
	}
	if inv.ID == "" || inv.SubtotalCents != 400 {
		t.Fatalf("final invoice = %q, subtotal %d; want one invoice of 400¢ (input tokens only)", inv.ID, inv.SubtotalCents)
	}
	if len(invoices.lineItems) != 1 || invoices.lineItems[0].PriceID != "prc_in" {
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

// After a spend-threshold invoice (reset_billing_cycle=false) the close
// prices usage from that invoice's end, so the -40 is the net of
// [Apr 10, May 1), not of the whole period. The row names that window for
// each meter: it is the range of events that netted below zero.
func TestCycleClose_AfterThresholdFire_NetNegativeRowNamesItsWindow(t *testing.T) {
	_, usage, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000})
	seedThresholdInvoice(invoices, domain.InvoiceFinalized)
	aud := &fakeEngineAudit{}
	engine.SetAuditLogger(aud)
	fireEnd := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	usage.perInterval[mockIntervalKey("mtr_api", fireEnd, periodEnd)] = -40

	if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
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

// The cancel after a threshold invoice prices [fire end, cancel). The row
// names that window.
func TestBillFinalOnImmediateCancel_AfterThresholdFire_NetNegativeRow(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	fireEnd := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "neg",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_1", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_1": {ID: "pln_1", Name: "P", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseBillTiming: domain.BillInArrears},
		},
		meters: map[string]domain.Meter{
			"mtr_api":     {ID: "mtr_api", EventName: "api_calls", Name: "API", Unit: "calls", Aggregation: "sum"},
			"mtr_storage": {ID: "mtr_storage", EventName: "storage", Name: "Storage", Unit: "GB", Aggregation: "sum"},
		},
		prices: map[string][]domain.PlanPrice{
			"pln_1": {priceOf("prc_api", "mtr_api", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}),
				priceOf("prc_storage", "mtr_storage", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(100)})},
		},
	}
	usage := &mockUsage{
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

	if _, err := engine.BillFinalOnImmediateCancel(postgres.WithLivemode(context.Background(), true), sub); err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	rows := netNegativeRows(t, aud)
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1", netNegativeAction, len(rows))
	}
	if got := noteMeters(t, rows[0]); got["mtr_api"] != "-40" {
		t.Errorf("meters = %v, want mtr_api: -40", got)
	}
	wantNoteWindow(t, rows[0], "mtr_api", fireEnd, cancelAt)
}

// A meter below zero in two windows (a mid-period plan change) is one entry:
// the sum of its quantities, and the window that spans them.
func TestNetNegativeUsage_AddSpansWindows(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 3, day, 0, 0, 0, 0, time.UTC) }
	n := netNegativeUsage{}
	n.add("mtr_api", "api_calls", "API calls", decimal.NewFromInt(-20), d(15), d(31))
	n.add("mtr_api", "api_calls", "API calls", decimal.NewFromInt(-30), d(1), d(10))
	m := n["mtr_api"]
	if !m.qty.Equal(decimal.NewFromInt(-50)) || !m.from.Equal(d(1)) || !m.to.Equal(d(31)) {
		t.Errorf("entry = %s over [%s, %s), want -50 over [%s, %s)", m.qty, m.from, m.to, d(1), d(31))
	}
}
