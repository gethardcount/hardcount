package billing

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestBillOnCancel_FansAcrossUpgradeInvoice is the regression for the money-bug
// audit's seed finding: subscribe in_advance, upgrade mid-period (creating a
// SECOND funding invoice), then cancel. Pre-fix the whole unused credit was
// issued against the single day-1 invoice, overran its credit-note cap,
// hard-errored, and was swallowed to $0 — the customer was silently overcharged.
// Post-fix the credit fans across BOTH funding invoices, each within its own cap.
func TestBillOnCancel_FansAcrossUpgradeInvoice(t *testing.T) {
	periodStart := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC) // 30-day cycle
	changeAt := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC) // upgrade $100 -> $200
	cancelAt := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC) // 23 days unused

	pricing := &mockPricing{plans: map[string]domain.Plan{
		"pln_20x": {ID: "pln_20x", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 20000, BaseBillTiming: domain.BillInAdvance},
	}}

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "max",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{PlanID: "pln_20x", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}

	// Day-1 base invoice ($100 paid) + mid-period upgrade proration invoice
	// ($83.33 paid). The upgrade invoice is identified by its header
	// (subscription_update + source_plan_changed_at in period), not a line at
	// periodStart — exactly the row FindBaseInvoiceForPeriod used to miss.
	baseInv := domain.Invoice{
		ID: "inv_base", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 10000, TotalAmountCents: 10000,
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd,
	}
	baseLine := domain.InvoiceLineItem{
		ID: "ili_base", InvoiceID: "inv_base", LineType: domain.LineTypeBaseFee,
		BillingPeriodStart: &periodStart,
	}
	upInv := domain.Invoice{
		ID: "inv_up", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 8333, TotalAmountCents: 8333,
		BillingReason:       domain.BillingReasonSubscriptionUpdate,
		SourcePlanChangedAt: &changeAt,
		BillingPeriodStart:  changeAt, BillingPeriodEnd: periodEnd,
	}
	inv := &mockInvoices{
		invoices:  []domain.Invoice{baseInv, upInv},
		lineItems: []domain.InvoiceLineItem{baseLine},
	}

	granter := &fakeCreditGranter{}
	adjuster := &fakeCreditNoteAdjuster{}
	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, pricing, inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
	e.SetCreditGranter(granter)
	e.SetCreditNoteAdjuster(adjuster)
	e.SetCreditHeadroomReader(&fakeCreditHeadroom{})

	credited, err := billOnCancel(e, context.Background(), sub)
	if err != nil {
		t.Fatalf("BillOnCancel: %v", err)
	}

	// Authoritative figure = $200 × 23/30 = 15333 (the full unused prepayment
	// across both invoices). Pre-fix this was 0.
	const wantTotal = 15333
	if credited != wantTotal {
		t.Fatalf("credited = %d, want %d (full unused; pre-fix bug returned 0)", credited, wantTotal)
	}
	if credited <= baseInv.TotalAmountCents {
		t.Fatalf("credited %d must exceed the single base invoice total %d — the whole point of the fix",
			credited, baseInv.TotalAmountCents)
	}

	// Two credit notes, one per funding invoice, each within its own cap, summing
	// to the authoritative total.
	if len(adjuster.calls) != 2 {
		t.Fatalf("got %d credit notes, want 2 (one per funding invoice): %+v", len(adjuster.calls), adjuster.calls)
	}
	var sum int64
	caps := map[string]int64{"inv_base": baseInv.TotalAmountCents, "inv_up": upInv.TotalAmountCents}
	for _, c := range adjuster.calls {
		sum += c.gross
		if c.gross > caps[c.invoiceID] {
			t.Errorf("credit note on %s = %d exceeds its invoice cap %d", c.invoiceID, c.gross, caps[c.invoiceID])
		}
	}
	if sum != wantTotal {
		t.Errorf("credit notes sum to %d, want %d", sum, wantTotal)
	}
	if len(granter.grants) != 0 {
		t.Errorf("paid sources must credit via credit notes (tax-reversed), not bare ledger grants; got %d grants", len(granter.grants))
	}
}
