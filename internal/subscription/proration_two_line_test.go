package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// sumLineAmounts / sumLineTax / sumLineTotals are tiny helpers for the
// two-line invariant assertions.
func sumLineAmounts(lines []domain.InvoiceLineItem) (amt, tax, total int64) {
	for _, l := range lines {
		amt += l.AmountCents
		tax += l.TaxAmountCents
		total += l.TotalAmountCents
	}
	return
}

// TestUpdateItem_PlanUpgrade_RendersTwoLines locks ADR-048 Phase C: a mid-cycle
// PLAN upgrade emits ONE invoice with TWO line items — a NEGATIVE credit for
// unused time on the old plan and a POSITIVE charge for remaining time on the
// new plan — that sum to the same net the single line used to show.
func TestUpdateItem_PlanUpgrade_RendersTwoLines(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow) // 15 of 30 days remain
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_starter", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_starter": {ID: "plan_starter", Name: "Starter", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_pro":     {ID: "plan_pro", Name: "Pro", BaseAmountCents: 5000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices) // no tax applier → zero tax

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_pro", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("createdInvoices: got %d, want 1", len(invoices.createdInvoices))
	}
	if len(invoices.createdLineItems) != 1 || len(invoices.createdLineItems[0]) != 2 {
		t.Fatalf("line items: got %v, want exactly 2 lines on the upgrade invoice", invoices.createdLineItems)
	}
	lines := invoices.createdLineItems[0]
	credit, charge := lines[0], lines[1]

	// (newAmount-oldAmount)*15/30 = 3000*15/30 = 1500 net; credit = -2000*15/30 = -1000; charge = residual 2500.
	if credit.AmountCents != -1000 {
		t.Errorf("credit line amount: got %d, want -1000 (unused Starter)", credit.AmountCents)
	}
	if charge.AmountCents != 2500 {
		t.Errorf("charge line amount: got %d, want 2500 (remaining Pro)", charge.AmountCents)
	}
	if !strings.Contains(credit.Description, "Unused time on Starter") {
		t.Errorf("credit label: got %q, want 'Unused time on Starter …'", credit.Description)
	}
	if !strings.Contains(charge.Description, "Remaining time on Pro") {
		t.Errorf("charge label: got %q, want 'Remaining time on Pro …'", charge.Description)
	}

	inv := invoices.createdInvoices[0]
	amt, _, total := sumLineAmounts(lines)
	if amt != inv.SubtotalCents || inv.SubtotalCents != 1500 {
		t.Errorf("line amounts sum to %d, invoice subtotal %d, want both 1500", amt, inv.SubtotalCents)
	}
	if total != inv.TotalAmountCents || inv.TotalAmountCents != 1500 {
		t.Errorf("line totals sum to %d, invoice total %d, want both 1500", total, inv.TotalAmountCents)
	}
}

// TestUpdateItem_PlanUpgrade_TwoLineTaxApportionsAndTiesOut proves the per-line
// tax is the structurally-correct partition (credit line carries the NEGATIVE
// reversed slice, charge line the positive remainder) and that the sums tie
// back EXACTLY to the invoice tax/total — i.e. the invoice is unchanged. It
// also proves the provider was called with a SINGLE positive net line (the
// split runs after tax; Stripe Tax must never see a negative line).
func TestUpdateItem_PlanUpgrade_TwoLineTaxApportionsAndTiesOut(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_starter", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_starter": {ID: "plan_starter", Name: "Starter", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_pro":     {ID: "plan_pro", Name: "Pro", BaseAmountCents: 5000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}
	// 10% tax on the 1500 net = 150.
	taxMock := &prorationTaxApplierMock{result: ProrationTaxResult{TaxFacts: domain.TaxFacts{TaxAmountCents: 150, TaxRate: 10, TaxName: "VAT"}}}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetProrationTaxApplier(taxMock)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_pro", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)

	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	// The provider saw the single positive net line, never a negative one.
	if taxMock.receivedLineCount != 1 {
		t.Errorf("tax provider received %d lines, want 1 (split must run AFTER tax)", taxMock.receivedLineCount)
	}
	if taxMock.receivedNegative {
		t.Error("tax provider was handed a NEGATIVE line — Stripe Tax rejects that; split must run after tax")
	}

	lines := invoices.createdLineItems[0]
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	credit, charge := lines[0], lines[1]
	// creditTax = round(150 * -1000 / 1500) = -100; chargeTax = 150 - (-100) = 250.
	if credit.TaxAmountCents != -100 {
		t.Errorf("credit line tax: got %d, want -100 (reversed slice on unused old)", credit.TaxAmountCents)
	}
	if charge.TaxAmountCents != 250 {
		t.Errorf("charge line tax: got %d, want 250", charge.TaxAmountCents)
	}

	inv := invoices.createdInvoices[0]
	amt, tax, total := sumLineAmounts(lines)
	if amt != inv.SubtotalCents {
		t.Errorf("Σ line amount %d != invoice subtotal %d", amt, inv.SubtotalCents)
	}
	if tax != inv.TaxAmountCents || inv.TaxAmountCents != 150 {
		t.Errorf("Σ line tax %d != invoice tax %d (want 150)", tax, inv.TaxAmountCents)
	}
	if total != inv.TotalAmountCents || inv.TotalAmountCents != 1650 {
		t.Errorf("Σ line total %d != invoice total %d (want 1650)", total, inv.TotalAmountCents)
	}
	// Per-line tax rate inherited from the provider result.
	if credit.TaxRate != 10 || charge.TaxRate != 10 {
		t.Errorf("per-line tax rate: credit %g charge %g, want 10/10", credit.TaxRate, charge.TaxRate)
	}
}

// TestUpdateItem_QuantityIncrease_RendersTwoLines locks the quantity-change
// extension of ADR-048 Phase C: a mid-cycle seat increase emits the same
// credit-unused / charge-remaining pair a plan upgrade does (Stripe stamps
// the identical shape for quantity updates). The pre-fix single line showed
// the NEW total quantity against the DELTA-only amount, so the derived unit
// price was a fiction and qty × unit visibly disagreed with the amount
// (3 × $33.33 ≠ $100.00 — integer truncation).
func TestUpdateItem_QuantityIncrease_RendersTwoLines(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow) // 15 of 30 days remain
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_seats", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}
	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)

	newQty := int64(3)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &newQty})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdLineItems) != 1 || len(invoices.createdLineItems[0]) != 2 {
		t.Fatalf("quantity increase must render TWO lines (credit unused + charge remaining), got %v", invoices.createdLineItems)
	}
	lines := invoices.createdLineItems[0]
	credit, charge := lines[0], lines[1]

	// 1→3 seats at $10.00/seat, half period: net = 2×1000×15/30 = 1000.
	// credit = −(1×1000×15/30) = −500; charge = residual 1500 = 3 seats × $5.00.
	if credit.AmountCents != -500 || credit.Quantity != 1 || credit.UnitAmountCents != -500 {
		t.Errorf("credit line: got amount=%d qty=%d unit=%d, want -500/1/-500",
			credit.AmountCents, credit.Quantity, credit.UnitAmountCents)
	}
	if charge.AmountCents != 1500 || charge.Quantity != 3 || charge.UnitAmountCents != 500 {
		t.Errorf("charge line: got amount=%d qty=%d unit=%d, want 1500/3/500",
			charge.AmountCents, charge.Quantity, charge.UnitAmountCents)
	}
	// qty × unit must reconstruct the amount on BOTH lines — the exact
	// coherence the single-line display violated.
	if credit.Quantity*credit.UnitAmountCents != credit.AmountCents {
		t.Errorf("credit line incoherent: %d × %d ≠ %d", credit.Quantity, credit.UnitAmountCents, credit.AmountCents)
	}
	if charge.Quantity*charge.UnitAmountCents != charge.AmountCents {
		t.Errorf("charge line incoherent: %d × %d ≠ %d", charge.Quantity, charge.UnitAmountCents, charge.AmountCents)
	}
	if !strings.Contains(credit.Description, "Unused time on 1 × Seat") {
		t.Errorf("credit label: got %q, want 'Unused time on 1 × Seat …'", credit.Description)
	}
	if !strings.Contains(charge.Description, "Remaining time on 3 × Seat") {
		t.Errorf("charge label: got %q, want 'Remaining time on 3 × Seat …'", charge.Description)
	}

	inv := invoices.createdInvoices[0]
	amt, _, total := sumLineAmounts(lines)
	if amt != inv.SubtotalCents || inv.SubtotalCents != 1000 {
		t.Errorf("line amounts sum to %d, invoice subtotal %d, want both 1000", amt, inv.SubtotalCents)
	}
	if total != inv.TotalAmountCents || inv.TotalAmountCents != 1000 {
		t.Errorf("line totals sum to %d, invoice total %d, want both 1000", total, inv.TotalAmountCents)
	}
}

// TestUpdateItem_ItemAdd_KeepsSingleLine confirms the split's remaining scope
// boundary: an item ADD has no unused old slice to credit (oldAmount is 0 —
// the credit half would be a $0 line), so it keeps the single net line.
func TestUpdateItem_ItemAdd_KeepsSingleLine(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	t.Run("item add → single line", func(t *testing.T) {
		store := newMemStore()
		subID, _ := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_existing", proPeriodStart, proPeriodEnd)
		svc := NewService(store, nil)
		plans := &plansMock{plans: map[string]domain.Plan{
			"plan_existing": {ID: "plan_existing", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			"plan_addon":    {ID: "plan_addon", Name: "Add-on", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		}}
		invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}
		h := NewHandler(svc)
		h.SetProrationDeps(plans, invoices)

		body, _ := json.Marshal(AddItemInput{PlanID: "plan_addon", Quantity: 1})
		req := addItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, body)
		rr := httptest.NewRecorder()
		h.addItem(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		if len(invoices.createdLineItems) != 1 || len(invoices.createdLineItems[0]) != 1 {
			t.Errorf("item add must keep ONE line, got %v", invoices.createdLineItems)
		}
	})
}
