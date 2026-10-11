package billing

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// TestBillOnCancel_PaidPrebillReversesTax proves the cancel site actually
// routes a PAID, taxed prebill clawback through the credit-note primitive
// (ADR-048): the customer is credited the GROSS unused via a CN (which reverses
// the proportional tax), not the bare net ledger grant that dropped the tax.
func TestBillOnCancel_PaidPrebillReversesTax(t *testing.T) {
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) // 31-day cycle
	cancelAt := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC) // 16 unused / 31
	ps := periodStart

	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "starter",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{PlanID: "pln_advance", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		CanceledAt:                &cancelAt,
	}
	pricing := &mockPricing{plans: map[string]domain.Plan{
		"pln_advance": {ID: "pln_advance", Name: "Advance", Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseAmountCents: 6000, BaseBillTiming: domain.BillInAdvance},
	}}
	// PAID in_advance invoice with 10% tax.
	inv := &mockInvoices{
		invoices: []domain.Invoice{{
			ID: "inv_1", TenantID: "t1", SubscriptionID: "sub_1",
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600,
			AmountDueCents: 0, AmountPaidCents: 6600,
		}},
		lineItems: []domain.InvoiceLineItem{{
			ID: "ili_1", InvoiceID: "inv_1", LineType: domain.LineTypeBaseFee, BillingPeriodStart: &ps,
		}},
	}
	g := &fakeCreditGranter{}
	a := &fakeCreditNoteAdjuster{}
	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, pricing, inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
	e.SetCreditGranter(g)
	e.SetCreditNoteAdjuster(a)
	e.SetCreditHeadroomReader(&fakeCreditHeadroom{})

	netUnused := money.RoundHalfToEven(6000*16, 31)            // pre-fix bare grant
	grossUnused := money.RoundHalfToEven(netUnused*6600, 6000) // grossed up by the invoice's tax

	cents, err := billOnCancel(e, context.Background(), sub)
	if err != nil {
		t.Fatalf("BillOnCancel: %v", err)
	}
	if grossUnused <= netUnused {
		t.Fatalf("setup: gross (%d) must exceed net (%d)", grossUnused, netUnused)
	}
	if len(a.calls) != 1 || a.calls[0].invoiceID != "inv_1" || a.calls[0].gross != grossUnused {
		t.Errorf("credit-note adjustment = %+v, want one call on inv_1 for gross %d", a.calls, grossUnused)
	}
	if len(g.grants) != 0 {
		t.Errorf("bare ledger grant fired %d times — the paid cancel must route through the CN", len(g.grants))
	}
	if cents != grossUnused {
		t.Errorf("returned cents = %d, want gross %d", cents, grossUnused)
	}
}
