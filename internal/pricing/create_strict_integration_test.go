package pricing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestPlanCreateAndStatus_Validated covers the plan routes:
//   - POST /v1/plans with `base_amount` (not base_amount_cents) made a $0 plan
//     with a 201. The unknown key is now a 422 naming it.
//   - D-S2: create ignored `status` and made the plan active, so
//     {status:"archived"} looked accepted. A plan is created active; any other
//     status is a 422.
//   - D-09: PATCH with a status outside the plans_status_check set reached
//     the constraint and came back a 500 (cached by the idempotency
//     middleware). It is now a 422 and the row is unchanged.
func TestPlanCreateAndStatus_Validated(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Plan Strict Create")

	svc := pricing.NewService(pricing.NewPostgresStore(db))
	srv := pricing.NewHandler(svc).PlanRoutes()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)))
		return rr
	}
	param := func(rr *httptest.ResponseRecorder) string {
		var env struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		return env.Error.Param
	}
	planCount := func() int {
		plans, err := svc.ListPlans(ctx, tenantID)
		if err != nil {
			t.Fatalf("list plans: %v", err)
		}
		return len(plans)
	}
	body := func(code, extra string) string {
		return `{"code":"` + code + `","name":"` + code + `","currency":"USD","billing_interval":"monthly",` + extra + `}`
	}

	refusals := []struct {
		name, body, param string
	}{
		{"base_amount", body("p-base", `"base_amount":4900`), "base_amount"},
		{"status archived", body("p-arch", `"base_amount_cents":4900,"status":"archived"`), "status"},
		{"status draft", body("p-draft", `"base_amount_cents":4900,"status":"draft"`), "status"},
	}
	for _, tc := range refusals {
		rr := do(http.MethodPost, "/", tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422; body %s", tc.name, rr.Code, rr.Body.String())
		}
		if p := param(rr); p != tc.param {
			t.Errorf("%s: param %q, want %q", tc.name, p, tc.param)
		}
	}
	if n := planCount(); n != 0 {
		t.Fatalf("refused creates wrote %d plans", n)
	}

	rr := do(http.MethodPost, "/", body("p-ok", `"base_amount_cents":4900,"status":"active"`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status active: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	var plan domain.Plan
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.Status != domain.PlanActive || plan.BaseAmountCents != 4900 {
		t.Fatalf("created plan status=%q base=%d, want active 4900", plan.Status, plan.BaseAmountCents)
	}

	for _, status := range []string{"Archived", "inactive"} {
		rr = do(http.MethodPatch, "/"+plan.ID, `{"status":"`+status+`"}`)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("PATCH status %q: status %d, want 422; body %s", status, rr.Code, rr.Body.String())
		}
		if p := param(rr); p != "status" {
			t.Errorf("PATCH status %q: param %q, want status", status, p)
		}
	}
	got, err := svc.GetPlan(ctx, tenantID, plan.ID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if got.Status != domain.PlanActive || got.Name != plan.Name {
		t.Fatalf("plan after refused PATCHes: status=%q name=%q, want active %q", got.Status, got.Name, plan.Name)
	}

	// Control: a valid status still archives.
	rr = do(http.MethodPatch, "/"+plan.ID, `{"status":"archived"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status archived: status %d, want 200; body %s", rr.Code, rr.Body.String())
	}
}

// TestRatingRuleAndOverrideCreate_RefuseUnknownField: the rate lives on the
// rating rule and the price override, and validation accepts 0 as a free
// rate. So POST /v1/rating-rules with `flat_amount` (not flat_amount_cents),
// or a tier with `unit_amount`, and POST /v1/price-overrides with
// `unit_amount_decimal`, each made a $0 price with a 201. The unknown key is
// now a 422 naming it and nothing is written.
func TestRatingRuleAndOverrideCreate_RefuseUnknownField(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Rate Strict Create")

	svc := pricing.NewService(pricing.NewPostgresStore(db))
	h := pricing.NewHandler(svc)
	rules, overrides := h.RatingRuleRoutes(), h.OverrideRoutes()
	do := func(srv http.Handler, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req.WithContext(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)))
		return rr
	}
	param := func(rr *httptest.ResponseRecorder) string {
		var env struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		return env.Error.Param
	}
	ruleCount := func() int {
		list, err := svc.ListRatingRules(ctx, pricing.RatingRuleFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("list rating rules: %v", err)
		}
		return len(list)
	}

	refusals := []struct {
		name  string
		srv   http.Handler
		body  string
		param string
	}{
		{"rule flat_amount", rules, `{"rule_key":"r_flat","name":"r","mode":"flat","currency":"USD","flat_amount":2}`, "flat_amount"},
		{"rule tier unit_amount", rules, `{"rule_key":"r_tier","name":"r","mode":"graduated","currency":"USD","graduated_tiers":[{"up_to":0,"unit_amount":"5"}]}`, "unit_amount"},
		{"override unit_amount_decimal", overrides, `{"customer_id":"vlx_cus_x","rating_rule_version_id":"vlx_rrv_x","mode":"flat","unit_amount_decimal":"1.5"}`, "unit_amount_decimal"},
	}
	for _, tc := range refusals {
		rr := do(tc.srv, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422; body %s", tc.name, rr.Code, rr.Body.String())
		}
		if p := param(rr); p != tc.param {
			t.Errorf("%s: param %q, want %q", tc.name, p, tc.param)
		}
	}
	if n := ruleCount(); n != 0 {
		t.Fatalf("refused creates wrote %d rating rules", n)
	}
	overrideList, err := svc.ListOverrides(ctx, tenantID, "")
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	if len(overrideList) != 0 {
		t.Fatalf("refused create wrote %d overrides", len(overrideList))
	}

	// Control: the known body is created at the price sent.
	rr := do(rules, `{"rule_key":"r_ok","name":"r","mode":"flat","currency":"USD","flat_amount_cents":"2"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("known body: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	var rule domain.RatingRuleVersion
	if err := json.Unmarshal(rr.Body.Bytes(), &rule); err != nil {
		t.Fatalf("decode rule: %v", err)
	}
	if rule.FlatAmountCents.String() != "2" {
		t.Fatalf("created rule flat_amount_cents=%s, want 2", rule.FlatAmountCents)
	}
}
