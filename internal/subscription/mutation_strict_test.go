package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
)

// TestMutationRoutes_UnknownFieldRefused: the subscription mutation routes
// dropped an unknown key silently, so a typo changed money. `"immediately"`
// instead of `"immediate"` on an item PATCH answered 200 and moved the plan
// change to period end; `"reset_billing_cycles"` on PUT /billing-thresholds
// saved the default. Each route now refuses the key with a 422 naming it,
// before the service runs (the handler has no subscription to act on).
func TestMutationRoutes_UnknownFieldRefused(t *testing.T) {
	h := NewHandler(NewService(newMemStore(), nil))
	srv := h.Routes()

	cases := []struct {
		method, path, body, param string
	}{
		{http.MethodPatch, "/sub_1/items/item_1", `{"new_plan_id":"plan_2","immediately":true}`, "immediately"},
		{http.MethodPost, "/sub_1/items", `{"plan_id":"plan_2","qty":3}`, "qty"},
		{http.MethodPut, "/sub_1/billing-thresholds", `{"amount_gte":5000,"reset_billing_cycles":false}`, "reset_billing_cycles"},
		{http.MethodPut, "/sub_1/billing-thresholds", `{"item_thresholds":[{"subscription_item_id":"item_1","usage":"10"}]}`, "usage"},
		{http.MethodPost, "/sub_1/schedule-cancel", `{"at_period_end":true,"cancel_at_period_end":true}`, "cancel_at_period_end"},
		{http.MethodPut, "/sub_1/pause-collection", `{"behavior":"keep_as_draft","resume_at":"2027-01-01T00:00:00Z"}`, "resume_at"},
	}
	runUnknownFieldCases(t, srv, cases)
}

// TestSetBillingThresholds_ResetBillingCycleRemoved: a spend threshold
// always keeps the billing period (2026-10-23). reset_billing_cycle is gone
// from the body, so either value is refused with a 422 naming it, and
// nothing is saved: true no longer starts a new period at the fire, and
// false is the only behavior left, which needs no field.
func TestSetBillingThresholds_ResetBillingCycleRemoved(t *testing.T) {
	h := NewHandler(NewService(newMemStore(), nil))
	runUnknownFieldCases(t, h.Routes(), []struct{ method, path, body, param string }{
		{http.MethodPut, "/sub_1/billing-thresholds", `{"amount_gte":5000,"reset_billing_cycle":true}`, "reset_billing_cycle"},
		{http.MethodPut, "/sub_1/billing-thresholds", `{"amount_gte":5000,"reset_billing_cycle":false}`, "reset_billing_cycle"},
	})
}

func runUnknownFieldCases(t *testing.T, srv http.Handler, cases []struct{ method, path, body, param string }) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.method+" "+c.path+" "+c.body, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			req = req.WithContext(context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1"))
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
			}
			var env struct {
				Error struct {
					Param string `json:"param"`
				} `json:"error"`
			}
			_ = json.Unmarshal(rr.Body.Bytes(), &env)
			if env.Error.Param != c.param {
				t.Errorf("param: got %q, want %q", env.Error.Param, c.param)
			}
		})
	}
}
