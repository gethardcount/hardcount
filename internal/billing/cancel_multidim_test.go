package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// TestBillFinalOnImmediateCancel_MultiDimMeterBilled locks the ADR-044 fix for
// the cancel-path revenue leak: a meter priced via dimension-match pricing
// rules (the AI `tokens` shape — one rule per {model, token_type}, meter
// RatingRuleVersionID empty) was silently skipped by BillFinalOnImmediateCancel's
// single-rule-only usage loop, so a mid-period immediate cancel emitted NO
// invoice for all token usage. The cycle path, preview, and the threshold
// scan all carry the AggregateClaims fork; this asserts the cancel
// path now does too.
//
// Numbers: 1,000,000 input tokens @ $3.00/M + 500,000 output tokens @
// $15.00/M = $3.00 + $7.50 = $10.50 the customer would otherwise consume
// for free by canceling mid-period.
func TestBillFinalOnImmediateCancel_MultiDimMeterBilled(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC) // mid-period

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "tokens",
		Status: domain.SubscriptionCanceled,
		Items: []domain.SubscriptionItem{{
			ID: "si_1", PlanID: "pln_tok", Quantity: 1,
		}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}

	pricing := &mockPricing{
		plans: map[string]domain.Plan{
			// Pure-usage plan: no base fee, one multi-dim tokens meter.
			"pln_tok": {ID: "pln_tok", Name: "Tokens", Currency: "USD",
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 0,
				BaseBillTiming: domain.BillInArrears},
		},
		meters: map[string]domain.Meter{
			// Multi-dim shape: NO meter-linked default rule. Pre-fix the
			// cancel loop's `RatingRuleVersionID == "" → continue` guard
			// skipped this meter entirely.
			"mtr_tokens": {ID: "mtr_tokens", EventName: "tokens", Name: "Tokens",
				Unit: "tokens", Aggregation: "sum"},
		},
		prices: map[string][]domain.PlanPrice{
			"pln_tok": {priceWith("prc_in", "mtr_tokens", map[string]string{"token_type": "input"}, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0003")}),
				priceWith("prc_out", "mtr_tokens", map[string]string{"token_type": "output"}, domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0015")})},
		},
	}
	usage := &mockUsage{
		priceAggs: map[string][]domain.ClaimedUsage{
			"mtr_tokens": {
				{PriceID: "prc_in", Quantity: decimal.NewFromInt(1_000_000)},
				{PriceID: "prc_out", Quantity: decimal.NewFromInt(500_000)},
			},
		},
	}
	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	inv, err := engine.BillFinalOnImmediateCancel(postgres.WithLivemode(context.Background(), true), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}

	// Pre-fix: subtotal stayed 0 → the zero-subtotal guard returned an empty
	// invoice and the token usage was never billed.
	if inv.ID == "" {
		t.Fatal("REVENUE LEAK: no final invoice emitted — multi-dim token usage went unbilled on immediate cancel")
	}
	if inv.SubtotalCents != 1050 {
		t.Errorf("subtotal: got %d, want 1050 ($3.00 input + $7.50 output)", inv.SubtotalCents)
	}
	if inv.BillingReason != domain.BillingReasonSubscriptionCancel {
		t.Errorf("billing reason: got %q, want subscription_cancel", inv.BillingReason)
	}

	if len(invoices.lineItems) != 2 {
		t.Fatalf("line items: got %d, want 2 (one per claimed pricing rule)", len(invoices.lineItems))
	}
	byRule := map[string]domain.InvoiceLineItem{}
	for _, li := range invoices.lineItems {
		byRule[li.PriceID] = li
	}
	in, out := byRule["prc_in"], byRule["prc_out"]
	if in.AmountCents != 300 {
		t.Errorf("input line amount: got %d, want 300", in.AmountCents)
	}
	if out.AmountCents != 750 {
		t.Errorf("output line amount: got %d, want 750", out.AmountCents)
	}
	if !in.QuantityDecimal.Equal(decimal.NewFromInt(1_000_000)) {
		t.Errorf("input line QuantityDecimal: got %s, want 1000000", in.QuantityDecimal)
	}
	for _, li := range invoices.lineItems {
		if !strings.Contains(li.Description, "canceled mid-period") {
			t.Errorf("line description %q: want the '- canceled mid-period' suffix (cancel-path parity)", li.Description)
		}
		if li.LineType != domain.LineTypeUsage {
			t.Errorf("line type: got %q, want usage", li.LineType)
		}
	}
}

// TestBillFinalOnImmediateCancel_SingleRuleMeterStillBilled proves the legacy
// single-rule path is untouched by the multi-dim fork: a meter with a direct
// RatingRuleVersionID binding (and no pricing rules) bills exactly as before.
func TestBillFinalOnImmediateCancel_SingleRuleMeterStillBilled(t *testing.T) {
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
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 0,
				BaseBillTiming: domain.BillInArrears},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", EventName: "api", Name: "API Calls",
				Unit: "calls", Aggregation: "sum"},
		},
		prices: map[string][]domain.PlanPrice{
			"pln_api": {priceOf("prc_api", "mtr_api", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(2)})},
		},
		// No meterPricingRules → single-rule path.
	}
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 500}}
	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &mockSettings{}, nil, nil, billingTestClock()))

	inv, err := engine.BillFinalOnImmediateCancel(postgres.WithLivemode(context.Background(), true), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.ID == "" {
		t.Fatal("expected a final invoice for single-rule meter usage")
	}
	if inv.SubtotalCents != 1000 {
		t.Errorf("subtotal: got %d, want 1000 (500 calls × 2¢)", inv.SubtotalCents)
	}
}

// zeroTermsSettings returns a settings row on Net 0 ("Due on receipt").
type zeroTermsSettings struct{ mockSettings }

func (z *zeroTermsSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{NetPaymentTerms: 0, TaxProvider: "none"}, nil
}

// TestBillFinalOnImmediateCancel_HonorsDueOnReceipt: Net 0 is the "Due on
// receipt" setting (dashboard preset, validated as "0 = due immediately"), so
// the final cancel invoice is due the moment it is issued. Pre-P24 this test
// asserted the opposite — that 0 was a legacy row to coerce to Net 30 —
// which made "Due on receipt" silently mean Net 30 on every engine invoice.
func TestBillFinalOnImmediateCancel_HonorsDueOnReceipt(t *testing.T) {
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
				BillingInterval: domain.BillingMonthly, BaseAmountCents: 0,
				BaseBillTiming: domain.BillInArrears},
		},
		meters: map[string]domain.Meter{
			"mtr_api": {ID: "mtr_api", EventName: "api", Name: "API Calls",
				Unit: "calls", Aggregation: "sum"},
		},
		prices: map[string][]domain.PlanPrice{
			"pln_api": {priceOf("prc_api", "mtr_api", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(2)})},
		},
	}
	usage := &mockUsage{totals: map[string]int64{"mtr_api": 500}}
	invoices := &mockInvoices{}

	engine := wireBaseTax(NewEngine(&mockSubs{subs: map[string]domain.Subscription{sub.ID: sub}}, usage, pricing, invoices, nil, &zeroTermsSettings{}, nil, nil, billingTestClock()))

	inv, err := engine.BillFinalOnImmediateCancel(postgres.WithLivemode(context.Background(), true), sub)
	if err != nil {
		t.Fatalf("BillFinalOnImmediateCancel: %v", err)
	}
	if inv.NetPaymentTermDays != 0 {
		t.Errorf("NetPaymentTermDays: got %d, want 0 (Due on receipt)", inv.NetPaymentTermDays)
	}
	if inv.DueAt == nil || inv.IssuedAt == nil || !inv.DueAt.Equal(*inv.IssuedAt) {
		t.Errorf("DueAt = %v, want IssuedAt (%v)", inv.DueAt, inv.IssuedAt)
	}
}
