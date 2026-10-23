package billing_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/integrations/litellm"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/recipe"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// recipeTenant is one tenant with the anthropic_style recipe installed and
// one customer subscribed to its plan over April 2026, an elapsed period.
type recipeTenant struct {
	t            *testing.T
	db           *postgres.DB
	ctx, authCtx context.Context
	tenantID     string
	customerID   string
	subID        string
	meterID      string
	pricingSvc   *pricing.Service
	usageSvc     *usage.Service
	usageStore   *usage.PostgresStore
	invoiceStore *invoice.PostgresStore
	handler      *litellm.Handler
	periodStart  time.Time
	periodEnd    time.Time
}

func newRecipeTenant(t *testing.T, db *postgres.DB, name string) *recipeTenant {
	t.Helper()
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling, leader.RoleDunning)
	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	usageStore := usage.NewPostgresStore(db)
	usageSvc := usage.NewService(usageStore)
	tenantID := testutil.CreateTestTenant(t, db, name)

	registry, err := recipe.Load()
	if err != nil {
		t.Fatalf("load recipes: %v", err)
	}
	recipeSvc := recipe.NewService(db, recipe.NewPostgresStore(db), registry,
		pricingSvc, dunning.NewService(dunning.NewPostgresStore(db), nil, nil),
		webhook.NewService(webhook.NewPostgresStore(db), nil, "local"))
	if _, err := recipeSvc.Instantiate(ctx, tenantID, "anthropic_style", nil, recipe.InstantiateOptions{}); err != nil {
		t.Fatalf("instantiate anthropic_style: %v", err)
	}
	plans, err := pricingStore.ListPlans(ctx, tenantID)
	if err != nil {
		t.Fatalf("list plans: %v", err)
	}
	var plan domain.Plan
	for _, p := range plans {
		if p.Code == "ai_api_pro" {
			plan = p
		}
	}
	if plan.ID == "" || len(plan.MeterIDs) != 1 {
		t.Fatalf("recipe plan not found or not on one meter: %+v", plan)
	}
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_fam", DisplayName: "Families"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-fam", DisplayName: "Families", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)

	return &recipeTenant{
		t: t, db: db, ctx: ctx, authCtx: auth.WithTenantID(ctx, tenantID),
		tenantID: tenantID, customerID: cust.ID, subID: sub.ID, meterID: plan.MeterIDs[0],
		pricingSvc: pricingSvc, usageSvc: usageSvc, usageStore: usageStore,
		invoiceStore: invoice.NewPostgresStore(db),
		handler:      litellm.New(customerStore, pricingSvc, usageSvc, usageSvc),
		periodStart:  periodStart, periodEnd: periodEnd,
	}
}

// spend posts one LiteLLM 1.83.9-shaped record (top-level counts, end_user,
// the usage object under metadata) through the real handler, mid-period.
func (rt *recipeTenant) spend(id, model string, prompt, cached, output int64) string {
	rt.t.Helper()
	body := fmt.Sprintf(`{
		"id":%q,"call_type":"acompletion","status":"success","model":%q,"custom_llm_provider":"anthropic",
		"end_user":"cus_fam","prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,
		"metadata":{"usage_object":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,
			"cache_read_input_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}},
		"endTime":%d}`,
		id, model, prompt, output, prompt+output, prompt, output, prompt+output, cached, cached,
		rt.periodStart.Add(15*24*time.Hour).Unix())
	req := httptest.NewRequest(http.MethodPost, "/spend", strings.NewReader(body)).WithContext(rt.authCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.handler.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		rt.t.Fatalf("/spend %s: status %d, body %s", id, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// priceRule makes a new rating rule and a pricing rule on the tokens meter.
// The pricing rule is seeded past the meter-in-use guard: these tests check
// which rule claims an event, and the tenant's subscription already exists.
// A model_raw rule at 101 outranks the model rules, so the guard would refuse
// it on a live meter (pricing/meter_edit_guard.go).
func (rt *recipeTenant) priceRule(key, cents string, match map[string]any, priority int) {
	rt.t.Helper()
	rr, err := rt.pricingSvc.CreateRatingRule(rt.ctx, rt.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: key, Name: key, Mode: domain.PricingFlat, Currency: "USD",
		FlatAmountCents: decimal.RequireFromString(cents),
	})
	if err != nil {
		rt.t.Fatalf("create rating rule %s: %v", key, err)
	}
	seedPricingRule(rt.t, rt.db, rt.ctx, rt.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: rt.meterID, RatingRuleVersionID: rr.ID, DimensionMatch: match, Priority: priority,
	})
}

// storeOldRow writes one event as the release before this change stored it.
func (rt *recipeTenant) storeOldRow(key string, qty int64, dims map[string]any) {
	rt.t.Helper()
	ts := rt.periodStart.Add(10 * 24 * time.Hour)
	if _, err := rt.usageSvc.Ingest(rt.ctx, rt.tenantID, usage.IngestInput{
		CustomerID: rt.customerID, MeterID: rt.meterID, Quantity: decimal.NewFromInt(qty),
		Dimensions: dims, IdempotencyKey: key, Timestamp: &ts,
	}); err != nil {
		rt.t.Fatalf("store old row %s: %v", key, err)
	}
}

// close runs the cycle and returns the period's invoice lines by description.
func (rt *recipeTenant) close() (int64, map[string]int64) {
	rt.t.Helper()
	engine := newRealEngine(rt.db, clock.NewFake(rt.periodEnd.Add(time.Nanosecond)))
	if _, errs := engine.RunCycle(rt.ctx, 50, nil); len(errs) > 0 {
		rt.t.Fatalf("RunCycle: %v", errs)
	}
	invs, _, err := rt.invoiceStore.List(rt.ctx, invoice.ListFilter{TenantID: rt.tenantID})
	if err != nil {
		rt.t.Fatalf("list invoices: %v", err)
	}
	for _, inv := range invs {
		if inv.SubscriptionID != rt.subID {
			continue
		}
		items, err := rt.invoiceStore.ListLineItems(rt.ctx, rt.tenantID, inv.ID)
		if err != nil {
			rt.t.Fatalf("list lines: %v", err)
		}
		lines := map[string]int64{}
		for _, it := range items {
			lines[it.Description] += it.AmountCents
		}
		return inv.SubtotalCents, lines
	}
	rt.t.Fatal("no invoice for the subscription")
	return 0, nil
}

func dumpLines(lines map[string]int64) string {
	keys := make([]string, 0, len(lines))
	for k := range lines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "\n  %s = %d¢", k, lines[k])
	}
	return b.String()
}

// TestLiteLLM_ModelFamiliesE2E: spend logs for the new Claude models and the
// cloud-platform name forms go through the real handler, the cycle closes,
// and every line bills at the anthropic_style 6.0.0 list price (Anthropic's
// pricing page, checked 2026-10-22). Quantities are chosen so each line is
// whole cents.
func TestLiteLLM_ModelFamiliesE2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rt := newRecipeTenant(t, db, "Model Families Corp")

	// A call stored by the previous release, which priced Opus 5.5 as
	// Opus 5, then sent again: it dedups to the stored row and keeps
	// billing as stored (one row, one bill).
	rt.storeOldRow("fam_old_opus55:input", 1_000_000, map[string]any{
		"model": "claude-opus-5", "model_raw": "claude-opus-5-5", "provider": "anthropic", "token_type": "input",
	})
	if got := rt.spend("fam_old_opus55", "claude-opus-5-5", 1_000_000, 0, 0); !strings.Contains(got, `"deduplicated":1`) || !strings.Contains(got, `"accepted":0`) {
		t.Fatalf("resend of a stored call: %s, want accepted 0, deduplicated 1", got)
	}

	rt.spend("fam_opus55", "claude-opus-5-5", 1_000_000, 0, 100_000)
	rt.spend("fam_sonnet55", "anthropic/claude-sonnet-5-5", 2_000_000, 1_000_000, 100_000)
	rt.spend("fam_sonnet5", "claude-sonnet-5", 1_000_000, 0, 100_000)
	rt.spend("fam_fable51", "bedrock/global.anthropic.claude-fable-5-1", 2_000_000, 1_000_000, 100_000)
	rt.spend("fam_haiku_std", "claude-haiku-5-5", 100_000, 0, 100_000)
	rt.spend("fam_haiku_long", "us.anthropic.claude-haiku-5-5", 1_000_000, 800_000, 100_000)
	rt.spend("fam_vertex_opus46", "vertex_ai/claude-opus-4-6@default", 1_000_000, 0, 100_000)
	rt.spend("fam_converse_sonnet46", "bedrock/converse/us.anthropic.claude-sonnet-4-6", 1_000_000, 0, 100_000)

	// Decision 2: an unknown version keeps its raw name and has no price...
	rt.spend("fam_unknown", "claude-opus-4-9", 1_000_000, 0, 0)
	evs, _, err := rt.usageStore.List(rt.ctx, usage.ListFilter{
		TenantID: rt.tenantID, Dimensions: map[string]any{"model_raw": "claude-opus-4-9"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) != 1 || evs[0].Dimensions["model"] != "claude-opus-4-9" {
		t.Errorf("unknown model's events: %+v, want one with model = the raw name", evs)
	}
	// ...until a price is added before the invoice is made.
	rt.priceRule("opus49_input_by_hand", "0.0001", map[string]any{"model_raw": "claude-opus-4-9", "token_type": "input"}, 101)

	subtotal, lines := rt.close()
	want := map[string]int64{
		"Tokens (claude-opus-5 · input)":                            500, // the stored old row
		"Tokens (claude-opus-5.5 · input)":                          400,
		"Tokens (claude-opus-5.5 · output)":                         200,
		"Tokens (claude-sonnet-5.5 · input)":                        200,
		"Tokens (claude-sonnet-5.5 · cache_read)":                   10,
		"Tokens (claude-sonnet-5.5 · output)":                       100,
		"Tokens (claude-sonnet-5 · input)":                          200,
		"Tokens (claude-sonnet-5 · output)":                         100,
		"Tokens (claude-fable-5.1 · input)":                         1000,
		"Tokens (claude-fable-5.1 · cache_read)":                    25,
		"Tokens (claude-fable-5.1 · output)":                        500,
		"Tokens (prompt_up_to_100k · claude-haiku-5.5 · input)":     1,
		"Tokens (prompt_up_to_100k · claude-haiku-5.5 · output)":    5,
		"Tokens (prompt_over_100k · claude-haiku-5.5 · input)":      10,
		"Tokens (prompt_over_100k · claude-haiku-5.5 · cache_read)": 4,
		"Tokens (prompt_over_100k · claude-haiku-5.5 · output)":     25,
		"Tokens (claude-opus-4.6 · input)":                          500,
		"Tokens (claude-opus-4.6 · output)":                         250,
		"Tokens (claude-sonnet-4.6 · input)":                        300,
		"Tokens (claude-sonnet-4.6 · output)":                       150,
		"Tokens (claude-opus-4-9 · input)":                          100,
	}
	var wantTotal int64
	for desc, cents := range want {
		wantTotal += cents
		if lines[desc] != cents {
			t.Errorf("line %q = %d¢, want %d¢", desc, lines[desc], cents)
		}
	}
	if subtotal != wantTotal {
		t.Errorf("subtotal %d¢, want %d¢. Lines:%s", subtotal, wantTotal, dumpLines(lines))
	}
}

// TestLiteLLM_StoredRowRepricedByModelRaw pins the docs' remedy for usage
// stored before the upgrade (review F1): a Vertex Opus 4.6 call stored as
// claude-opus-4 bills at Opus 4 prices unless a pricing rule on model_raw
// claims it, and that rule must outrank the recipe's (priority 101). At
// equal priority the older rule, the recipe's, wins.
func TestLiteLLM_StoredRowRepricedByModelRaw(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, tc := range []struct {
		priority  int
		wantCents int64
		wantDesc  string
	}{
		{101, 500, "Tokens (vertex_ai/claude-opus-4-6@default · input)"},
		{100, 1500, "Tokens (claude-opus-4 · input)"},
	} {
		t.Run(fmt.Sprintf("priority_%d", tc.priority), func(t *testing.T) {
			rt := newRecipeTenant(t, db, fmt.Sprintf("Stored Row Corp %d", tc.priority))
			rt.storeOldRow("old_vertex:input", 1_000_000, map[string]any{
				"model": "claude-opus-4", "model_raw": "vertex_ai/claude-opus-4-6@default",
				"provider": "vertex_ai", "token_type": "input",
			})
			rt.priceRule("opus46_input_stored_vertex", "0.0005", map[string]any{
				"model_raw": "vertex_ai/claude-opus-4-6@default", "token_type": "input",
			}, tc.priority)
			subtotal, lines := rt.close()
			if subtotal != tc.wantCents || lines[tc.wantDesc] != tc.wantCents {
				t.Errorf("subtotal %d¢, want %d¢ on %q. Lines:%s", subtotal, tc.wantCents, tc.wantDesc, dumpLines(lines))
			}
		})
	}
}

// TestLiteLLM_RawRowStoredBeforeUpgradeBilledByModelRule pins the docs' remedy
// for usage stored under a raw name that now maps to a model (second review,
// finding 3): such a row, here a Google Cloud dated Haiku 4.5 name, has no
// price after the upgrade, while a new call with the same name bills at the
// recipe price. A rule on model with the raw name bills the old row, and
// only it: a new call carries the model's name in model.
func TestLiteLLM_RawRowStoredBeforeUpgradeBilledByModelRule(t *testing.T) {
	db := testutil.SetupTestDB(t)
	const raw = "vertex_ai/claude-haiku-4-5@20251001"
	for _, tc := range []struct {
		name     string
		addRule  bool
		wantSub  int64
		wantRows map[string]int64
	}{
		{"no rule", false, 100, map[string]int64{"Tokens (claude-haiku-4.5 · input)": 100}},
		{"rule on model", true, 200, map[string]int64{
			"Tokens (claude-haiku-4.5 · input)": 100,
			"Tokens (" + raw + " · input)":      100,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRecipeTenant(t, db, "Raw Row Corp "+tc.name)
			// The previous release kept this name raw on model: no family matched.
			rt.storeOldRow("old_raw_haiku45:input", 1_000_000, map[string]any{
				"model": raw, "model_raw": raw, "provider": "vertex_ai", "token_type": "input",
			})
			rt.spend("new_raw_haiku45", raw, 1_000_000, 0, 0)
			if tc.addRule {
				rt.priceRule("haiku45_input_stored_raw", "0.0001", map[string]any{"model": raw, "token_type": "input"}, 100)
			}
			subtotal, lines := rt.close()
			if subtotal != tc.wantSub {
				t.Errorf("subtotal %d¢, want %d¢. Lines:%s", subtotal, tc.wantSub, dumpLines(lines))
			}
			for desc, cents := range tc.wantRows {
				if lines[desc] != cents {
					t.Errorf("line %q = %d¢, want %d¢. Lines:%s", desc, lines[desc], cents, dumpLines(lines))
				}
			}
		})
	}
}

// TestLiteLLM_ProviderCostsAfterUpgrade pins what the docs say about provider
// costs (second review, findings 1 and 2). A cost saved on an older model's
// name does not cover the model's new name: Opus 5.5 events arrive as
// claude-opus-5.5 with no cost, which leaves the period's margin unmeasured
// until a cost is saved for the new name, and only for events after that.
// And provider costs have no context_tier: one Haiku 5.5 cost is stamped on
// both prompt tiers.
func TestLiteLLM_ProviderCostsAfterUpgrade(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rt := newRecipeTenant(t, db, "Provider Cost Corp")
	saveCost := func(model, tokenType, perToken string) {
		t.Helper()
		if _, err := rt.usageStore.UpsertProviderCostRate(rt.ctx, rt.tenantID, domain.ProviderCostRate{
			Provider: "anthropic", Model: model, TokenType: tokenType,
			CostPerToken: decimal.RequireFromString(perToken), Currency: "USD",
		}); err != nil {
			t.Fatalf("save cost %s %s: %v", model, tokenType, err)
		}
	}
	events := func(modelRaw string) []domain.UsageEvent {
		t.Helper()
		evs, _, err := rt.usageStore.List(rt.ctx, usage.ListFilter{
			TenantID: rt.tenantID, Dimensions: map[string]any{"model_raw": modelRaw}, Limit: 20,
		})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		return evs
	}
	unresolved := func() int64 {
		t.Helper()
		_, n, err := rt.usageStore.CustomerProviderCost(rt.ctx, rt.tenantID, rt.customerID, rt.periodStart, rt.periodEnd)
		if err != nil {
			t.Fatalf("customer provider cost: %v", err)
		}
		return n
	}

	// Costs saved before the upgrade, on the name Opus 5.5 used to arrive as.
	saveCost("claude-opus-5", "input", "0.000004")
	saveCost("claude-opus-5", "output", "0.00002")
	rt.spend("pc_opus55_before", "claude-opus-5-5", 1_000_000, 0, 100_000)
	for _, e := range events("claude-opus-5-5") {
		if e.ProviderCostMicros != nil || e.ProviderCostSource != "" {
			t.Errorf("%s: cost %v source %q, want none: a cost on claude-opus-5 does not cover claude-opus-5.5", e.IdempotencyKey, e.ProviderCostMicros, e.ProviderCostSource)
		}
	}
	if n := unresolved(); n != 2 {
		t.Errorf("unresolved events = %d, want 2 (the call's input and output)", n)
	}

	// A cost saved for the new name covers later calls only.
	saveCost("claude-opus-5.5", "input", "0.000004")
	saveCost("claude-opus-5.5", "output", "0.00002")
	rt.spend("pc_opus55_after", "anthropic/claude-opus-5-5", 1_000_000, 0, 100_000)
	for _, e := range events("anthropic/claude-opus-5-5") {
		if e.ProviderCostSource != "table" || e.ProviderCostMicros == nil {
			t.Errorf("%s: cost %v source %q, want a table cost", e.IdempotencyKey, e.ProviderCostMicros, e.ProviderCostSource)
		}
	}
	if n := unresolved(); n != 2 {
		t.Errorf("unresolved events = %d, want still 2: a cost applies only to events that arrive after it", n)
	}

	// One Haiku 5.5 cost, the standard tier's, is stamped on a long prompt
	// too, which bills at the higher price.
	saveCost("claude-haiku-5.5", "input", "0.0000001")
	rt.spend("pc_haiku_long", "claude-haiku-5-5", 200_000, 0, 0)
	for _, e := range events("claude-haiku-5-5") {
		if e.Dimensions["token_type"] != "input" {
			continue
		}
		if e.Dimensions[litellm.DimContextTier] != litellm.ContextTierOver100k {
			t.Fatalf("%s: %s = %v, want %s", e.IdempotencyKey, litellm.DimContextTier, e.Dimensions[litellm.DimContextTier], litellm.ContextTierOver100k)
		}
		// 200,000 tokens x $0.10 per million = $0.02 = 20,000 micros.
		if e.ProviderCostMicros == nil || *e.ProviderCostMicros != 20_000 {
			t.Errorf("%s: cost %v micros, want 20000: provider costs have no context_tier, so the one Haiku 5.5 cost covers both tiers", e.IdempotencyKey, e.ProviderCostMicros)
		}
	}
}
