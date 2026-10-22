package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

func prorationTaxCommitHandler(t *testing.T, taxMock *prorationTaxApplierMock) (*Handler, *invoicesMock, string, string) {
	t.Helper()
	store := newMemStore()
	subID, itemID := seedSubWithItem(t, store, "t1", "cus_1", "plan_old")
	svc := NewService(store, nil)
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}
	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices)
	h.SetProrationTaxApplier(taxMock)
	return h, invoices, subID, itemID
}

// An upgrade proration whose tax resolves via Stripe Tax must (a) stamp the
// invoice's tax_provider + tax_calculation_id, and (b) COMMIT the calculation
// into a reportable tax transaction. Pre-fix the adapter dropped the
// provider/calc_id and the path never called CommitTax — so the proration tax
// was charged but never reported to Stripe Tax (an under-remittance gap).
func TestUpdateItem_ProrationStampsProviderAndCommitsTax(t *testing.T) {
	ctx := context.Background()
	taxMock := &prorationTaxApplierMock{result: ProrationTaxResult{
		TaxFacts: domain.TaxFacts{
			TaxAmountCents:   151,
			TaxRate:          8.875,
			TaxName:          "Sales Tax",
			TaxProvider:      "stripe_tax",
			TaxCalculationID: "taxcalc_test_1",
			TaxStatus:        domain.InvoiceTaxOK,
		},
	}}
	h, invoices, subID, itemID := prorationTaxCommitHandler(t, taxMock)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), "t1"), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("got %d proration invoices, want 1", len(invoices.createdInvoices))
	}
	inv := invoices.createdInvoices[0]
	if inv.TaxProvider != "stripe_tax" {
		t.Errorf("invoice tax_provider = %q, want stripe_tax (was dropped pre-fix → blank)", inv.TaxProvider)
	}
	if inv.TaxCalculationID != "taxcalc_test_1" {
		t.Errorf("invoice tax_calculation_id = %q, want taxcalc_test_1", inv.TaxCalculationID)
	}
	if taxMock.commitCalls != 1 {
		t.Fatalf("CommitTax fired %d times, want 1 — proration tax must be committed/reported, not just charged", taxMock.commitCalls)
	}
	if taxMock.commitInvoiceID != inv.ID || taxMock.commitCalcID != "taxcalc_test_1" {
		t.Errorf("CommitTax(invoiceID=%q, calcID=%q), want (%q, taxcalc_test_1)", taxMock.commitInvoiceID, taxMock.commitCalcID, inv.ID)
	}
}

// A reverse-charge or exempt customer's proration invoice must carry the
// exemption status onto the invoice. Pre-fix ProrationTaxResult and its adapter
// dropped TaxReverseCharge + TaxExemptReason, so a mid-cycle upgrade for a
// reverse-charge customer produced a proration invoice with
// tax_reverse_charge=false and a blank exempt reason — silently dropping the
// legally required reverse-charge / exemption legend on exactly that invoice.
func TestUpdateItem_ProrationCarriesExemptionStatus(t *testing.T) {
	t.Run("reverse_charge flag flows to the invoice", func(t *testing.T) {
		ctx := context.Background()
		taxMock := &prorationTaxApplierMock{result: ProrationTaxResult{
			TaxFacts: domain.TaxFacts{
				TaxProvider: "stripe_tax", TaxReverseCharge: true,
				TaxStatus: domain.InvoiceTaxOK,
			},
		}}
		h, invoices, subID, itemID := prorationTaxCommitHandler(t, taxMock)
		body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
		req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), "t1"), subID, itemID, body)
		rr := httptest.NewRecorder()
		h.updateItem(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
		}
		if len(invoices.createdInvoices) != 1 {
			t.Fatalf("got %d proration invoices, want 1", len(invoices.createdInvoices))
		}
		if !invoices.createdInvoices[0].TaxReverseCharge {
			t.Error("proration invoice tax_reverse_charge = false, want true — the reverse-charge legend would be dropped on this invoice")
		}
	})

	t.Run("exempt reason flows to the invoice", func(t *testing.T) {
		ctx := context.Background()
		taxMock := &prorationTaxApplierMock{result: ProrationTaxResult{
			TaxFacts: domain.TaxFacts{
				TaxProvider: "manual", TaxExemptReason: "Reseller certificate",
				TaxStatus: domain.InvoiceTaxOK,
			},
		}}
		h, invoices, subID, itemID := prorationTaxCommitHandler(t, taxMock)
		body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
		req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), "t1"), subID, itemID, body)
		rr := httptest.NewRecorder()
		h.updateItem(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
		}
		if len(invoices.createdInvoices) != 1 {
			t.Fatalf("got %d proration invoices, want 1", len(invoices.createdInvoices))
		}
		if got := invoices.createdInvoices[0].TaxExemptReason; got != "Reseller certificate" {
			t.Errorf("proration invoice tax_exempt_reason = %q, want %q", got, "Reseller certificate")
		}
	})
}

// Manual / none providers produce no calculation id — CommitTax must NOT fire
// (the guard is provider AND calc id non-empty), but the provider is still
// recorded on the invoice for provenance.
func TestUpdateItem_ProrationManualProviderDoesNotCommit(t *testing.T) {
	ctx := context.Background()
	taxMock := &prorationTaxApplierMock{result: ProrationTaxResult{
		TaxFacts: domain.TaxFacts{
			TaxAmountCents: 145,
			TaxRate:        7.25,
			TaxName:        "Sales Tax",
			TaxProvider:    "manual",
			// no TaxCalculationID — manual provider computes locally
			TaxStatus: domain.InvoiceTaxOK,
		},
	}}
	h, invoices, subID, itemID := prorationTaxCommitHandler(t, taxMock)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), "t1"), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(invoices.createdInvoices) != 1 {
		t.Fatalf("got %d proration invoices, want 1", len(invoices.createdInvoices))
	}
	if got := invoices.createdInvoices[0].TaxProvider; got != "manual" {
		t.Errorf("invoice tax_provider = %q, want manual", got)
	}
	if taxMock.commitCalls != 0 {
		t.Errorf("CommitTax fired %d times for a manual provider, want 0 (no calculation to commit)", taxMock.commitCalls)
	}
}
