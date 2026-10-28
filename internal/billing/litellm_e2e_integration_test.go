package billing_test

import (
	"context"
	"fmt"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/recipe"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// TestLiteLLM_WedgeE2E is the AI-native wedge as a CI test: a REALISTIC LiteLLM
// spend payload (dated API model string + a prompt-cache hit) → the real
// /spend handler → usage events → cycle billing → invoice priced by the
// anthropic_style recipe. It exercises the two fixes that make the wedge
// actually rate end-to-end (ADR-044):
//
//   - MODEL NORMALIZATION: the payload carries "claude-3-5-sonnet-20241022"
//     (what LiteLLM forwards); the mapper canonicalizes it to the recipe family
//     "claude-3.5-sonnet" so the {model, token_type} rules match. Without this
//     the usage rates at $0.
//   - CACHE_READ additive-disjoint split: prompt_tokens includes the cached
//     tokens; the mapper emits input = prompt − cached and cache_read = cached,
//     each priced at its own recipe rate.
//   - CACHE_WRITE 5m/1h split: a second call, shaped like a LiteLLM 1.83.9
//     record (top-level counts, end_user, the split under
//     metadata.usage_object), is the flag-2 probe call scaled by 100 so every
//     line is whole cents. Before the split it billed input 10.1M at 1.0x.
//
// This is the test that would have caught the S2/X15 wedge breakage at CI time.
func TestLiteLLM_WedgeE2E(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// livemode=false test partition; tenant bound below for the handler path.
	baseCtx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleBilling, leader.RoleDunning)

	customerStore := customer.NewPostgresStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	usageSvc := usage.NewService(usageStore)

	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM Wedge Corp")
	// Stores/engine take tenantID as a param (mirroring the other billing
	// integration tests); only the litellm HTTP handler reads tenant from ctx.
	ctx := baseCtx
	authCtx := auth.WithTenantID(baseCtx, tenantID)

	// 1. Instantiate the anthropic_style recipe → one `tokens` meter, the
	//    per-{model, token_type} rules, and the ai_api_pro plan.
	registry, err := recipe.Load()
	if err != nil {
		t.Fatalf("load recipe registry: %v", err)
	}
	recipeSvc := recipe.NewService(db, recipe.NewPostgresStore(db), registry, pricingSvc)
	if _, err := recipeSvc.Instantiate(ctx, tenantID, "anthropic_style", nil, recipe.InstantiateOptions{}); err != nil {
		t.Fatalf("instantiate anthropic_style: %v", err)
	}

	// Find the recipe's plan (default code ai_api_pro).
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
	if plan.ID == "" {
		t.Fatal("recipe did not create the ai_api_pro plan")
	}

	// 2. Customer + subscription on the recipe plan, over an ELAPSED period in
	//    the past (so RunCycle bills it and event timestamps aren't future).
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_wedge", DisplayName: "Wedge Customer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-wedge", DisplayName: "Wedge sub", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt:                 &periodStart,
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)

	// 3. POST a realistic LiteLLM spend payload through the REAL /spend handler.
	//    Quantities are scaled to whole-cent amounts at the recipe's
	//    claude-3.5-sonnet rates (input $3.00/M, cache_read $0.30/M, output
	//    $15.00/M): input 1M → 300¢, cache_read 1M → 30¢, output 1M → 1500¢.
	eventTS := periodStart.Add(15 * 24 * time.Hour).Unix() // mid-period (Apr 15)
	body := fmt.Sprintf(`{
		"id":"wedge_call_1","call_type":"completion",
		"model":"claude-3-5-sonnet-20241022","custom_llm_provider":"anthropic",
		"user":"cus_wedge",
		"usage":{"prompt_tokens":2000000,"completion_tokens":1000000,"total_tokens":3000000,
		         "prompt_tokens_details":{"cached_tokens":1000000},"cache_read_input_tokens":1000000},
		"endTime":%d
	}`, eventTS)

	handler := litellm.New(customerStore, pricingSvc, usageSvc, usageSvc)
	post := func(body string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/spend", strings.NewReader(body)).WithContext(authCtx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("/spend: status %d, body %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	// input + cache_read + output = 3 accepted events.
	if got := post(body); !strings.Contains(got, `"accepted":3`) {
		t.Fatalf("/spend body: got %s, want accepted=3 (input+cache_read+output)", got)
	}

	// The cache-write call, at claude-sonnet-4.5's recipe rates (input
	// $3.00/M, 5m write $3.75/M, 1h write $6.00/M, output $15.00/M): 100,000
	// fresh input → 30¢, 4M 5m writes → 1500¢, 6M 1h writes → 3600¢, 50,000
	// output → 75¢: 5205¢, the probe's $0.5205 times 100.
	writeBody := fmt.Sprintf(`{
		"id":"wedge_call_writes","call_type":"acompletion","status":"success","cache_hit":null,
		"model":"anthropic/claude-sonnet-4-5-20250929","custom_llm_provider":"anthropic",
		"end_user":"cus_wedge","prompt_tokens":10100000,"completion_tokens":50000,"total_tokens":10150000,
		"metadata":{"usage_object":{"prompt_tokens":10100000,"completion_tokens":50000,"total_tokens":10150000,
			"cache_creation_input_tokens":10000000,"cache_read_input_tokens":0,
			"prompt_tokens_details":{"cached_tokens":0,"cache_creation_tokens":10000000,"text_tokens":100000,
				"cache_creation_token_details":{"ephemeral_5m_input_tokens":4000000,"ephemeral_1h_input_tokens":6000000}}}},
		"endTime":%d
	}`, eventTS)
	if got := post(writeBody); !strings.Contains(got, `"accepted":4`) {
		t.Fatalf("/spend body: got %s, want accepted=4 (input+cache_write_5m+cache_write_1h+output)", got)
	}

	// A Claude Code /v1/messages call (call_type anthropic_messages), shaped
	// like the LiteLLM 1.83.9 proxy record in
	// internal/integrations/litellm/testdata/litellm-1.83.9-proxy-messages-split.json
	// and scaled by 100. Before 2026-10-22 Hardcount skipped this call type and
	// billed nothing. At claude-sonnet-4.5's rates: 100,000 input → 30¢,
	// 500,000 cache reads at $0.30/M → 15¢, 4M 5m writes → 1500¢, 6M 1h
	// writes → 3600¢, 50,000 output → 75¢: 5220¢.
	messagesBody := fmt.Sprintf(`{
		"id":"chatcmpl-wedge-messages","call_type":"anthropic_messages","status":"success","cache_hit":null,
		"model":"anthropic/claude-sonnet-4-5-20250929","custom_llm_provider":"anthropic",
		"end_user":"cus_wedge","prompt_tokens":10600000,"completion_tokens":50000,"total_tokens":10650000,
		"metadata":{"usage_object":{"prompt_tokens":10600000,"completion_tokens":50000,"total_tokens":10650000,
			"cache_creation_input_tokens":10000000,"cache_read_input_tokens":500000,
			"prompt_tokens_details":{"cached_tokens":500000,"cache_creation_tokens":10000000,"text_tokens":100000,
				"cache_creation_token_details":{"ephemeral_5m_input_tokens":4000000,"ephemeral_1h_input_tokens":6000000}}}},
		"startTime":%d.25,"endTime":%d
	}`, eventTS, eventTS)
	if got := post(messagesBody); !strings.Contains(got, `"accepted":5`) {
		t.Fatalf("/spend body: got %s, want accepted=5 (input+cache_read+cache_write_5m+cache_write_1h+output)", got)
	}

	// 3b. Verify the events landed with the canonical family + token roles.
	prices, err := pricing.NewPostgresStore(db).PlanPricesAt(ctx, tenantID, plan.ID, periodStart, "")
	if err != nil || len(prices) == 0 {
		t.Fatalf("plan prices: %v (%d)", err, len(prices))
	}
	events, err := usageStore.AggregateClaims(ctx, tenantID, cust.ID, prices[0].MeterID, domain.AggregationSum, domain.ClaimsOf(prices), periodStart, periodEnd)
	if err != nil {
		t.Fatalf("aggregate by price: %v", err)
	}
	claimed := 0
	for _, e := range events {
		if e.PriceID != "" {
			claimed++
		}
	}
	if claimed == 0 {
		t.Fatal("no price took an event — events didn't match the recipe prices (model normalization failed?)")
	}

	// 4. Run the cycle → invoice for the elapsed period.
	engine := newRealEngine(db, clock.NewFake(periodEnd.Add(time.Nanosecond)))
	if _, errs := engine.RunCycle(ctx, 50, nil); len(errs) > 0 {
		t.Fatalf("RunCycle: %v", errs)
	}

	// 5. Assert the invoice rates the usage at the recipe's per-{model,token_type}
	//    rates — proving the dated model string normalized and all three roles
	//    (input/cache_read/output) priced correctly.
	invs, _, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var inv domain.Invoice
	for _, i := range invs {
		if i.SubscriptionID == sub.ID {
			inv = i
		}
	}
	if inv.ID == "" {
		t.Fatal("no cycle invoice for the wedge sub")
	}
	// Call 1: input 300¢ + cache_read 30¢ + output 1500¢ = 1830¢. Call 2:
	// 30¢ + 1500¢ + 3600¢ + 75¢ = 5205¢ (it billed 3105¢ before the split).
	// Call 3, the /v1/messages call: 5220¢ (0¢ before 2026-10-22). Base $0,
	// no tax.
	const wantCents = 1830 + 5205 + 5220
	if inv.SubtotalCents != wantCents {
		items, _ := invoiceStore.ListLineItems(ctx, tenantID, inv.ID)
		t.Errorf("invoice subtotal: got %d¢, want %d¢ (1830 for the cache-read call + 5205 for the cache-write call + 5220 for the /v1/messages call). Lines: %+v", inv.SubtotalCents, wantCents, items)
	}
	t.Logf("wedge e2e: dated model ids normalized, 5 token roles rated, invoice $%.2f", float64(inv.SubtotalCents)/100)
}
