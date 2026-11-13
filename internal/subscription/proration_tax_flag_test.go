package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

// prorationWithTaxError runs an immediate plan change whose tax application
// fails with err, and returns the response code and the invoices written.
func prorationWithTaxError(t *testing.T, err error) (int, []domain.Invoice) {
	t.Helper()
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
	h.SetProrationTaxApplier(&prorationTaxApplierMock{err: err})

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	rr := httptest.NewRecorder()
	h.updateItem(rr, updateItemURL(auth.WithTenantID(ctx, "t1"), subID, itemID, body))
	return rr.Code, invoices.createdInvoices
}

// When the tax settings could not be read, whether the proration's prices
// include tax is not known. The change is refused and no invoice is written,
// rather than a deferred draft that guesses the flag.
func TestUpdateItem_TaxSettingsUnread_RefusesTheChange(t *testing.T) {
	code, written := prorationWithTaxError(t, errors.New("read tenant settings: connection refused"))
	if code < http.StatusBadRequest {
		t.Errorf("status = %d, want the change refused", code)
	}
	if len(written) != 0 {
		t.Errorf("invoices written = %d, want none: a draft would have to guess whether its prices include tax", len(written))
	}
}

// A tax failure after the settings were read: the proration invoice is a
// deferred draft for the tax retry, and it records whether its prices include
// tax, from the failure.
func TestUpdateItem_TaxFailedOnPricedLines_DraftRecordsTheFlag(t *testing.T) {
	for _, inclusive := range []bool{false, true} {
		code, written := prorationWithTaxError(t, &domain.PricedTaxError{TaxInclusive: inclusive, Err: errors.New("stripe tax: 503 service unavailable")})
		if code != http.StatusOK || len(written) != 1 {
			t.Fatalf("inclusive=%v: status %d, %d invoices; want 200 and one deferred draft", inclusive, code, len(written))
		}
		inv := written[0]
		if inv.TaxStatus != domain.InvoiceTaxPending {
			t.Errorf("inclusive=%v: tax status %q, want pending", inclusive, inv.TaxStatus)
		}
		if inv.TaxInclusive != inclusive {
			t.Errorf("inclusive=%v: the draft records TaxInclusive=%v", inclusive, inv.TaxInclusive)
		}
	}
}
