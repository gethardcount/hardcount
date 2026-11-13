package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

// A provider that does not answer: the engine leaves the proration invoice's
// tax pending, with no error, and the result says whether its prices include
// tax. The draft for the tax retry records it.
func TestUpdateItem_PendingTax_DraftRecordsTheFlag(t *testing.T) {
	for _, inclusive := range []bool{false, true} {
		ctx := context.Background()
		store := newMemStore()
		subID, itemID := seedSubWithItem(t, store, "t1", "cus_1", "plan_old")
		plans := &plansMock{plans: map[string]domain.Plan{
			"plan_old": {ID: "plan_old", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			"plan_new": {ID: "plan_new", Name: "Pro", BaseAmountCents: 3000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		}}
		invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}
		h := NewHandler(NewService(store, nil))
		h.SetProrationDeps(plans, invoices)
		h.SetProrationTaxApplier(&prorationTaxApplierMock{result: ProrationTaxResult{
			TaxFacts:     domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage", TaxPendingReason: "tax provider: 503"},
			TaxInclusive: inclusive,
		}})

		body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
		rr := httptest.NewRecorder()
		h.updateItem(rr, updateItemURL(auth.WithTenantID(ctx, "t1"), subID, itemID, body))
		if rr.Code != http.StatusOK || len(invoices.createdInvoices) != 1 {
			t.Fatalf("inclusive=%v: status %d, %d invoices; want 200 and one draft", inclusive, rr.Code, len(invoices.createdInvoices))
		}
		inv := invoices.createdInvoices[0]
		if inv.Status != domain.InvoiceDraft || inv.TaxStatus != domain.InvoiceTaxPending {
			t.Errorf("inclusive=%v: status %q, tax status %q; want a draft with its tax pending", inclusive, inv.Status, inv.TaxStatus)
		}
		if inv.TaxInclusive != inclusive {
			t.Errorf("inclusive=%v: the draft records TaxInclusive=%v", inclusive, inv.TaxInclusive)
		}
	}
}
