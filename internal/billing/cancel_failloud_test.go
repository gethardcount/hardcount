package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// cancelFailLoudFixture builds the minimal canceled sub used by both
// fail-loud tests: mid-period immediate cancel of an in_arrears plan.
func cancelFailLoudFixture() (domain.Subscription, *mockPricing, *mockUsage, *mockInvoices) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "api",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "si_1", PlanID: "pln_api", Quantity: 1,
		}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}
	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			"pln_api": {ID: "pln_api", Name: "API", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 3000,
				BaseBillTiming: domain.BillInArrears, MeterIDs: []string{"mtr_api"}},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", Key: "api", Name: "API Calls",
				Unit: "calls", Aggregation: "sum", RatingRuleVersionID: "rrv_api"},
		},
		rules: map[string]domain.RatingRuleVersion{
			"rrv_api": {ID: "rrv_api", RuleKey: "api_calls", Version: 1,
				Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(2)},
		},
	}
	return sub, pricing, &mockUsage{totals: map[string]int64{"mtr_api": 500}}, &mockInvoices{}
}

// TestBillFinalOnImmediateCancel_IntervalReadFailsLoud pins fail-loud parity
// with the cycle builder: a transient failure reading billing_intervals must
// fail the cancel bill (the operator's cancel retries) — never silently
// single-line the final invoice at the current plan's rate.
func TestBillFinalOnImmediateCancel_IntervalReadFailsLoud(t *testing.T) {
	sub, pricing, usage, invoices := cancelFailLoudFixture()
	subs := &mockSubs{intervalsErr: errors.New("db blip")}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	engine.SetIntervalReader(subs)

	_, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err == nil {
		t.Fatal("interval read failure must fail the cancel bill, not silently single-line it")
	}
	if !strings.Contains(err.Error(), "billing-intervals read") {
		t.Errorf("error should name the failed read, got %v", err)
	}
	if len(invoices.invoices) != 0 {
		t.Errorf("no invoice may be created on a failed interval read, got %d", len(invoices.invoices))
	}
}

// TestBillFinalOnImmediateCancel_SegmentPlanLookupFailsLoud pins the second
// half: a change-log segment whose plan can't be hydrated was silently
// DROPPED (GetPlan err -> continue; the segment loop then skips unknown
// plans) — underbilling the final invoice with no signal. It must error.
func TestBillFinalOnImmediateCancel_SegmentPlanLookupFailsLoud(t *testing.T) {
	sub, pricing, usage, invoices := cancelFailLoudFixture()
	// A pre-swap interval whose plan is NOT in the pricing mock:
	// hydration hits mockPricing.GetPlan's "plan not found".
	swapAt := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	subs := &mockSubs{intervals: []domain.ItemInterval{
		{SubscriptionItemID: "si_1", PlanID: "pln_gone", Quantity: 1,
			StartsAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), EndsAt: &swapAt},
		{SubscriptionItemID: "si_1", PlanID: "pln_api", Quantity: 1, StartsAt: swapAt},
	}}
	engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
	engine.SetIntervalReader(subs)

	_, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
	if err == nil {
		t.Fatal("segment-plan lookup failure must fail the cancel bill, not silently drop the segment")
	}
	if !strings.Contains(err.Error(), "get segment plan pln_gone on cancel") {
		t.Errorf("error should name the missing segment plan, got %v", err)
	}
	if len(invoices.invoices) != 0 {
		t.Errorf("no invoice may be created on a failed segment hydration, got %d", len(invoices.invoices))
	}
}

// TestBillFinalOnImmediateCancel_EndedPeriodIsRefused pins the window check's
// four outcomes. A cancel at or after the period end is refused with
// unbilled_period (the period is ended and unbilled, and nothing bills a
// canceled subscription), with a next step that fits the subscription's
// clock. A cancel at the period start, and a never-activated trial however
// late, write nothing and are not errors.
func TestBillFinalOnImmediateCancel_EndedPeriodIsRefused(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name       string
		canceledAt time.Time
		clockID    string
		trial      bool
		wantMsg    string // empty: no invoice and no error
	}{
		{name: "exactly at period end", canceledAt: periodEnd, wantMsg: "retry billing for this subscription, then cancel again"},
		{name: "after period end", canceledAt: periodEnd.Add(time.Hour), wantMsg: "retry billing for this subscription, then cancel again"},
		{name: "after period end on a test clock", canceledAt: periodEnd.Add(time.Hour), clockID: "vlx_tclk_1", wantMsg: "finish or retry the clock advance, then cancel again"},
		{name: "exactly at period start", canceledAt: periodStart},
		{name: "never-activated trial after period end", canceledAt: periodEnd.Add(time.Hour), trial: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			sub, pricing, usage, invoices := cancelFailLoudFixture()
			sub.CanceledAt, sub.TestClockID = &c.canceledAt, c.clockID
			if c.trial {
				sub.TrialEndAt = &periodStart
			} else {
				sub.ActivatedAt = &periodStart
			}
			subs := &mockSubs{}
			engine := wireBaseTax(NewEngine(subs, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))
			engine.SetIntervalReader(subs)

			inv, err := engine.BillFinalOnImmediateCancel(context.Background(), sub)
			if len(invoices.invoices) != 0 || inv.ID != "" {
				t.Fatalf("an invoice was written (%d stored, returned %q), want none", len(invoices.invoices), inv.ID)
			}
			if c.wantMsg == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "unbilled_period" || !strings.Contains(err.Error(), c.wantMsg) {
				t.Fatalf("err = %v (code %q), want invalid state, code unbilled_period, saying %q", err, errs.Code(err), c.wantMsg)
			}
		})
	}
}
