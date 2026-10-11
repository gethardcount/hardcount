package billing

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestSettleUnusedAcrossFunding_MixedPaidUnpaid_NoSilentDrop is the audit
// finding #1 regression: when a period is funded by a PAID invoice (with room)
// and a partially-paid UNPAID one whose amount_due is below its weighted share,
// the unpaid share used to be clamped to amount_due and the overflow SILENTLY
// dropped (neither credited nor redistributed). It must now water-fill the
// overflow onto the paid invoice so the full unused amount is placed.
func TestSettleUnusedAcrossFunding_MixedPaidUnpaid_NoSilentDrop(t *testing.T) {
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
	// Upgrade invoice, PARTIALLY paid: $83.33 billed, $63.33 paid, $20.00 still
	// owed. Its weighted cancel share (~$57.50) far exceeds the $20 it can relieve.
	upInv := domain.Invoice{
		ID: "inv_up", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		SubtotalCents: 8333, TotalAmountCents: 8333, AmountPaidCents: 6333, AmountDueCents: 2000,
		BillingReason: domain.BillingReasonSubscriptionUpdate, SourcePlanChangedAt: &changeAt,
		BillingPeriodStart: changeAt, BillingPeriodEnd: periodEnd,
	}
	inv := &mockInvoices{invoices: []domain.Invoice{baseInv, upInv}, lineItems: []domain.InvoiceLineItem{baseLine}}

	adjuster := &fakeCreditNoteAdjuster{}
	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, pricing, inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
	e.SetCreditGranter(&fakeCreditGranter{})
	e.SetCreditNoteAdjuster(adjuster)
	e.SetCreditHeadroomReader(&fakeCreditHeadroom{})
	e.SetInvoiceVoider(&fakeInvoiceVoider{})

	credited, err := billOnCancel(e, context.Background(), sub)
	if err != nil {
		t.Fatalf("BillOnCancel: %v", err)
	}

	const wantTotal = 11500 // 15000 * 23/30
	// Two adjustments: a credit note on the paid base + an amount_due reduction
	// on the unpaid upgrade. Their grosses must SUM to the full unused — no drop.
	if len(adjuster.calls) != 2 {
		t.Fatalf("got %d adjustments, want 2: %+v", len(adjuster.calls), adjuster.calls)
	}
	var sum, baseGross, upGross int64
	for _, c := range adjuster.calls {
		sum += c.gross
		switch c.invoiceID {
		case "inv_base":
			baseGross = c.gross
		case "inv_up":
			upGross = c.gross
		}
	}
	if sum != wantTotal {
		t.Errorf("adjustments sum to %d, want %d (pre-fix dropped the unpaid overflow → ~7750)", sum, wantTotal)
	}
	if upGross > 2000 {
		t.Errorf("unpaid upgrade reduced by %d, exceeds its amount_due 2000", upGross)
	}
	// The base invoice must absorb the overflow: its share is now well above its
	// natural ~5750 weighted share.
	if baseGross <= 5751 {
		t.Errorf("base credit %d did not absorb the unpaid overflow (natural share ~5750)", baseGross)
	}
	// credited = the paid balance credit only (unpaid relief returns 0).
	if credited != baseGross {
		t.Errorf("credited = %d, want %d (paid base credit)", credited, baseGross)
	}
}
