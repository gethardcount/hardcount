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

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/recipe"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
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
	planID       string
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
	recipeSvc := recipe.NewService(db, recipe.NewPostgresStore(db), registry, pricingSvc)
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
	planMeters, err := pricingStore.PlanMeterIDs(ctx, tenantID, plan.ID)
	if plan.ID == "" || err != nil || len(planMeters) != 1 {
		t.Fatalf("recipe plan not found or not on one meter: %+v (meters %v, %v)", plan, planMeters, err)
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
		tenantID: tenantID, customerID: cust.ID, subID: sub.ID, planID: plan.ID, meterID: planMeters[0],
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

// priceRule adds a price for match to the recipe plan's tokens meter, in
// force from before the period opened: these tests check which price takes
// an event, and the tenant's subscription already exists (on a used plan an
// added price takes part from the next period).
func (rt *recipeTenant) priceRule(cents string, match map[string]string) {
	rt.t.Helper()
	p := addPrice(rt.t, rt.db, rt.ctx, rt.tenantID, rt.planID, priceOn{meterID: rt.meterID, match: match, shape: perUnitShape(cents)})
	backdatePriceVersions(rt.t, rt.db, rt.ctx, p.ID, rt.periodStart.Add(-time.Hour))
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
	rt.priceRule("0.0001", map[string]string{"model": "claude-opus-4-9", "token_type": "input"})

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

// TestLiteLLM_ModelRawPriceNeedsOneMoreLabel: a call on a platform name,
// here Google Cloud's Opus 4.6, arrives with the model's name in model and
// the platform's name in model_raw, and bills at the recipe's
// {model, token_type} price unless a price that also names model_raw takes
// it. Priority is gone (ADR-130 §4), so such a price needs one more label
// than the price it refines; a {model_raw, token_type} price beside it ties
// and is refused when saved, naming the other price, and the call keeps
// billing at the recipe's price.
func TestLiteLLM_ModelRawPriceNeedsOneMoreLabel(t *testing.T) {
	db := testutil.SetupTestDB(t)
	const raw = "vertex_ai/claude-opus-4-6@default"
	t.Run("one more label wins", func(t *testing.T) {
		rt := newRecipeTenant(t, db, "Model Raw Corp more")
		rt.spend("raw_vertex_more", raw, 1_000_000, 0, 0)
		rt.priceRule("0.0007", map[string]string{"model": "claude-opus-4.6", "model_raw": raw, "token_type": "input"})
		subtotal, lines := rt.close()
		desc := "Tokens (claude-opus-4.6 · " + raw + " · input)"
		if subtotal != 700 || lines[desc] != 700 {
			t.Errorf("subtotal %d¢, want 700¢ on %q. Lines:%s", subtotal, desc, dumpLines(lines))
		}
	})
	t.Run("equal labels refused", func(t *testing.T) {
		rt := newRecipeTenant(t, db, "Model Raw Corp equal")
		rt.spend("raw_vertex_equal", raw, 1_000_000, 0, 0)
		_, err := pricing.NewPostgresStore(db).WritePrice(rt.ctx, rt.tenantID, rt.planID, pricing.PriceChange{
			Op: pricing.PriceAdd, MeterID: rt.meterID, Match: map[string]string{"model_raw": raw, "token_type": "input"}, Shape: perUnitShape("0.0007"),
		}, nil)
		if errs.Code(err) != errs.CodePriceConflict || !strings.Contains(err.Error(), "hc_prc_") {
			t.Fatalf("a {model_raw, token_type} price beside {model, token_type}: got %v, want price_conflict naming the other price", err)
		}
		subtotal, lines := rt.close()
		if subtotal != 500 || lines["Tokens (claude-opus-4.6 · input)"] != 500 {
			t.Errorf("subtotal %d¢, want 500¢ at the recipe's Opus 4.6 price. Lines:%s", subtotal, dumpLines(lines))
		}
	})
}

// TestLiteLLM_DatedNameBillsAtItsModelsPrice: a call on a Google Cloud dated
// name of Haiku 4.5 bills at the recipe's Haiku 4.5 price, because the call
// carries the model's name in model. So a price on model with the raw name
// takes nothing: to price a platform name apart, name it in model_raw
// (TestLiteLLM_ModelRawPriceNeedsOneMoreLabel).
func TestLiteLLM_DatedNameBillsAtItsModelsPrice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	const raw = "vertex_ai/claude-haiku-4-5@20251001"
	for _, tc := range []struct {
		name     string
		addPrice bool
	}{
		{"recipe prices only", false},
		{"with a price on model = the raw name", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRecipeTenant(t, db, "Dated Name Corp "+tc.name)
			rt.spend("dated_haiku45", raw, 1_000_000, 0, 0)
			if tc.addPrice {
				rt.priceRule("0.0001", map[string]string{"model": raw, "token_type": "input"})
			}
			subtotal, lines := rt.close()
			if subtotal != 100 || len(lines) != 1 || lines["Tokens (claude-haiku-4.5 · input)"] != 100 {
				t.Errorf("subtotal %d¢, want 100¢ on one line at the recipe's Haiku 4.5 price. Lines:%s", subtotal, dumpLines(lines))
			}
		})
	}
}

// TestLiteLLM_ProviderCostIsPerModelName pins what the docs say about
// provider costs. A cost names one model: a cost saved for an older model,
// claude-opus-5, does not cover Opus 5.5 calls, which arrive as
// claude-opus-5.5, so those calls have no cost and the period's margin is
// not measured for them. A cost saved for claude-opus-5.5 covers the calls
// that arrive after it, never earlier ones. And provider costs have no
// context_tier: one Haiku 5.5 cost is stamped on both prompt tiers.
func TestLiteLLM_ProviderCostIsPerModelName(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rt := newRecipeTenant(t, db, "Provider Cost Corp")
	saveCost := func(model, tokenType, centsPerToken string) {
		t.Helper()
		if _, err := rt.usageStore.UpsertProviderCostRate(rt.ctx, rt.tenantID, domain.ProviderCostRate{
			Provider: "anthropic", Model: model, TokenType: tokenType,
			UnitCostCents: decimal.RequireFromString(centsPerToken), Currency: "USD",
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

	// Costs saved for the older model only.
	saveCost("claude-opus-5", "input", "0.0004")
	saveCost("claude-opus-5", "output", "0.002")
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
	saveCost("claude-opus-5.5", "input", "0.0004")
	saveCost("claude-opus-5.5", "output", "0.002")
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
	saveCost("claude-haiku-5.5", "input", "0.00001")
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

// TestLiteLLM_RawNamesOnAFreshInstall: the anthropic_style recipe prices
// Claude by the family the LiteLLM mapper puts in `model`, so a fresh install
// bills platform names (a Foundry azure_ai/ name, a Google Cloud @date name, a
// Bedrock cross-region name) at their model's list price, and a name the
// mapper does not know keeps itself and has no price (ADR-130 §15, design
// §4.22). One raw name is priced apart by a price with one more label than
// the recipe's price it refines: `model_raw` beside the recipe's labels. For
// Haiku 5.5, whose recipe prices carry context_tier, that is four labels; a
// three-label price there ties the recipe's and is refused.
func TestLiteLLM_RawNamesOnAFreshInstall(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rt := newRecipeTenant(t, db, "Raw Names Corp")
	const vertexOpus46 = "vertex_ai/claude-opus-4-6@default"
	const bedrockHaiku55 = "us.anthropic.claude-haiku-5-5"

	rt.spend("raw_azure_opus45", "azure_ai/claude-opus-4-5", 1_000_000, 0, 0)
	rt.spend("raw_vertex_sonnet45", "vertex_ai/claude-sonnet-4-5@20250929", 1_000_000, 0, 0)
	rt.spend("raw_bedrock_haiku45", "bedrock/eu.anthropic.claude-haiku-4-5-20251001-v1:0", 1_000_000, 0, 0)
	rt.spend("raw_unknown", "claude-opus-5-9", 1_000_000, 0, 0)
	rt.spend("raw_vertex_opus46", vertexOpus46, 1_000_000, 0, 0)
	rt.spend("raw_direct_opus46", "claude-opus-4-6", 1_000_000, 0, 0)
	rt.spend("raw_bedrock_haiku55", bedrockHaiku55, 100_000, 0, 0)
	rt.spend("raw_direct_haiku55", "claude-haiku-5-5", 100_000, 0, 0)

	rt.priceRule("0.0006", map[string]string{"model": "claude-opus-4.6", "model_raw": vertexOpus46, "token_type": "input"})
	rt.priceRule("0.00002", map[string]string{
		"model": "claude-haiku-5.5", "model_raw": bedrockHaiku55, "context_tier": "prompt_up_to_100k", "token_type": "input",
	})
	_, err := pricing.NewPostgresStore(db).WritePrice(rt.ctx, rt.tenantID, rt.planID, pricing.PriceChange{
		Op: pricing.PriceAdd, MeterID: rt.meterID,
		Match: map[string]string{"model": "claude-haiku-5.5", "model_raw": bedrockHaiku55, "token_type": "input"},
		Shape: perUnitShape("0.00002"),
	}, nil)
	if errs.Code(err) != errs.CodePriceConflict || !strings.Contains(err.Error(), "hc_prc_") {
		t.Errorf("a three-label Haiku 5.5 model_raw price: got %v, want price_conflict naming the recipe's price", err)
	}

	subtotal, lines := rt.close()
	want := map[string]int64{
		"Tokens (claude-opus-4.5 · input)":                                               500,
		"Tokens (claude-sonnet-4.5 · input)":                                             300,
		"Tokens (claude-haiku-4.5 · input)":                                              100,
		"Tokens (claude-opus-4.6 · " + vertexOpus46 + " · input)":                        600,
		"Tokens (claude-opus-4.6 · input)":                                               500,
		"Tokens (prompt_up_to_100k · claude-haiku-5.5 · " + bedrockHaiku55 + " · input)": 2,
		"Tokens (prompt_up_to_100k · claude-haiku-5.5 · input)":                          1,
	}
	var wantTotal int64
	for desc, cents := range want {
		wantTotal += cents
		if lines[desc] != cents {
			t.Errorf("line %q = %d¢, want %d¢", desc, lines[desc], cents)
		}
	}
	if subtotal != wantTotal || len(lines) != len(want) {
		t.Errorf("subtotal %d¢ over %d lines, want %d¢ over %d. Lines:%s", subtotal, len(lines), wantTotal, len(want), dumpLines(lines))
	}
	evs, _, err := rt.usageStore.List(rt.ctx, usage.ListFilter{
		TenantID: rt.tenantID, Dimensions: map[string]any{"model_raw": "claude-opus-5-9"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) != 1 || evs[0].Dimensions["model"] != "claude-opus-5-9" {
		t.Errorf("unknown model's events: %+v, want one with model = the raw name", evs)
	}
}
