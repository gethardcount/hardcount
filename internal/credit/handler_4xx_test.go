package credit

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
)

// Handler-layer 4xx coverage (code-quality audit gap #3): the credit
// handlers had zero direct HTTP tests. These pin the request-decode and
// validation error contract for the money-mutating grant/adjust paths.

func creditHandler() *Handler { return NewHandler(NewService(newMemStore())) }

func creditReq(t *testing.T, fn func(http.ResponseWriter, *http.Request), method, body string, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", bytes.NewBufferString(body))
	ctx := auth.WithTenantID(req.Context(), "tnt_test")
	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	fn(rr, req)
	return rr
}

func TestCreditHandler_BadJSON_Returns400(t *testing.T) {
	h := creditHandler()
	for _, c := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{{"grant", h.grant}, {"adjust", h.adjust}} {
		t.Run(c.name, func(t *testing.T) {
			if rr := creditReq(t, c.fn, http.MethodPost, "{bad", nil); rr.Code != http.StatusBadRequest {
				t.Errorf("%s bad JSON: got %d, want 400; body=%s", c.name, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestCreditHandler_Validation_Returns422(t *testing.T) {
	h := creditHandler()
	for _, c := range []struct {
		name, body string
		fn         func(http.ResponseWriter, *http.Request)
	}{
		{"grant missing customer_id", `{"amount_cents":100,"description":"x"}`, h.grant},
		{"grant zero amount", `{"customer_id":"cus_1","amount_cents":0,"description":"x"}`, h.grant},
		{"grant amount over cap", `{"customer_id":"cus_1","amount_cents":100000001,"description":"x"}`, h.grant},
		{"grant missing description", `{"customer_id":"cus_1","amount_cents":100}`, h.grant},
		{"adjust missing customer_id", `{"amount_cents":100,"description":"x"}`, h.adjust},
		{"adjust zero amount", `{"customer_id":"cus_1","amount_cents":0,"description":"x"}`, h.adjust},
		{"adjust missing description", `{"customer_id":"cus_1","amount_cents":100}`, h.adjust},
	} {
		t.Run(c.name, func(t *testing.T) {
			if rr := creditReq(t, c.fn, http.MethodPost, c.body, nil); rr.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: got %d, want 422; body=%s", c.name, rr.Code, rr.Body.String())
			}
		})
	}
}
