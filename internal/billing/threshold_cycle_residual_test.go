package billing

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/shopspring/decimal"
)

// These tests lock the C2 fix from the periodic-jobs audit: when a
// threshold invoice fired mid-cycle, the cycle close bills only the residual
// usage — from the threshold invoice's billing_period_end. Pre-fix the cycle
// close re-billed the whole period on top of the threshold invoice.
//
// A fire bills usage only, so the close bills the period's in_arrears base
// in full. A fire made before that change carried the full base; the close
// then skips it (seedLegacyThresholdInvoice).

func residualHarness(thresholds *domain.BillingThresholds) (*mockSubs, *mockUsage, *mockPricing, *mockInvoices, *Engine) {
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	nextBilling := periodEnd

	subs := &mockSubs{
		subs: map[string]domain.Subscription{
			"sub_1": {
				ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
				Items:                     []domain.SubscriptionItem{{ID: "subitem_1", PlanID: "pln_1", Quantity: 1}},
				Status:                    domain.SubscriptionActive,
				BillingTime:               domain.BillingTimeCalendar,
				CurrentBillingPeriodStart: &periodStart,
				CurrentBillingPeriodEnd:   &periodEnd,
				NextBillingAt:             &nextBilling,
				BillingThresholds:         thresholds,
			},
		},
		cycleUpdated: make(map[string]bool),
	}
	usage := &mockUsage{
		totals: map[string]int64{"mtr_api": 1000}, // full period [Apr 1, May 1)
		perInterval: map[string]int64{
			// residual window [Apr 10, May 1) — what the clamped cycle
			// close must aggregate instead of the full period
			mockIntervalKey("mtr_api", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC), periodEnd): 300,
		},
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{"pln_1": {
			ID: "pln_1", Name: "Pro Plan", Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseAmountCents: 4900,
		}},
		meters: map[string]domain.Meter{"mtr_api": {ID: "mtr_api", Name: "API Calls", Unit: "calls"}},
		prices: map[string][]domain.PlanPrice{
			"pln_1": {priceOf("prc_api", "mtr_api", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(100)})},
		},
	}
	invoices := &mockInvoices{}
	clk := clock.NewFake(time.Date(2026, 5, 1, 0, 0, 1, 0, time.UTC))
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, clk))
	return subs, usage, pricing, invoices, engine
}

// seedThresholdInvoice seeds a fire over [Apr 1, Apr 10): 700 calls, usage
// only, as fires bill now.
func seedThresholdInvoice(inv *mockInvoices, status domain.InvoiceStatus) {
	seedThresholdInvoiceLines(inv, status, false)
}

// seedLegacyThresholdInvoice seeds the same fire as made before fires
// stopped billing the base: it also carries the full 4900c in_arrears base.
func seedLegacyThresholdInvoice(inv *mockInvoices, status domain.InvoiceStatus) {
	seedThresholdInvoiceLines(inv, status, true)
}

func seedThresholdInvoiceLines(inv *mockInvoices, status domain.InvoiceStatus, withBase bool) {
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	subtotal := int64(70000)
	inv.lineItems = append(inv.lineItems, domain.InvoiceLineItem{
		ID: "li_thr_usage", InvoiceID: "inv_thr", TenantID: "t1", LineType: domain.LineTypeUsage,
		MeterID: "mtr_api", PriceID: "prc_api", Description: "API Calls",
		Quantity: 700, QuantityDecimal: decimal.NewFromInt(700), UnitAmountCents: 100,
		AmountCents: 70000, TotalAmountCents: 70000, Currency: "USD",
		BillingPeriodStart: &start, BillingPeriodEnd: &end,
	})
	if withBase {
		subtotal += 4900
		inv.lineItems = append(inv.lineItems, domain.InvoiceLineItem{
			ID: "li_thr_base", InvoiceID: "inv_thr", TenantID: "t1", LineType: domain.LineTypeBaseFee,
			Description: "Pro Plan - base fee (qty 1)", Quantity: 1, UnitAmountCents: 4900,
			AmountCents: 4900, TotalAmountCents: 4900, Currency: "USD",
		})
	}
	inv.invoices = append(inv.invoices, domain.Invoice{
		ID: "inv_thr", TenantID: "t1", CustomerID: "cus_1", SubscriptionID: "sub_1",
		InvoiceNumber: "HC-THR-1", BillingReason: domain.BillingReasonThreshold,
		Status: status, PaymentStatus: domain.PaymentSucceeded,
		BillingPeriodStart: start,
		BillingPeriodEnd:   end,
		SubtotalCents:      subtotal, TotalAmountCents: subtotal,
	})
}

func cycleInvoiceLines(t *testing.T, inv *mockInvoices, invoiceID string) (base, usage []domain.InvoiceLineItem) {
	t.Helper()
	for _, li := range inv.lineItems {
		if li.InvoiceID != invoiceID {
			continue
		}
		switch li.LineType {
		case domain.LineTypeBaseFee:
			base = append(base, li)
		case domain.LineTypeUsage:
			usage = append(usage, li)
		}
	}
	return base, usage
}

func TestCycleClose_AfterThresholdFire_BillsOnlyResidual(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000})
	seedThresholdInvoice(invoices, domain.InvoiceFinalized)

	if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	if len(invoices.invoices) != 2 {
		t.Fatalf("invoices: got %d, want 2 (seeded threshold + cycle close)", len(invoices.invoices))
	}
	cycle := invoices.invoices[1]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)

	// The fire billed no base, so the close bills April's in full, once.
	if len(base) != 1 || base[0].AmountCents != 4900 {
		t.Errorf("cycle close must bill the full in_arrears base (4900c) after a usage-only fire; got %+v", base)
	}
	// Usage clamps to the residual window: 300 units @ 100 = 30000, NOT
	// the full-period 1000 units (the pre-fire 700 were on the threshold
	// invoice).
	if len(usage) != 1 {
		t.Fatalf("usage lines: got %d, want 1", len(usage))
	}
	if !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(300)) {
		t.Errorf("usage quantity: got %s, want 300 (residual window only)", usage[0].QuantityDecimal)
	}
	if usage[0].AmountCents != 30000 {
		t.Errorf("usage amount: got %d, want 30000", usage[0].AmountCents)
	}
	wantStart := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	if usage[0].BillingPeriodStart == nil || !usage[0].BillingPeriodStart.Equal(wantStart) {
		t.Errorf("usage line period start: got %v, want %v (threshold watermark)", usage[0].BillingPeriodStart, wantStart)
	}
	if cycle.SubtotalCents != 34900 {
		t.Errorf("cycle subtotal: got %d, want 34900 (base + residual usage)", cycle.SubtotalCents)
	}
}

// A fire made before fires stopped billing the base carried the full
// in_arrears base. The close of a period that crosses the upgrade must not
// bill it again: it skips the base because that invoice has a base line.
func TestCycleClose_AfterLegacyThresholdFireWithBase_SkipsBase(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000})
	seedLegacyThresholdInvoice(invoices, domain.InvoiceFinalized)

	if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	cycle := invoices.invoices[1]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)
	if len(base) != 0 {
		t.Errorf("the legacy fire billed the base; the close re-billed it on %d lines", len(base))
	}
	if len(usage) != 1 || usage[0].AmountCents != 30000 || cycle.SubtotalCents != 30000 {
		t.Errorf("close: usage %+v subtotal %d, want 30000c residual usage only", usage, cycle.SubtotalCents)
	}
}

func TestCycleClose_NoThresholdInvoice_FullPeriodUnchanged(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000})

	if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	if len(invoices.invoices) != 1 {
		t.Fatalf("invoices: got %d, want 1", len(invoices.invoices))
	}
	cycle := invoices.invoices[0]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)
	if len(base) != 1 || base[0].AmountCents != 4900 {
		t.Fatalf("expected the full in_arrears base line (4900); got %+v", base)
	}
	if len(usage) != 1 || !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("expected full-period usage of 1000 units; got %+v", usage)
	}
	if cycle.SubtotalCents != 104900 {
		t.Errorf("cycle subtotal: got %d, want 104900", cycle.SubtotalCents)
	}
}

// The watermark keys on the INVOICE (ground truth), not the mutable
// BillingThresholds config — an operator removing thresholds after a
// mid-cycle fire must not resurrect the double-bill at period end. The
// legacy fire is used so the base skip is covered too.
func TestCycleClose_ThresholdsRemovedAfterFire_StillBillsResidual(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(nil)
	seedLegacyThresholdInvoice(invoices, domain.InvoiceFinalized)

	if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	cycle := invoices.invoices[1]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)
	if len(base) != 0 {
		t.Errorf("base must stay skipped even after thresholds were removed; got %d base lines", len(base))
	}
	if len(usage) != 1 || !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(300)) {
		t.Fatalf("expected residual usage of 300 units; got %+v", usage)
	}
}

// A VOIDED threshold invoice returned the money — its window is billable
// again, so the cycle close must fall back to full-period billing.
func TestCycleClose_VoidedThresholdInvoice_BillsFullPeriod(t *testing.T) {
	_, _, _, invoices, engine := residualHarness(&domain.BillingThresholds{AmountGTE: 100000})
	seedThresholdInvoice(invoices, domain.InvoiceVoided)

	if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
		t.Fatalf("cycle errors: %v", errs)
	}
	cycle := invoices.invoices[1]
	base, usage := cycleInvoiceLines(t, invoices, cycle.ID)
	if len(base) != 1 {
		t.Errorf("voided threshold invoice must not suppress the base line; got %d base lines", len(base))
	}
	if len(usage) != 1 || !usage[0].QuantityDecimal.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("expected full-period usage of 1000 units; got %+v", usage)
	}
}
