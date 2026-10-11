package billing

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

type fakeCreditHeadroom struct {
	credited map[string]int64 // invoiceID -> already-credited cents (non-voided CN totals)
}

func (f *fakeCreditHeadroom) CreditedCents(_ context.Context, _, invoiceID string) (int64, error) {
	return f.credited[invoiceID], nil
}

// TestSettleUnusedAcrossFunding_HeadroomSpill is the headroom-aware regression:
// a prior credit note (e.g. an earlier downgrade clawback) shrank the upgrade
// invoice's remaining creditable, so a later cancel must cap that invoice's
// share at its remaining headroom and SPILL the overflow onto the base invoice —
// instead of overrunning the upgrade invoice's credit-note cap and loud-failing.
func TestSettleUnusedAcrossFunding_HeadroomSpill(t *testing.T) {
	periodStart := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	changeAt := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC) // 23 of 30 unused

	pricing := &mockPricing{plans: map[string]domain.Plan{
		"pln_150": {ID: "pln_150", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 15000, BaseBillTiming: domain.BillInAdvance},
	}}
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "mid",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{PlanID: "pln_150", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
		CanceledAt: &cancelAt,
	}
	baseInv := domain.Invoice{
		ID: "inv_base", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 10000, TotalAmountCents: 10000,
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd,
	}
	baseLine := domain.InvoiceLineItem{ID: "ili_base", InvoiceID: "inv_base", LineType: domain.LineTypeBaseFee, BillingPeriodStart: &periodStart}
	upInv := domain.Invoice{
		ID: "inv_up", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 8333, TotalAmountCents: 8333,
		BillingReason: domain.BillingReasonSubscriptionUpdate, SourcePlanChangedAt: &changeAt,
		BillingPeriodStart: changeAt, BillingPeriodEnd: periodEnd,
	}
	inv := &mockInvoices{invoices: []domain.Invoice{baseInv, upInv}, lineItems: []domain.InvoiceLineItem{baseLine}}

	adjuster := &fakeCreditNoteAdjuster{}
	// Prior downgrade CN already credited $60 of the $83.33 upgrade invoice →
	// only $23.33 (2333c) of headroom remains there.
	headroom := &fakeCreditHeadroom{credited: map[string]int64{"inv_up": 6000}}

	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, pricing, inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
	e.SetCreditGranter(&fakeCreditGranter{})
	e.SetCreditNoteAdjuster(adjuster)
	e.SetCreditHeadroomReader(headroom)

	credited, err := billOnCancel(e, context.Background(), sub)
	if err != nil {
		t.Fatalf("BillOnCancel: %v (must spill, not loud-fail)", err)
	}

	// totalUnused = 15000 * 23/30 = 11500.
	const wantTotal = 11500
	if credited != wantTotal {
		t.Fatalf("credited = %d, want %d", credited, wantTotal)
	}
	if len(adjuster.calls) != 2 {
		t.Fatalf("got %d credit notes, want 2: %+v", len(adjuster.calls), adjuster.calls)
	}
	var sum, upGross, baseGross int64
	for _, c := range adjuster.calls {
		sum += c.gross
		switch c.invoiceID {
		case "inv_up":
			upGross = c.gross
		case "inv_base":
			baseGross = c.gross
		}
	}
	if sum != wantTotal {
		t.Errorf("credit notes sum to %d, want %d", sum, wantTotal)
	}
	// Upgrade invoice capped at its REMAINING headroom 8333-6000=2333; the rest
	// spilled onto the base invoice (within its 10000 headroom).
	if upGross > 2333 {
		t.Errorf("upgrade CN = %d exceeds its remaining headroom 2333", upGross)
	}
	if baseGross > 10000 {
		t.Errorf("base CN = %d exceeds its headroom 10000", baseGross)
	}
	if upGross != 2333 {
		t.Errorf("upgrade CN = %d, want it filled to its 2333 headroom before spilling", upGross)
	}
}
