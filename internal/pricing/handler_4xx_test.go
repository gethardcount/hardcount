package pricing

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
)

// Handler-layer 4xx coverage (code-quality audit gap #3): the pricing
// handlers had zero direct HTTP tests, so request-decode / validation /
// not-found branches were exercised only indirectly by the happy-path
// e2e. These pin the observable error contract.

func newTestHandler() *Handler {
	return NewHandler(NewService(newMemStore()))
}

func doJSON(t *testing.T, h func(http.ResponseWriter, *http.Request), method, target, body string, urlParams map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	ctx := auth.WithTenantID(req.Context(), "tnt_test")
	if len(urlParams) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range urlParams {
			rctx.URLParams.Add(k, v)
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestPricingHandler_BadJSON_Returns400(t *testing.T) {
	h := newTestHandler()
	cases := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"createPlan", h.createPlan},
		{"createMeter", h.createMeter},
		{"createOverride", h.createOverride},
		{"addPrice", h.addPrice},
		{"editPrice", h.editPrice},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := doJSON(t, c.fn, http.MethodPost, "/", "{not json", map[string]string{"id": "vlx_pln_1", "price_id": "vlx_prc_1"})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s bad JSON: got %d, want 400; body=%s", c.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestPricingHandler_Validation_Returns422(t *testing.T) {
	h := newTestHandler()
	cases := []struct {
		name, body string
		fn         func(http.ResponseWriter, *http.Request)
	}{
		{"plan missing code", `{"name":"P","currency":"USD","billing_interval":"monthly"}`, h.createPlan},
		{"plan missing name", `{"code":"p1","currency":"USD","billing_interval":"monthly"}`, h.createPlan},
		{"plan bad billing_interval", `{"code":"p1","name":"P","currency":"USD","billing_interval":"weekly"}`, h.createPlan},
		{"plan negative base fee", `{"code":"p1","name":"P","currency":"USD","billing_interval":"monthly","base_amount_cents":-5}`, h.createPlan},
		{"meter missing name", `{"aggregation":"sum"}`, h.createMeter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := doJSON(t, c.fn, http.MethodPost, "/", c.body, nil)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: got %d, want 422; body=%s", c.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestPricingHandler_NotFound_Returns404(t *testing.T) {
	h := newTestHandler()
	cases := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"getPlan", h.getPlan},
		{"getMeter", h.getMeter},
		{"listPrices", h.listPrices},
		{"removePrice", h.removePrice},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := doJSON(t, c.fn, http.MethodGet, "/vlx_missing", "", map[string]string{"id": "vlx_missing", "price_id": "vlx_missing"})
			if rr.Code != http.StatusNotFound {
				t.Errorf("%s unknown id: got %d, want 404; body=%s", c.name, rr.Code, rr.Body.String())
			}
		})
	}
}

// A meter only counts and a plan's meters are its prices' meters (ADR-130
// §1): a meter sent with a default price, or a plan with a meter list, is
// refused by name, not silently ignored, and nothing is written. A meter is
// created by event_name; the old key field is refused the same way when it
// is the only name given.
func TestPricingHandler_RemovedFieldsRefused(t *testing.T) {
	store := newMemStore()
	h := NewHandler(NewService(store))
	for _, tc := range []struct {
		name, body, field string
		fn                func(http.ResponseWriter, *http.Request)
	}{
		{"meter with a default price", `{"event_name":"calls","name":"Calls","rating_rule_version_id":"vlx_rrv_x"}`, "rating_rule_version_id", h.createMeter},
		{"meter with key and no event_name", `{"key":"calls","name":"Calls"}`, `"key"`, h.createMeter},
		{"plan with meter_ids", `{"code":"p","name":"p","currency":"USD","billing_interval":"monthly","meter_ids":["vlx_mtr_1"]}`, "meter_ids", h.createPlan},
		{"price with a priority", `{"meter_id":"vlx_mtr_1","mode":"per_unit","unit_amount_cents":"1","priority":100}`, "priority", h.addPrice},
		{"override with a rule key", `{"customer_id":"vlx_cus_1","rule_key":"r","mode":"per_unit","unit_amount_cents":"1"}`, "rule_key", h.createOverride},
	} {
		rr := doJSON(t, tc.fn, http.MethodPost, "/", tc.body, map[string]string{"id": "vlx_pln_1"})
		if rr.Code != http.StatusUnprocessableEntity || !bytes.Contains(rr.Body.Bytes(), []byte(tc.field)) {
			t.Errorf("%s: got %d %s, want 422 naming %s", tc.name, rr.Code, rr.Body.String(), tc.field)
		}
	}
	if len(store.meters) != 0 || len(store.plans) != 0 || len(store.writes) != 0 || len(store.overrides) != 0 {
		t.Errorf("a refused request wrote something: %+v", store)
	}
}
