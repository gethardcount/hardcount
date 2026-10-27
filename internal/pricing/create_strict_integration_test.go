package pricing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

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

	rr := do(http.MethodPost, "/", body("p-ok", `"base_amount_cents":4900,"base_bill_timing":"in_arrears","status":"active"`))
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

	// draft is not a plan status: create makes every plan active and nothing
	// read draft differently from active, so it was removed.
	for _, status := range []string{"Archived", "inactive", "draft"} {
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

// TestPriceAndOverrideCreate_RefuseUnknownField: the rate lives on the plan
// price and the price override, and validation accepts 0 as a free rate. So
// a misspelt rate key (`flat_amount`, a tier's `unit_amount`,
// `unit_amount_decimal`) would make a $0 price with a 201. The unknown key is
// a 422 naming it and nothing is written.
func TestPriceAndOverrideCreate_RefuseUnknownField(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Rate Strict Create")

	store := pricing.NewPostgresStore(db)
	svc := pricing.NewService(store)
	h := pricing.NewHandler(svc)
	plans, overrides := h.PlanRoutes(), h.OverrideRoutes()
	meter, err := svc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{EventName: "calls", Name: "Calls"})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	plan, prices, err := svc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "strict", Name: "Strict", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Prices: []pricing.PriceInput{{MeterID: meter.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	do := func(srv http.Handler, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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
	pricePath := "/" + plan.ID + "/prices"

	refusals := []struct {
		name, path, body, param string
		srv                     http.Handler
	}{
		{"price flat_amount", pricePath, `{"meter_id":"` + meter.ID + `","dimension_match":{"m":"a"},"mode":"per_unit","flat_amount":2}`, "flat_amount", plans},
		{"price tier unit_amount", pricePath, `{"meter_id":"` + meter.ID + `","dimension_match":{"m":"b"},"mode":"graduated","tiers":[{"up_to":null,"unit_amount":"5"}]}`, "unit_amount", plans},
		{"override unit_amount_decimal", "/", `{"customer_id":"vlx_cus_x","price_id":"` + prices[0].ID + `","mode":"per_unit","unit_amount_decimal":"1.5"}`, "unit_amount_decimal", overrides},
	}
	for _, tc := range refusals {
		rr := do(tc.srv, tc.path, tc.body)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d, want 422; body %s", tc.name, rr.Code, rr.Body.String())
		}
		if p := param(rr); p != tc.param {
			t.Errorf("%s: param %q, want %q", tc.name, p, tc.param)
		}
	}
	if listed, err := svc.ListPlanPrices(ctx, tenantID, plan.ID); err != nil || len(listed) != 1 {
		t.Fatalf("refused creates left %d prices (%v), want the 1 the plan was made with", len(listed), err)
	}
	overrideList, err := svc.ListOverrides(ctx, tenantID, "", "")
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	if len(overrideList) != 0 {
		t.Fatalf("refused create wrote %d overrides", len(overrideList))
	}

	// Control: the known body is created at the price sent.
	rr := do(plans, pricePath, `{"meter_id":"`+meter.ID+`","dimension_match":{"m":"a"},"mode":"per_unit","unit_amount_cents":"2"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("known body: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	var price domain.PlanPrice
	if err := json.Unmarshal(rr.Body.Bytes(), &price); err != nil {
		t.Fatalf("decode price: %v", err)
	}
	if price.UnitAmountCents.String() != "2" {
		t.Fatalf("created price unit_amount_cents=%s, want 2", price.UnitAmountCents)
	}
}
