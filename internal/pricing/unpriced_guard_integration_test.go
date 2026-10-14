package pricing_test

// #981 guard, on real Postgres: removing a meter's last price (its last
// pricing rule, or its default binding) is refused while a plan wiring the
// meter has a live subscription. Each refusal has a twin that must pass, so
// the guard cannot be "refuse everything".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

type unpricedGuardFixture struct {
	db       *postgres.DB
	svc      *pricing.Service
	tenantID string
	ctx      context.Context
}

func newUnpricedGuardFixture(t *testing.T) unpricedGuardFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	svc := pricing.NewService(pricing.NewPostgresStore(db))
	svc.SetSubscriptionPlanUsageReader(subscription.NewPostgresStore(db))
	return unpricedGuardFixture{
		db:       db,
		svc:      svc,
		tenantID: testutil.CreateTestTenant(t, db, "unpriced guard"),
		ctx:      postgres.WithLivemode(context.Background(), false),
	}
}

func (f unpricedGuardFixture) rule(t *testing.T, key string) domain.RatingRuleVersion {
	t.Helper()
	r, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: key, Name: key, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rule %s: %v", key, err)
	}
	return r
}

// meterWithRules creates a meter (default binding = defaultRuleID, may be
// empty) and one pricing rule per entry in models, each matching
// {model: <entry>}.
func (f unpricedGuardFixture) meterWithRules(t *testing.T, key, defaultRuleID string, models ...string) (domain.Meter, []domain.MeterPricingRule) {
	t.Helper()
	m, err := f.svc.CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
		Key: key, Name: key, Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: defaultRuleID,
	})
	if err != nil {
		t.Fatalf("create meter %s: %v", key, err)
	}
	var rules []domain.MeterPricingRule
	for _, model := range models {
		rr := f.rule(t, key+"_"+model)
		mpr, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: rr.ID,
			DimensionMatch: map[string]any{"model": model},
		})
		if err != nil {
			t.Fatalf("bind rule %s: %v", model, err)
		}
		rules = append(rules, mpr)
	}
	return m, rules
}

func (f unpricedGuardFixture) plan(t *testing.T, code string, meterIDs ...string) domain.Plan {
	t.Helper()
	p, err := f.svc.CreatePlan(f.ctx, f.tenantID, pricing.CreatePlanInput{
		Code: code, Name: code, Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: meterIDs,
	})
	if err != nil {
		t.Fatalf("create plan %s: %v", code, err)
	}
	return p
}

// sub inserts a customer and a subscription on planID with the given status.
func (f unpricedGuardFixture) sub(t *testing.T, planID, status string) {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	now := time.Now().UTC()
	custID := postgres.NewID("vlx_cus")
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $3, $4, $4)
	`, custID, f.tenantID, "ext_"+custID, now); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	subID := postgres.NewID("vlx_sub")
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			created_at, updated_at
		) VALUES ($1, $2, $3, $3, $4, $5, 'anniversary', $6, $7, $7, $6, $6)
	`, subID, f.tenantID, "code_"+subID, custID, status, now, now.AddDate(0, 1, 0)); err != nil {
		t.Fatalf("insert sub: %v", err)
	}
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, postgres.NewID("vlx_si"), f.tenantID, subID, planID, now); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (f unpricedGuardFixture) ruleCount(t *testing.T, meterID string) int {
	t.Helper()
	rules, err := f.svc.ListMeterPricingRulesByMeter(f.ctx, f.tenantID, meterID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	return len(rules)
}

func strPtr(s string) *string { return &s }

func TestUnpricedGuard_DeleteLastRule(t *testing.T) {
	f := newUnpricedGuardFixture(t)

	t.Run("refused while a live subscription's plan wires the meter", func(t *testing.T) {
		m, rules := f.meterWithRules(t, "tok_live", "", "gpt-a")
		p := f.plan(t, "ai_live", m.ID)
		f.sub(t, p.ID, "active")

		err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, rules[0].ID)
		if !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("want ErrInvalidState refusal, got %v", err)
		}
		if !strings.Contains(err.Error(), "ai_live") {
			t.Errorf("refusal must name the plan, got %q", err.Error())
		}
		if n := f.ruleCount(t, m.ID); n != 1 {
			t.Errorf("rule must still exist after the refusal, have %d", n)
		}
	})

	t.Run("draft and trialing subscriptions count as live", func(t *testing.T) {
		for _, st := range []string{"draft", "trialing"} {
			m, rules := f.meterWithRules(t, "tok_"+st, "", "gpt-a")
			p := f.plan(t, "ai_"+st, m.ID)
			f.sub(t, p.ID, st)
			if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, rules[0].ID); !errors.Is(err, errs.ErrInvalidState) {
				t.Errorf("%s sub: want refusal, got %v", st, err)
			}
		}
	})

	t.Run("allowed when the only subscriptions are canceled", func(t *testing.T) {
		m, rules := f.meterWithRules(t, "tok_canceled", "", "gpt-a")
		p := f.plan(t, "ai_canceled", m.ID)
		f.sub(t, p.ID, "canceled")
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, rules[0].ID); err != nil {
			t.Fatalf("no live sub: delete must pass, got %v", err)
		}
		if n := f.ruleCount(t, m.ID); n != 0 {
			t.Errorf("rule must be gone, have %d", n)
		}
	})

	t.Run("allowed when another rule remains", func(t *testing.T) {
		m, rules := f.meterWithRules(t, "tok_two", "", "gpt-a", "gpt-b")
		p := f.plan(t, "ai_two", m.ID)
		f.sub(t, p.ID, "active")
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, rules[0].ID); err != nil {
			t.Fatalf("another rule remains: delete must pass, got %v", err)
		}
		// The second delete would leave the meter with nothing: refused.
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, rules[1].ID); !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("now the last rule: want refusal, got %v", err)
		}
	})

	t.Run("allowed when the meter has a default binding", func(t *testing.T) {
		def := f.rule(t, "tok_default_catchall")
		m, rules := f.meterWithRules(t, "tok_default", def.ID, "gpt-a")
		p := f.plan(t, "ai_default", m.ID)
		f.sub(t, p.ID, "active")
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, rules[0].ID); err != nil {
			t.Fatalf("default binding remains: delete must pass, got %v", err)
		}
	})

	t.Run("allowed when no plan wires the meter", func(t *testing.T) {
		_, rules := f.meterWithRules(t, "tok_noplan", "", "gpt-a")
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, rules[0].ID); err != nil {
			t.Fatalf("no plan: delete must pass, got %v", err)
		}
	})

	t.Run("unknown rule stays a not-found", func(t *testing.T) {
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, "vlx_mpr_nope"); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

func TestUnpricedGuard_ClearDefaultBinding(t *testing.T) {
	f := newUnpricedGuardFixture(t)

	t.Run("refused while a live subscription's plan wires the meter", func(t *testing.T) {
		def := f.rule(t, "calls_flat_live")
		m, _ := f.meterWithRules(t, "calls_live", def.ID)
		p := f.plan(t, "calls_plan_live", m.ID)
		f.sub(t, p.ID, "active")

		_, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{RatingRuleVersionID: strPtr("")})
		if !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("want ErrInvalidState refusal, got %v", err)
		}
		if !strings.Contains(err.Error(), "calls_plan_live") {
			t.Errorf("refusal must name the plan, got %q", err.Error())
		}
		got, err := f.svc.GetMeter(f.ctx, f.tenantID, m.ID)
		if err != nil {
			t.Fatalf("get meter: %v", err)
		}
		if got.RatingRuleVersionID != def.ID {
			t.Errorf("binding must survive the refusal, got %q", got.RatingRuleVersionID)
		}
	})

	t.Run("allowed when a pricing rule remains", func(t *testing.T) {
		def := f.rule(t, "calls_flat_rules")
		m, _ := f.meterWithRules(t, "calls_rules", def.ID, "gpt-a")
		p := f.plan(t, "calls_plan_rules", m.ID)
		f.sub(t, p.ID, "active")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{RatingRuleVersionID: strPtr("")}); err != nil {
			t.Fatalf("a rule remains: clear must pass, got %v", err)
		}
	})

	t.Run("allowed with no live subscription", func(t *testing.T) {
		def := f.rule(t, "calls_flat_nosub")
		m, _ := f.meterWithRules(t, "calls_nosub", def.ID)
		f.plan(t, "calls_plan_nosub", m.ID)
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{RatingRuleVersionID: strPtr("")}); err != nil {
			t.Fatalf("no live sub: clear must pass, got %v", err)
		}
	})

	t.Run("a PATCH that leaves the binding alone is not checked", func(t *testing.T) {
		def := f.rule(t, "calls_flat_rename")
		m, _ := f.meterWithRules(t, "calls_rename", def.ID)
		p := f.plan(t, "calls_plan_rename", m.ID)
		f.sub(t, p.ID, "active")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{Name: strPtr("Calls renamed")}); err != nil {
			t.Fatalf("rename must pass, got %v", err)
		}
	})
}

// TestUnpricedGuard_HTTP pins what an API caller sees: 409 with the code the
// other meter-binding guards use (invalid_state), and a message naming the
// plan.
func TestUnpricedGuard_HTTP(t *testing.T) {
	f := newUnpricedGuardFixture(t)
	h := pricing.NewHandler(f.svc)
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	r.Mount("/v1/meters/{meter_id}/pricing-rules", h.MeterPricingRuleRoutes(pass, pass))
	r.Mount("/v1/meters", h.MeterRoutes())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithTenantID(f.ctx, f.tenantID))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) (code, msg string) {
		t.Helper()
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return env.Error.Code, env.Error.Message
	}

	m, rules := f.meterWithRules(t, "tok_http", "", "gpt-a")
	p := f.plan(t, "ai_http", m.ID)
	f.sub(t, p.ID, "active")

	rec := do(http.MethodDelete, "/v1/meters/"+m.ID+"/pricing-rules/"+rules[0].ID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE last rule: status %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code, msg := decode(t, rec); code != "invalid_state" || !strings.Contains(msg, "ai_http") {
		t.Errorf("DELETE last rule: code=%q msg=%q, want invalid_state naming the plan", code, msg)
	}

	def := f.rule(t, "calls_flat_http")
	m2, _ := f.meterWithRules(t, "calls_http", def.ID)
	p2 := f.plan(t, "calls_plan_http", m2.ID)
	f.sub(t, p2.ID, "active")
	rec = do(http.MethodPatch, "/v1/meters/"+m2.ID, `{"rating_rule_version_id":""}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PATCH clear binding: status %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if code, msg := decode(t, rec); code != "invalid_state" || !strings.Contains(msg, "calls_plan_http") {
		t.Errorf("PATCH clear binding: code=%q msg=%q, want invalid_state naming the plan", code, msg)
	}
}
