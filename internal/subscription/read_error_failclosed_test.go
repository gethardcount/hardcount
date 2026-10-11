package subscription

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// A read error fails the step; only "not found" (genuinely not set) skips.
// Each test's old-code behaviour carried on with the read silently skipped.

var errReadBlip = errors.New("connection reset")

// blipOnceStore fails the FIRST Get / GetItem it is asked for, then serves
// normally — the shape of a transient DB blip on the handler's snapshot read.
// A store that always failed would also fail the mutation itself and could
// not tell the old code from the new.
type blipOnceStore struct {
	*memStore
	failGet, failGetItem bool
}

func (b *blipOnceStore) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	if b.failGet {
		b.failGet = false
		return domain.Subscription{}, errReadBlip
	}
	return b.memStore.Get(ctx, tenantID, id)
}

func (b *blipOnceStore) GetItem(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	if b.failGetItem {
		b.failGetItem = false
		return domain.SubscriptionItem{}, errReadBlip
	}
	return b.memStore.GetItem(ctx, tenantID, itemID)
}

type errProfileReader struct{ err error }

func (e errProfileReader) GetBillingProfile(context.Context, string, string) (domain.CustomerBillingProfile, error) {
	return domain.CustomerBillingProfile{}, e.err
}

type errCustomerReader struct{ err error }

func (e errCustomerReader) Get(context.Context, string, string) (domain.Customer, error) {
	return domain.Customer{}, e.err
}

// The outgoing plan of a swap pins the currency; a read error used to drop
// it, so a cross-currency swap passed and proration subtracted raw cents.
func TestCurrencyPin_OutgoingPlanReadErrorFails(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
		"pln_eur": {ID: "pln_eur", Code: "eur", Currency: "EUR"},
	}}) // "pln_out" is unreadable
	if err := svc.rejectCurrencyMismatch(context.Background(), "t1", "cus_1", "", []string{"pln_eur"}, "pln_out"); err == nil {
		t.Fatal("outgoing plan unreadable: err = nil, want an error")
	}
}

// A billing-profile read error fails the pin; a missing profile does not.
func TestCurrencyPin_ProfileReadErrorFails(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
		"pln_usd": {ID: "pln_usd", Code: "usd", Currency: "USD"},
	}})
	svc.SetProfileReader(errProfileReader{err: errReadBlip})
	if err := svc.rejectCurrencyMismatch(context.Background(), "t1", "cus_1", "", []string{"pln_usd"}, ""); !errors.Is(err, errReadBlip) {
		t.Fatalf("profile read error: err = %v, want the read error", err)
	}
	svc.SetProfileReader(errProfileReader{err: errs.ErrNotFound})
	if err := svc.rejectCurrencyMismatch(context.Background(), "t1", "cus_1", "", []string{"pln_usd"}, ""); err != nil {
		t.Fatalf("no profile: err = %v, want nil", err)
	}
}

// A customer read error fails Create with nothing written: it used to skip
// the archived guard and the test-clock inheritance and create the sub.
func TestCreate_CustomerReadErrorFails(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil)
	svc.SetCustomerReader(errCustomerReader{err: errReadBlip})
	_, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "s-cust-blip", DisplayName: "S", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_1", Quantity: 1}},
	})
	if !errors.Is(err, errReadBlip) {
		t.Fatalf("err = %v, want the customer read error", err)
	}
	if n := len(store.subs); n != 0 {
		t.Fatalf("subscriptions written: %d, want 0", n)
	}
}

func withTenantRoute(req *http.Request, tenantID string, params map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	return req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID))
}

func prorationHandler(t *testing.T) (*Handler, *blipOnceStore, string, string) {
	t.Helper()
	mem := newMemStore()
	subID, itemID := seedSubWithItemAt(t, mem, "t1", "cus_1", "plan_existing", proPeriodStart, proPeriodEnd)
	store := &blipOnceStore{memStore: mem}
	h := NewHandler(NewService(store, nil))
	h.SetProrationDeps(&plansMock{plans: map[string]domain.Plan{
		"plan_existing": {ID: "plan_existing", Name: "Basic", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_new":      {ID: "plan_new", Name: "Add-on", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}, &invoicesMock{sourceInvoice: domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded}}, &creditsMock{})
	return h, store, subID, itemID
}

func proCtx() context.Context { return clock.WithEffectiveNow(context.Background(), proNow) }

// The thresholds currency guard: a sub read error used to skip it.
func TestSetBillingThresholds_SubReadErrorFails(t *testing.T) {
	h, store, subID, _ := prorationHandler(t)
	store.failGet = true
	body, _ := json.Marshal(BillingThresholdsInput{AmountGTE: 5000})
	req := withTenantRoute(httptest.NewRequestWithContext(proCtx(), http.MethodPut, "/subscriptions/"+subID+"/billing-thresholds", bytes.NewReader(body)), "t1", map[string]string{"id": subID})
	rr := httptest.NewRecorder()
	h.setBillingThresholds(rr, req)
	if rr.Code < 500 {
		t.Fatalf("status = %d, want 5xx on a read error. body=%s", rr.Code, rr.Body.String())
	}
}

// Add-item: a sub read error used to add the item with proration off.
func TestAddItem_SubReadErrorFailsWithNothingAdded(t *testing.T) {
	h, store, subID, _ := prorationHandler(t)
	store.failGet = true
	body, _ := json.Marshal(AddItemInput{PlanID: "plan_new", Quantity: 1})
	req := withTenantRoute(httptest.NewRequestWithContext(proCtx(), http.MethodPost, "/subscriptions/"+subID+"/items", bytes.NewReader(body)), "t1", map[string]string{"id": subID})
	rr := httptest.NewRecorder()
	h.addItem(rr, req)
	if rr.Code < 500 {
		t.Fatalf("status = %d, want 5xx. body=%s", rr.Code, rr.Body.String())
	}
	if sub, _ := store.memStore.Get(context.Background(), "t1", subID); len(sub.Items) != 1 {
		t.Fatalf("items = %d, want 1 (nothing added)", len(sub.Items))
	}
}

// Update-item: an item read error used to apply the change with proration off.
func TestUpdateItem_ItemReadErrorFailsWithNothingChanged(t *testing.T) {
	h, store, subID, itemID := prorationHandler(t)
	store.failGetItem = true
	q := int64(3)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &q})
	req := withTenantRoute(httptest.NewRequestWithContext(proCtx(), http.MethodPatch, "/subscriptions/"+subID+"/items/"+itemID, bytes.NewReader(body)), "t1", map[string]string{"id": subID, "itemID": itemID})
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code < 500 {
		t.Fatalf("status = %d, want 5xx. body=%s", rr.Code, rr.Body.String())
	}
	if it, _ := store.memStore.GetItem(context.Background(), "t1", itemID); it.Quantity != 1 {
		t.Fatalf("quantity = %d, want 1 (unchanged)", it.Quantity)
	}
}

// Remove-item: an item read error used to hard-delete with no unused credit.
func TestRemoveItem_ItemReadErrorFailsWithNothingRemoved(t *testing.T) {
	h, store, subID, itemID := prorationHandler(t)
	if _, err := h.svc.AddItem(context.Background(), "t1", subID, AddItemInput{PlanID: "plan_new", Quantity: 1}); err != nil {
		t.Fatalf("seed second item: %v", err)
	}
	store.failGetItem = true
	req := withTenantRoute(httptest.NewRequestWithContext(proCtx(), http.MethodDelete, "/subscriptions/"+subID+"/items/"+itemID, nil), "t1", map[string]string{"id": subID, "itemID": itemID})
	rr := httptest.NewRecorder()
	h.removeItem(rr, req)
	if rr.Code < 500 {
		t.Fatalf("status = %d, want 5xx. body=%s", rr.Code, rr.Body.String())
	}
	if _, err := store.memStore.GetItem(context.Background(), "t1", itemID); err != nil {
		t.Fatalf("item gone after a failed remove: %v", err)
	}
}

// Update-item: a subscription read error used to leave the remaining days at
// 0, so the change applied with proration silently off.
func TestUpdateItem_SubReadErrorFailsWithNothingChanged(t *testing.T) {
	h, store, subID, itemID := prorationHandler(t)
	store.failGet = true
	q := int64(3)
	body, _ := json.Marshal(UpdateItemInput{Quantity: &q})
	req := withTenantRoute(httptest.NewRequestWithContext(proCtx(), http.MethodPatch, "/subscriptions/"+subID+"/items/"+itemID, bytes.NewReader(body)), "t1", map[string]string{"id": subID, "itemID": itemID})
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code < 500 {
		t.Fatalf("status = %d, want 5xx. body=%s", rr.Code, rr.Body.String())
	}
	if it, _ := store.memStore.GetItem(context.Background(), "t1", itemID); it.Quantity != 1 {
		t.Fatalf("quantity = %d, want 1 (unchanged)", it.Quantity)
	}
}

// failPlanOnce errors the first read of one plan id.
type failPlanOnce struct {
	*plansMock
	id     string
	failed bool
}

func (f *failPlanOnce) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	if id == f.id && !f.failed {
		f.failed = true
		return domain.Plan{}, errors.New("connection reset")
	}
	return f.plansMock.GetPlan(ctx, tenantID, id)
}

// Update-item plan swap: a read error on the outgoing plan used to read as
// "not a cross-interval swap" and route the change down the non-atomic path
// (ADR-056) with the wrong proration.
func TestUpdateItem_OldPlanReadErrorFailsWithNothingChanged(t *testing.T) {
	h, store, subID, itemID := prorationHandler(t)
	plans := h.plans.(*plansMock)
	h.plans = &failPlanOnce{plansMock: plans, id: "plan_existing"}
	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
	req := withTenantRoute(httptest.NewRequestWithContext(proCtx(), http.MethodPatch, "/subscriptions/"+subID+"/items/"+itemID, bytes.NewReader(body)), "t1", map[string]string{"id": subID, "itemID": itemID})
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)
	if rr.Code < 500 {
		t.Fatalf("status = %d, want 5xx. body=%s", rr.Code, rr.Body.String())
	}
	if it, _ := store.memStore.GetItem(context.Background(), "t1", itemID); it.PlanID != "plan_existing" {
		t.Fatalf("plan = %s, want plan_existing (unchanged)", it.PlanID)
	}
}

// Remove-item: a subscription read error used to hard-delete the item with no
// credit for the unused portion.
func TestRemoveItem_SubReadErrorFailsWithNothingRemoved(t *testing.T) {
	h, store, subID, itemID := prorationHandler(t)
	if _, err := h.svc.AddItem(context.Background(), "t1", subID, AddItemInput{PlanID: "plan_new", Quantity: 1}); err != nil {
		t.Fatalf("seed second item: %v", err)
	}
	store.failGet = true
	req := withTenantRoute(httptest.NewRequestWithContext(proCtx(), http.MethodDelete, "/subscriptions/"+subID+"/items/"+itemID, nil), "t1", map[string]string{"id": subID, "itemID": itemID})
	rr := httptest.NewRecorder()
	h.removeItem(rr, req)
	if rr.Code < 500 {
		t.Fatalf("status = %d, want 5xx. body=%s", rr.Code, rr.Body.String())
	}
	if _, err := store.memStore.GetItem(context.Background(), "t1", itemID); err != nil {
		t.Fatalf("item gone after a failed remove: %v", err)
	}
}
