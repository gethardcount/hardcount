package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// An amount on the proration invoice that does not fit int64 cents is not a
// failure a retry gets past: the change is refused, but not with 503 and a
// Retry-After, which would have a client send it again for ever. No invoice
// is written.
func TestUpdateItem_ProrationTaxOverflow_RefusedWithoutARetry(t *testing.T) {
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
	h.SetProrationTaxApplier(&prorationTaxApplierMock{err: fmt.Errorf("line 0 total with tax: %w", domain.ErrAmountOverflow)})

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	rr := httptest.NewRecorder()
	h.updateItem(rr, updateItemURL(auth.WithTenantID(ctx, "t1"), subID, itemID, body))

	if rr.Code < http.StatusBadRequest || rr.Code == http.StatusServiceUnavailable {
		t.Errorf("status = %d, want a refusal that is not 503. body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != "" {
		t.Error("an overflow answered with Retry-After: a client would send it again for ever")
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("got %d invoices, want none", len(invoices.createdInvoices))
	}
}

// An item added mid-period on an in-advance plan whose proration's tax cannot
// be applied is refused like an item change: 503 proration_unavailable with a
// Retry-After, no invoice, and no item added.
func TestAddItem_ProrationTaxCannotBeApplied_Refused(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	store := newMemStore()
	subID, _ := seedSubWithItemAt(t, store, "t1", "cus_1", "plan_existing", proPeriodStart, proPeriodEnd)
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_existing": {ID: "plan_existing", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new":      {ID: "plan_new", Name: "Add-on", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}
	h := NewHandler(NewService(store, nil))
	h.SetProrationDeps(plans, invoices)
	h.SetProrationTaxApplier(&prorationTaxApplierMock{err: errors.New("load tenant settings: connection refused")})

	body, _ := json.Marshal(AddItemInput{PlanID: "plan_new", Quantity: 1})
	rr := httptest.NewRecorder()
	h.addItem(rr, addItemURL(auth.WithTenantID(ctx, "t1"), subID, body))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "proration_unavailable") {
		t.Errorf("body = %s, want the proration_unavailable code", rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After: a client cannot tell the refusal is safe to send again")
	}
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("got %d invoices, want none", len(invoices.createdInvoices))
	}
	sub, err := store.Get(ctx, "t1", subID)
	if err != nil {
		t.Fatalf("get sub: %v", err)
	}
	if len(sub.Items) != 1 {
		t.Errorf("items = %d, want the one the subscription had: the item was added", len(sub.Items))
	}
}
