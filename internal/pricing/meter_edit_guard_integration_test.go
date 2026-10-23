package pricing_test

// D05 on real Postgres: while a draft, trialing or active subscription uses a
// meter, an edit that would change the price of usage the meter already
// prices is refused with 409 meter_in_use. That is editing or deleting a
// pricing rule, adding one that would take events from a price that bills
// them now, changing or removing the default price, and changing the
// aggregation. Each refusal has a twin that must pass, so the guard cannot be
// "refuse everything".

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

const codeMeterInUse = "meter_in_use"

type meterEditFixture struct {
	db       *postgres.DB
	svc      *pricing.Service
	tenantID string
	ctx      context.Context
}

func newMeterEditFixture(t *testing.T) meterEditFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	svc := pricing.NewService(pricing.NewPostgresStore(db))
	svc.SetSubscriptionPlanUsageReader(subscription.NewPostgresStore(db))
	return meterEditFixture{
		db:       db,
		svc:      svc,
		tenantID: testutil.CreateTestTenant(t, db, "meter edit guard"),
		ctx:      postgres.WithLivemode(context.Background(), false),
	}
}

func (f meterEditFixture) rule(t *testing.T, key string, cents int64) domain.RatingRuleVersion {
	t.Helper()
	r, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: key, Name: key, Mode: domain.PricingFlat,
		Currency: "USD", FlatAmountCents: decimal.NewFromInt(cents),
	})
	if err != nil {
		t.Fatalf("create rule %s: %v", key, err)
	}
	return r
}

func (f meterEditFixture) meter(t *testing.T, key, defaultRuleID string) domain.Meter {
	t.Helper()
	m, err := f.svc.CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
		Key: key, Name: key, Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: defaultRuleID,
	})
	if err != nil {
		t.Fatalf("create meter %s: %v", key, err)
	}
	return m
}

func (f meterEditFixture) bind(t *testing.T, meterID, ruleID string, match map[string]any, priority int) domain.MeterPricingRule {
	t.Helper()
	mpr, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: meterID, RatingRuleVersionID: ruleID, DimensionMatch: match, Priority: priority,
	})
	if err != nil {
		t.Fatalf("bind rule %s: %v", ruleID, err)
	}
	return mpr
}

func (f meterEditFixture) plan(t *testing.T, code string, meterIDs ...string) domain.Plan {
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

// insertSub writes a customer, a subscription with the given status whose
// current period started at periodStart, and one item on planID, on tx.
func (f meterEditFixture) insertSub(t *testing.T, tx *sql.Tx, planID, status string, periodStart time.Time) string {
	t.Helper()
	id, err := f.insertSubErr(tx, planID, status, periodStart)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// insertSubErr is insertSub for a goroutine, which must not call t.Fatal.
func (f meterEditFixture) insertSubErr(tx *sql.Tx, planID, status string, periodStart time.Time) (string, error) {
	custID := postgres.NewID("vlx_cus")
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $3, $4, $4)
	`, custID, f.tenantID, "ext_"+custID, periodStart); err != nil {
		return "", fmt.Errorf("insert customer: %w", err)
	}
	subID := postgres.NewID("vlx_sub")
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			created_at, updated_at
		) VALUES ($1, $2, $3, $3, $4, $5, 'anniversary', $6, $7, $7, $6, $6)
	`, subID, f.tenantID, "code_"+subID, custID, status, periodStart, periodStart.AddDate(0, 1, 0)); err != nil {
		return "", fmt.Errorf("insert sub: %w", err)
	}
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, postgres.NewID("vlx_si"), f.tenantID, subID, planID, periodStart); err != nil {
		return "", fmt.Errorf("insert item: %w", err)
	}
	return subID, nil
}

// sub commits a subscription on planID with the given status, current
// period starting now.
func (f meterEditFixture) sub(t *testing.T, planID, status string) string {
	t.Helper()
	tx := f.begin(t)
	defer postgres.Rollback(tx)
	id := f.insertSub(t, tx, planID, status, time.Now().UTC())
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sub: %v", err)
	}
	return id
}

func (f meterEditFixture) begin(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

func (f meterEditFixture) rules(t *testing.T, meterID string) []domain.MeterPricingRule {
	t.Helper()
	rules, err := f.svc.ListMeterPricingRulesByMeter(f.ctx, f.tenantID, meterID)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	return rules
}

func (f meterEditFixture) getMeter(t *testing.T, id string) domain.Meter {
	t.Helper()
	m, err := f.svc.GetMeter(f.ctx, f.tenantID, id)
	if err != nil {
		t.Fatalf("get meter: %v", err)
	}
	return m
}

func ptr(s string) *string { return &s }

// wantInUse asserts err is the D05 refusal naming n subscriptions.
func wantInUse(t *testing.T, what string, err error, n string) {
	t.Helper()
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != codeMeterInUse {
		t.Fatalf("%s: want ErrInvalidState with code %s, got %v (code %q)", what, codeMeterInUse, err, errs.Code(err))
	}
	if !strings.Contains(err.Error(), n+" use") {
		t.Errorf("%s: refusal must name how many subscriptions (%q), got %q", what, n, err.Error())
	}
	if !strings.Contains(err.Error(), "new version") {
		t.Errorf("%s: refusal must point to publishing a new version, got %q", what, err.Error())
	}
}

// pricedMeter is a meter with a default price and one pricing rule
// {model: a}, on a plan.
type pricedMeter struct {
	meter domain.Meter
	def   domain.RatingRuleVersion
	other domain.RatingRuleVersion
	rule  domain.MeterPricingRule
	plan  domain.Plan
}

func (f meterEditFixture) pricedMeter(t *testing.T, key string) pricedMeter {
	t.Helper()
	def := f.rule(t, key+"_default", 1)
	other := f.rule(t, key+"_other", 2)
	ra := f.rule(t, key+"_a", 3)
	m := f.meter(t, key, def.ID)
	mpr := f.bind(t, m.ID, ra.ID, map[string]any{"model": "a"}, 100)
	return pricedMeter{meter: m, def: def, other: other, rule: mpr, plan: f.plan(t, key+"_plan", m.ID)}
}

func TestMeterEditGuard_RefusesEachEditWhileInUse(t *testing.T) {
	f := newMeterEditFixture(t)

	for _, status := range []string{"active", "trialing", "draft"} {
		t.Run(status, func(t *testing.T) {
			pm := f.pricedMeter(t, "tok_"+status)
			f.sub(t, pm.plan.ID, status)

			// Editing a rule changes which events it prices.
			_, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
				MeterID: pm.meter.ID, RatingRuleVersionID: pm.rule.RatingRuleVersionID,
				DimensionMatch: map[string]any{"model": "b"}, Priority: 100,
			})
			wantInUse(t, "edit rule", err, "1 subscription")
			if got := f.rules(t, pm.meter.ID); len(got) != 1 || got[0].DimensionMatch["model"] != "a" {
				t.Errorf("edit rule: rule must be unchanged, have %+v", got)
			}

			// A new rule takes events from the default price, which bills them now.
			_, err = f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
				MeterID: pm.meter.ID, RatingRuleVersionID: pm.other.ID,
				DimensionMatch: map[string]any{"model": "b"}, Priority: 100,
			})
			wantInUse(t, "add rule over a default price", err, "1 subscription")
			if got := f.rules(t, pm.meter.ID); len(got) != 1 {
				t.Errorf("add rule: no rule may be added, have %d", len(got))
			}

			err = f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, pm.rule.ID)
			wantInUse(t, "delete rule", err, "1 subscription")
			if got := f.rules(t, pm.meter.ID); len(got) != 1 {
				t.Errorf("delete rule: rule must still exist, have %d", len(got))
			}

			_, err = f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{RatingRuleVersionID: ptr(pm.other.ID)})
			wantInUse(t, "re-point default", err, "1 subscription")

			_, err = f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{RatingRuleVersionID: ptr("")})
			wantInUse(t, "clear default", err, "1 subscription")

			_, err = f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")})
			wantInUse(t, "change aggregation", err, "1 subscription")

			got := f.getMeter(t, pm.meter.ID)
			if got.RatingRuleVersionID != pm.def.ID || got.Aggregation != "sum" {
				t.Errorf("meter must be unchanged, got default=%q aggregation=%q", got.RatingRuleVersionID, got.Aggregation)
			}
		})
	}

	t.Run("the count covers every plan on the meter", func(t *testing.T) {
		pm := f.pricedMeter(t, "tok_count")
		second := f.plan(t, "tok_count_plan_2", pm.meter.ID)
		f.sub(t, pm.plan.ID, "active")
		f.sub(t, pm.plan.ID, "trialing")
		f.sub(t, second.ID, "active")
		err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, pm.rule.ID)
		wantInUse(t, "delete rule", err, "3 subscriptions")
	})
}

func TestMeterEditGuard_AllowsEditsThatRepriceNothing(t *testing.T) {
	f := newMeterEditFixture(t)

	t.Run("every edit passes when only canceled and archived subscriptions use the meter", func(t *testing.T) {
		pm := f.pricedMeter(t, "free_ended")
		f.sub(t, pm.plan.ID, "canceled")
		f.sub(t, pm.plan.ID, "archived")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")}); err != nil {
			t.Fatalf("aggregation: %v", err)
		}
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{RatingRuleVersionID: ptr(pm.other.ID)}); err != nil {
			t.Fatalf("re-point default: %v", err)
		}
		if _, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: pm.meter.ID, RatingRuleVersionID: pm.rule.RatingRuleVersionID,
			DimensionMatch: map[string]any{"model": "b"}, Priority: 100,
		}); err != nil {
			t.Fatalf("edit rule: %v", err)
		}
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, pm.rule.ID); err != nil {
			t.Fatalf("delete rule: %v", err)
		}
	})

	t.Run("every edit passes when no plan uses the meter", func(t *testing.T) {
		def := f.rule(t, "free_noplan_default", 1)
		ra := f.rule(t, "free_noplan_a", 3)
		m := f.meter(t, "free_noplan", def.ID)
		mpr := f.bind(t, m.ID, ra.ID, map[string]any{"model": "a"}, 100)
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{RatingRuleVersionID: ptr("")}); err != nil {
			t.Fatalf("clear default: %v", err)
		}
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, mpr.ID); err != nil {
			t.Fatalf("delete rule: %v", err)
		}
	})

	t.Run("a rename or unit change on a meter in use passes", func(t *testing.T) {
		pm := f.pricedMeter(t, "free_rename")
		f.sub(t, pm.plan.ID, "active")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{Name: ptr("Renamed"), Unit: ptr("calls")}); err != nil {
			t.Fatalf("rename: %v", err)
		}
	})

	t.Run("setting the values a meter in use already has passes", func(t *testing.T) {
		pm := f.pricedMeter(t, "free_same")
		f.sub(t, pm.plan.ID, "active")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{
			Aggregation: ptr("sum"), RatingRuleVersionID: ptr(pm.def.ID),
		}); err != nil {
			t.Fatalf("same aggregation and default: %v", err)
		}
		// Posting a rule again unchanged is the documented idempotent upsert.
		if _, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: pm.meter.ID, RatingRuleVersionID: pm.rule.RatingRuleVersionID,
			DimensionMatch: map[string]any{"model": "a"}, Priority: 100,
		}); err != nil {
			t.Fatalf("same rule again: %v", err)
		}
	})

	t.Run("pointing the default at another version of the same price passes", func(t *testing.T) {
		// Usage resolves a price by its key as of the period start, so the
		// version the meter points at does not change what it bills.
		pm := f.pricedMeter(t, "free_samekey")
		f.sub(t, pm.plan.ID, "active")
		v2, err := f.svc.CreateRatingRule(f.ctx, f.tenantID, pricing.CreateRatingRuleInput{
			RuleKey: pm.def.RuleKey, Name: pm.def.Name, Mode: domain.PricingFlat,
			Currency: "USD", FlatAmountCents: decimal.NewFromInt(9),
		})
		if err != nil {
			t.Fatalf("publish v2: %v", err)
		}
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{RatingRuleVersionID: ptr(v2.ID)}); err != nil {
			t.Fatalf("same-key re-point: %v", err)
		}
	})

	t.Run("a first default price on a meter in use passes", func(t *testing.T) {
		// It prices only usage no rule prices, which bills nothing today.
		ra := f.rule(t, "free_firstdef_a", 3)
		def := f.rule(t, "free_firstdef_default", 1)
		m := f.meter(t, "free_firstdef", "")
		f.bind(t, m.ID, ra.ID, map[string]any{"model": "a"}, 100)
		p := f.plan(t, "free_firstdef_plan", m.ID)
		f.sub(t, p.ID, "active")
		if _, err := f.svc.UpdateMeter(f.ctx, f.tenantID, m.ID, pricing.UpdateMeterInput{RatingRuleVersionID: ptr(def.ID)}); err != nil {
			t.Fatalf("first default: %v", err)
		}
	})

	t.Run("a new rule that only prices unpriced usage passes", func(t *testing.T) {
		// No default price, and every existing rule that could match the
		// same event outranks the new one: the new rule takes no event any
		// price bills now. This is how a new model is priced.
		ra := f.rule(t, "free_newmodel_a", 3)
		broad := f.rule(t, "free_newmodel_broad", 4)
		rb := f.rule(t, "free_newmodel_b", 5)
		rc := f.rule(t, "free_newmodel_c", 6)
		m := f.meter(t, "free_newmodel", "")
		f.bind(t, m.ID, ra.ID, map[string]any{"model": "a", "token_type": "input"}, 100)
		f.bind(t, m.ID, broad.ID, map[string]any{"token_type": "output"}, 200)
		p := f.plan(t, "free_newmodel_plan", m.ID)
		f.sub(t, p.ID, "active")
		// Disjoint from {model: a}; under the higher-priority output rule.
		if _, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: rb.ID,
			DimensionMatch: map[string]any{"model": "b", "token_type": "input"}, Priority: 100,
		}); err != nil {
			t.Fatalf("disjoint new rule: %v", err)
		}
		if _, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: rc.ID,
			DimensionMatch: map[string]any{"model": "b", "token_type": "output"}, Priority: 150,
		}); err != nil {
			t.Fatalf("new rule under a higher-priority one: %v", err)
		}
	})

	t.Run("a new rule that outranks an overlapping rule is refused", func(t *testing.T) {
		broad := f.rule(t, "lock_outrank_broad", 4)
		fine := f.rule(t, "lock_outrank_fine", 5)
		m := f.meter(t, "lock_outrank", "")
		f.bind(t, m.ID, broad.ID, map[string]any{"token_type": "input"}, 50)
		p := f.plan(t, "lock_outrank_plan", m.ID)
		f.sub(t, p.ID, "active")
		_, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: fine.ID,
			DimensionMatch: map[string]any{"model": "b", "token_type": "input"}, Priority: 100,
		})
		wantInUse(t, "outranking new rule", err, "1 subscription")
	})
}

// The upgrade steps in docs/ai-pricing.md ("Rules you wrote on a raw Claude
// name", "Usage stored before you upgraded"), on a recipe-shaped tokens
// meter in use: no default price, every rule names a model at priority 100.
// The doc says which steps are refused after the upgrade and which still
// work; this pins each one.
func TestMeterEditGuard_AIPricingUpgradeSteps(t *testing.T) {
	f := newMeterEditFixture(t)
	opus4 := f.rule(t, "up_opus4_input", 15)
	raw := f.rule(t, "up_raw_sonnet46_input", 3)
	m := f.meter(t, "up_tokens", "")
	f.bind(t, m.ID, opus4.ID, map[string]any{"model": "claude-opus-4", "token_type": "input"}, 100)
	rawRule := f.bind(t, m.ID, raw.ID, map[string]any{"model": "bedrock/us.anthropic.claude-sonnet-4-6", "token_type": "input"}, 100)
	p := f.plan(t, "up_tokens_plan", m.ID)
	f.sub(t, p.ID, "active")

	t.Run("a model_raw rule at 101 is refused: it outranks the model rules", func(t *testing.T) {
		opus46 := f.rule(t, "up_vertex_opus46_input", 5)
		_, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: opus46.ID,
			DimensionMatch: map[string]any{"model_raw": "vertex_ai/claude-opus-4-6@default", "token_type": "input"},
			Priority:       101,
		})
		wantInUse(t, "model_raw at 101", err, "1 subscription")
	})

	t.Run("changing a raw-name rule's dimension_match is refused", func(t *testing.T) {
		_, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: rawRule.RatingRuleVersionID,
			DimensionMatch: map[string]any{"model_raw": "bedrock/us.anthropic.claude-sonnet-4-6", "token_type": "input"},
			Priority:       101,
		})
		wantInUse(t, "raw-name rule to model_raw", err, "1 subscription")
	})

	t.Run("a new rule on a model no rule prices passes", func(t *testing.T) {
		sonnet46 := f.rule(t, "up_sonnet46_input", 3)
		if _, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: sonnet46.ID,
			DimensionMatch: map[string]any{"model": "claude-sonnet-4.6", "token_type": "input"}, Priority: 100,
		}); err != nil {
			t.Fatalf("rule on the model's name: %v", err)
		}
	})

	t.Run("a new rule on model with a raw name passes", func(t *testing.T) {
		haiku := f.rule(t, "up_raw_haiku45_input", 1)
		if _, err := f.svc.UpsertMeterPricingRule(f.ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: m.ID, RatingRuleVersionID: haiku.ID,
			DimensionMatch: map[string]any{"model": "vertex_ai/claude-haiku-4-5@20251001", "token_type": "input"}, Priority: 100,
		}); err != nil {
			t.Fatalf("rule on model with the raw name: %v", err)
		}
	})
}

// A subscription that moved off the meter's plan during its current period
// still bills the meter up to the move, so it counts. One that moved before
// the period started does not.
func TestMeterEditGuard_CountsASubscriptionThatLeftThePlanThisPeriod(t *testing.T) {
	f := newMeterEditFixture(t)

	setup := func(t *testing.T, key string, movedAt func(periodStart time.Time) time.Time) pricedMeter {
		t.Helper()
		pm := f.pricedMeter(t, key)
		other := f.plan(t, key+"_nometer")
		tx := f.begin(t)
		defer postgres.Rollback(tx)
		periodStart := time.Now().UTC().AddDate(0, 0, -10)
		subID := f.insertSub(t, tx, pm.plan.ID, "active", periodStart)
		// The swap as the subscription service writes it; the item trigger
		// records the 'plan' change at plan_changed_at.
		moved := movedAt(periodStart)
		if _, err := tx.ExecContext(f.ctx, `
			UPDATE subscription_items SET plan_id = $1, plan_changed_at = $2, updated_at = $2
			WHERE subscription_id = $3
		`, other.ID, moved, subID); err != nil {
			t.Fatalf("swap item: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return pm
	}

	t.Run("moved this period: refused", func(t *testing.T) {
		pm := setup(t, "swap_this", func(ps time.Time) time.Time { return ps.Add(48 * time.Hour) })
		err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, pm.rule.ID)
		wantInUse(t, "delete after a swap this period", err, "1 subscription")
	})

	t.Run("moved before this period: passes", func(t *testing.T) {
		pm := setup(t, "swap_before", func(ps time.Time) time.Time { return ps.Add(-48 * time.Hour) })
		if err := f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, pm.rule.ID); err != nil {
			t.Fatalf("swap before the period: delete must pass, got %v", err)
		}
	})
}

// A subscription whose insert is still uncommitted when the edit starts: the
// edit waits for it, then sees it and refuses. Without the lock the edit
// reads zero subscriptions and reprices the new subscription's period.
func TestMeterEditGuard_WaitsForASubscriptionBeingCreated(t *testing.T) {
	f := newMeterEditFixture(t)
	pm := f.pricedMeter(t, "race_sub_first")

	subTx := f.begin(t)
	defer postgres.Rollback(subTx)
	f.insertSub(t, subTx, pm.plan.ID, "active", time.Now().UTC())

	done := make(chan error, 1)
	go func() {
		_, err := f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{Aggregation: ptr("max")})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the edit must wait for the uncommitted subscription; it returned %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := subTx.Commit(); err != nil {
		t.Fatalf("commit sub: %v", err)
	}
	select {
	case err := <-done:
		wantInUse(t, "edit after the subscription commits", err, "1 subscription")
	case <-time.After(10 * time.Second):
		t.Fatal("the edit never finished")
	}
	if got := f.getMeter(t, pm.meter.ID); got.Aggregation != "sum" {
		t.Errorf("aggregation must be unchanged, got %q", got.Aggregation)
	}
}

// An edit that has passed its check and is still writing: a subscription
// created on the meter's plan meanwhile waits until the edit commits, so it
// starts on the new prices. Without the lock it commits first, and the edit
// reprices a period it never counted. The edit is held between check and
// write by a row lock on the rule it deletes.
func TestMeterEditGuard_ASubscriptionWaitsForAnEditInProgress(t *testing.T) {
	f := newMeterEditFixture(t)
	pm := f.pricedMeter(t, "race_edit_first")

	holdTx := f.begin(t)
	defer postgres.Rollback(holdTx)
	if _, err := holdTx.ExecContext(f.ctx, `SELECT id FROM meter_pricing_rules WHERE id = $1 FOR UPDATE`, pm.rule.ID); err != nil {
		t.Fatalf("hold rule row: %v", err)
	}

	editDone := make(chan error, 1)
	go func() { editDone <- f.svc.DeleteMeterPricingRule(f.ctx, f.tenantID, pm.rule.ID) }()
	time.Sleep(500 * time.Millisecond) // the edit has run its check and waits on the row

	subDone := make(chan error, 1)
	go func() {
		tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
		if err != nil {
			subDone <- err
			return
		}
		defer postgres.Rollback(tx)
		if _, err := f.insertSubErr(tx, pm.plan.ID, "active", time.Now().UTC()); err != nil {
			subDone <- err
			return
		}
		subDone <- tx.Commit()
	}()

	select {
	case err := <-subDone:
		t.Fatalf("the subscription must wait for the edit in progress; it committed first (err %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := holdTx.Rollback(); err != nil {
		t.Fatalf("release rule row: %v", err)
	}
	select {
	case err := <-editDone:
		if err != nil {
			t.Fatalf("the edit counted no subscription and must pass, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the edit never finished")
	}
	select {
	case err := <-subDone:
		if err != nil {
			t.Fatalf("subscription insert: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the subscription insert never finished")
	}
}

// A PATCH writes every meter column from its own earlier read. When another
// write changed the meter after that read, the PATCH must refuse rather than
// put the old aggregation back: a rename would otherwise revert a pricing
// change past the guard. The other write is held open on the meter row so it
// commits between the rename's read and its write.
func TestMeterEditGuard_RenameDoesNotRevertAConcurrentChange(t *testing.T) {
	f := newMeterEditFixture(t)
	pm := f.pricedMeter(t, "stale_rename")

	other := f.begin(t)
	defer postgres.Rollback(other)
	if _, err := other.ExecContext(f.ctx, `UPDATE meters SET aggregation = 'max' WHERE id = $1`, pm.meter.ID); err != nil {
		t.Fatalf("hold meter update: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := f.svc.UpdateMeter(f.ctx, f.tenantID, pm.meter.ID, pricing.UpdateMeterInput{Name: ptr("Renamed")})
		done <- err
	}()
	time.Sleep(500 * time.Millisecond) // the rename has read the meter and waits on its row
	if err := other.Commit(); err != nil {
		t.Fatalf("commit other write: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, errs.ErrInvalidState) || !strings.Contains(err.Error(), "changed while the request was running") {
			t.Fatalf("want the meter-changed refusal, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rename never finished")
	}
	if got := f.getMeter(t, pm.meter.ID); got.Aggregation != "max" || got.Name == "Renamed" {
		t.Errorf("the other write must stand and the rename must not land: aggregation=%q name=%q", got.Aggregation, got.Name)
	}
}

// TestMeterEditGuard_HTTP pins what an API caller sees: 409, code
// meter_in_use, and a message naming the count.
func TestMeterEditGuard_HTTP(t *testing.T) {
	f := newMeterEditFixture(t)
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
	check := func(t *testing.T, what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: status %d, want 409; body=%s", what, rec.Code, rec.Body.String())
		}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: decode %q: %v", what, rec.Body.String(), err)
		}
		if env.Error.Code != codeMeterInUse || !strings.Contains(env.Error.Message, "2 subscriptions use") {
			t.Errorf("%s: code=%q msg=%q, want %s naming 2 subscriptions", what, env.Error.Code, env.Error.Message, codeMeterInUse)
		}
	}

	pm := f.pricedMeter(t, "http_tok")
	f.sub(t, pm.plan.ID, "active")
	f.sub(t, pm.plan.ID, "active")

	check(t, "PATCH aggregation", do(http.MethodPatch, "/v1/meters/"+pm.meter.ID, `{"aggregation":"max"}`))
	check(t, "PATCH default", do(http.MethodPatch, "/v1/meters/"+pm.meter.ID, `{"rating_rule_version_id":"`+pm.other.ID+`"}`))
	check(t, "POST rule", do(http.MethodPost, "/v1/meters/"+pm.meter.ID+"/pricing-rules",
		`{"rating_rule_version_id":"`+pm.rule.RatingRuleVersionID+`","dimension_match":{"model":"z"},"priority":100}`))
	check(t, "DELETE rule", do(http.MethodDelete, "/v1/meters/"+pm.meter.ID+"/pricing-rules/"+pm.rule.ID, ""))
}
