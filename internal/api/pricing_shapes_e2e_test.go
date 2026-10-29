package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The pricing-shapes suite (ADR-130; design §4.17). Each test is one way a
// business prices with Hardcount, driven over HTTP against real Postgres, and
// checks the invoices it produces line by line, to the cent. CI runs this
// file, with shape 3 (TestE2E_PlanPrices_TwoPlansPriceOneMeterDifferently
// and TestE2E_PlanPrices_SwapPricesEachWindowByItsPlan) and the shape
// goldens of internal/billing, as the pricing-shapes job.
//
// The rig is ppRig (plan_prices_e2e_test.go): a subscription starts now,
// and the period closes run later in simulated wall time.

// shapePlan creates a plan with a base fee and its prices, and returns its id
// and its prices' ids in order. timing is "" for a plan with no base fee.
func (r *ppRig) shapePlan(code, interval string, baseCents int64, timing string, prices ...map[string]any) (string, []string) {
	r.t.Helper()
	if prices == nil {
		prices = []map[string]any{}
	}
	body := map[string]any{
		"code": code, "name": code, "currency": "USD", "billing_interval": interval,
		"base_amount_cents": baseCents, "prices": prices,
	}
	if timing != "" {
		body["base_bill_timing"] = timing
	}
	out := r.must(time.Time{}, http.MethodPost, "/v1/plans", body, http.StatusCreated)
	var ids []string
	for _, p := range flowList(out, "prices") {
		ids = append(ids, flowStr(p, "id"))
	}
	if len(ids) != len(prices) {
		r.t.Fatalf("plan %s: %d prices back, want %d: %v", code, len(ids), len(prices), out)
	}
	return flowStr(out, "id"), ids
}

// subscribeQty is subscribe with a quantity on the item.
func (r *ppRig) subscribeQty(at time.Time, customerID, planID string, quantity int) (string, time.Time, time.Time) {
	r.t.Helper()
	r.seq++
	out := r.must(at, http.MethodPost, "/v1/subscriptions", map[string]any{
		"code": fmt.Sprintf("shape-sub-%d", r.seq), "display_name": "Shape", "customer_id": customerID, "billing_time": "anniversary",
		"items": []map[string]any{{"plan_id": planID, "quantity": quantity}},
	}, http.StatusCreated)
	return flowStr(out, "id"), ppTime(r.t, flowStr(out, "current_billing_period_start")), ppTime(r.t, flowStr(out, "current_billing_period_end"))
}

// scanThresholds runs the spend-threshold scan at `at` and returns the
// number of invoices it fired.
func (r *ppRig) scanThresholds(at time.Time) int {
	r.t.Helper()
	r.clk.Set(at)
	// The rig's key is a test-mode key, so its subscriptions are test mode.
	ctx := postgres.WithLivemode(clock.WithEffectiveNow(context.Background(), at), false)
	n, errs := r.srv.BillingEngine.ScanThresholds(ctx, 50)
	if len(errs) != 0 {
		r.t.Fatalf("threshold scan at %v: %v", at, errs)
	}
	return n
}

// money is an invoice's totals, read outside tenant scope.
type shapeMoney struct {
	subtotal, total, credits, due int64
}

func (r *ppRig) invoiceMoney(invoiceID string) shapeMoney {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var m shapeMoney
	if err := tx.QueryRowContext(ctx, `SELECT subtotal_cents, total_amount_cents, credits_applied_cents, amount_due_cents
		FROM invoices WHERE id = $1`, invoiceID).Scan(&m.subtotal, &m.total, &m.credits, &m.due); err != nil {
		r.t.Fatalf("invoice %s: %v", invoiceID, err)
	}
	return m
}

// shapeLines is an invoice's lines as text, one per line, in invoice order:
// the line type, its description, a usage line's quantity, and the amount.
// The ids of the prices are replaced by the names the test gave them.
func shapeLines(inv ppInvoice, names map[string]string) string {
	var b strings.Builder
	for _, l := range inv.lines {
		price := ""
		if l.priceID != "" {
			price = " price=" + names[l.priceID]
			if names[l.priceID] == "" {
				price = " price=" + l.priceID
			}
		}
		qty := ""
		if l.typ == "usage" {
			qty = " qty=" + l.qty.String()
		}
		fmt.Fprintf(&b, "%s %q%s amount=%d%s\n", l.typ, l.desc, qty, l.amount, price)
	}
	return b.String()
}

// wantLines checks an invoice's lines against their text.
func wantLines(t *testing.T, what string, inv ppInvoice, names map[string]string, want string) {
	t.Helper()
	got := shapeLines(inv, names)
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		t.Errorf("%s: lines =\n%s\nwant\n%s", what, got, strings.TrimSpace(want))
	}
}

// byReason returns the subscription's invoices with the billing reason, in
// period order.
func (r *ppRig) byReason(subID, reason string) []ppInvoice {
	r.t.Helper()
	var out []ppInvoice
	for _, i := range r.invoices(subID) {
		if i.reason == reason {
			out = append(out, i)
		}
	}
	return out
}

func names(pairs ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m
}

// TestPricingShapes_1_OpenAIStyleTokens: per_unit prices by {model,
// token_type}, fed by a LiteLLM proxy. One completion of 1,000,000 prompt
// tokens, 200,000 of them read from the prompt cache, and 100,000 completion
// tokens arrives as three events: 800,000 input, 200,000 cache_read and
// 100,000 output tokens. At $2.50, $1.25 and $10.00 per million:
//
//	input       800,000 × 0.00025c  = 200c
//	cache_read  200,000 × 0.000125c =  25c
//	output      100,000 × 0.001c    = 100c
//
// A second model with no price is not billed.
func TestPricingShapes_1_OpenAIStyleTokens(t *testing.T) {
	r := newPPRig(t, "shape 1")
	tokens := r.meter("tokens", "sum")
	plan, ids := r.plan("gpt",
		perUnit(tokens, "0.00025", map[string]string{"model": "gpt-4o", "token_type": "input"}),
		perUnit(tokens, "0.000125", map[string]string{"model": "gpt-4o", "token_type": "cache_read"}),
		perUnit(tokens, "0.001", map[string]string{"model": "gpt-4o", "token_type": "output"}))
	cust := r.customer("shape-1", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	at := recent(p0)

	call := map[string]any{
		"id": "chatcmpl-shape-1", "call_type": "completion", "model": "gpt-4o", "custom_llm_provider": "openai",
		"user": "shape-1", "endTime": at.Unix(),
		"usage": map[string]any{
			"prompt_tokens": 1000000, "completion_tokens": 100000, "total_tokens": 1100000,
			"prompt_tokens_details": map[string]any{"cached_tokens": 200000},
		},
	}
	out := r.must(at.Add(time.Second), http.MethodPost, "/v1/integrations/litellm/spend", call, http.StatusOK)
	if n := flowInt(out, "accepted"); n != 3 {
		t.Fatalf("LiteLLM answer %v: accepted %d, want 3 (input, cache_read, output)", out, n)
	}
	other := map[string]any{
		"id": "chatcmpl-shape-1b", "call_type": "completion", "model": "gpt-4o-mini", "custom_llm_provider": "openai",
		"user": "shape-1", "endTime": at.Unix(),
		"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 10, "total_tokens": 1010},
	}
	out = r.must(at.Add(time.Second), http.MethodPost, "/v1/integrations/litellm/spend", other, http.StatusOK)
	if n := flowInt(out, "no_price"); n != 2 {
		t.Errorf("unpriced model answer %v: no_price %d, want 2", out, n)
	}

	r.run(p1.Add(time.Hour))
	wantLines(t, "the period's invoice", r.cycle(sub, p0), names(ids[0], "input", ids[1], "cache_read", ids[2], "output"), `
usage "tokens (gpt-4o · input)" qty=800000 amount=200 price=input
usage "tokens (gpt-4o · cache_read)" qty=200000 amount=25 price=cache_read
usage "tokens (gpt-4o · output)" qty=100000 amount=100 price=output`)
}

// TestPricingShapes_2_AnthropicStyleMostSpecificWins: the anthropic_style
// recipe's shape. Claude Haiku 5.5 is priced by three labels {model,
// context_tier, token_type}, beside two-label {model, token_type} prices for
// an older model, and a {} price takes every event no other price takes. An
// event goes to the matching price with the most labels:
//
//	haiku  short prompt input  1,000 × 1c = 1,000c  (3 labels)
//	haiku  long prompt input   1,000 × 5c = 5,000c  (3 labels)
//	sonnet input                 100 × 3c =   300c  (2 labels)
//	sonnet output                100 × 15c = 1,500c (2 labels)
//	haiku  input, no tier label   10 ┐
//	unknown model                 10 ┴ 20 × 2c = 40c  ({} fallback)
func TestPricingShapes_2_AnthropicStyleMostSpecificWins(t *testing.T) {
	r := newPPRig(t, "shape 2")
	tokens := r.meter("tokens", "sum")
	plan, ids := r.plan("claude",
		perUnit(tokens, "1", map[string]string{"model": "claude-haiku-5.5", "context_tier": "prompt_up_to_100k", "token_type": "input"}),
		perUnit(tokens, "5", map[string]string{"model": "claude-haiku-5.5", "context_tier": "prompt_over_100k", "token_type": "input"}),
		perUnit(tokens, "3", map[string]string{"model": "claude-sonnet-4.5", "token_type": "input"}),
		perUnit(tokens, "15", map[string]string{"model": "claude-sonnet-4.5", "token_type": "output"}),
		perUnit(tokens, "2", nil))
	cust := r.customer("shape-2", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	at := recent(p0)
	r.ingest(time.Time{}, "shape-2", "tokens", 1000, at, map[string]string{"model": "claude-haiku-5.5", "context_tier": "prompt_up_to_100k", "token_type": "input"})
	r.ingest(time.Time{}, "shape-2", "tokens", 1000, at, map[string]string{"model": "claude-haiku-5.5", "context_tier": "prompt_over_100k", "token_type": "input"})
	r.ingest(time.Time{}, "shape-2", "tokens", 100, at, map[string]string{"model": "claude-sonnet-4.5", "token_type": "input"})
	r.ingest(time.Time{}, "shape-2", "tokens", 100, at, map[string]string{"model": "claude-sonnet-4.5", "token_type": "output"})
	r.ingest(time.Time{}, "shape-2", "tokens", 10, at, map[string]string{"model": "claude-haiku-5.5", "token_type": "input"})
	r.ingest(time.Time{}, "shape-2", "tokens", 10, at, map[string]string{"model": "claude-next", "token_type": "input"})

	r.run(p1.Add(time.Hour))
	wantLines(t, "the period's invoice", r.cycle(sub, p0),
		names(ids[0], "haiku_short", ids[1], "haiku_long", ids[2], "sonnet_in", ids[3], "sonnet_out", ids[4], "fallback"), `
usage "tokens (prompt_up_to_100k · claude-haiku-5.5 · input)" qty=1000 amount=1000 price=haiku_short
usage "tokens (prompt_over_100k · claude-haiku-5.5 · input)" qty=1000 amount=5000 price=haiku_long
usage "tokens (claude-sonnet-4.5 · input)" qty=100 amount=300 price=sonnet_in
usage "tokens (claude-sonnet-4.5 · output)" qty=100 amount=1500 price=sonnet_out
usage "tokens (units)" qty=20 amount=40 price=fallback`)
}

// TestPricingShapes_4_BasePlusOverage: a $20 base fee billed in advance, and
// usage on a graduated price whose first 1,000 calls are free and whose last
// tier is open (up_to: null) at 2c a call. The subscription's first invoice
// prepays the first period. 1,500 calls in that period bill 500 × 2c = 1,000c
// on the period's invoice, which also prepays the next period's base. An
// in-advance cycle invoice is headed at the period it prepays.
func TestPricingShapes_4_BasePlusOverage(t *testing.T) {
	r := newPPRig(t, "shape 4")
	calls := r.meter("calls", "sum")
	plan, ids := r.shapePlan("pro", "monthly", 2000, "in_advance", map[string]any{
		"meter_id": calls, "dimension_match": map[string]any{}, "mode": "graduated",
		"tiers": []map[string]any{{"up_to": 1000, "unit_amount_cents": "0"}, {"up_to": nil, "unit_amount_cents": "2"}},
	})
	cust := r.customer("shape-4", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)
	r.ingest(time.Time{}, "shape-4", "calls", 1500, recent(p0), nil)
	r.run(p1.Add(time.Hour))

	nm := names(ids[0], "overage")
	if invs := r.invoices(sub); len(invs) != 2 {
		t.Fatalf("%d invoices, want the create invoice and one cycle invoice: %+v", len(invs), invs)
	}
	creates := r.byReason(sub, "subscription_create")
	if len(creates) != 1 || !creates[0].start.Equal(p0) {
		t.Fatalf("create invoices = %+v, want one for the first period", creates)
	}
	wantLines(t, "the create invoice: the first period's base", creates[0], nm, `
base_fee "pro - base fee (qty 1)" amount=2000`)
	wantLines(t, "the period's invoice: its usage and the next period's base", r.cycle(sub, p1), nm, `
base_fee "pro - base fee (qty 1)" amount=2000
usage "calls (units)" qty=1500 amount=1000 price=overage`)
}

// TestPricingShapes_5_YearlyBasePlusUsage: usage on a yearly plan is billed
// once a year, at the year-end invoice (ADR-130 §13). A spend threshold
// (amount_gte) fires at most once a billing period, so once a year on a
// yearly plan: it bills a sum or count meter's usage so far, and later usage
// waits for the year end. A max or last meter's usage is never billed early.
// For monthly usage beside a yearly fee, a customer has two subscriptions: a
// yearly plan with no prices and a monthly plan with the usage prices.
//
// The yearly subscription: 300 calls in month 2 stay under a 500c
// threshold; 400 more in month 7 cross it, and the scan bills the 700 calls
// at once. 5,000 calls in month 10 are over the threshold again, but the
// scan makes no second invoice: they go on the year-end invoice with the
// base. A second yearly subscription prices a max meter: 10 seats at 100c
// are over its 500c threshold in month 2, and the scan makes no invoice; the
// year-end invoice bills them. No invoice is made in between.
func TestPricingShapes_5_YearlyBasePlusUsage(t *testing.T) {
	r := newPPRig(t, "shape 5")
	calls := r.meter("calls", "sum")

	// One yearly subscription: a 120,000c base in arrears and 1c a call.
	yearly, yIDs := r.shapePlan("yearly", "yearly", 120000, "in_arrears", perUnit(calls, "1", nil))
	cust := r.customer("shape-5", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, yearly)
	if !p1.Equal(yearAfter(p0, p0.Day())) {
		t.Fatalf("yearly period = [%v, %v), want a year", p0, p1)
	}
	r.must(time.Time{}, http.MethodPut, "/v1/subscriptions/"+sub+"/billing-thresholds", map[string]any{"amount_gte": 500}, http.StatusOK)

	// The max meter's yearly subscription: 100c a seat, the same threshold.
	seats := r.meter("seats", "max")
	seatPlan, sIDs := r.shapePlan("yearly-seats", "yearly", 0, "", perUnit(seats, "100", nil))
	custSeats := r.customer("shape-5s", "")
	seatSub, s0, s1 := r.subscribe(time.Time{}, custSeats, seatPlan)
	if !s0.Equal(p0) || !s1.Equal(p1) {
		t.Fatalf("seat period = [%v, %v), want [%v, %v)", s0, s1, p0, p1)
	}
	r.must(time.Time{}, http.MethodPut, "/v1/subscriptions/"+seatSub+"/billing-thresholds", map[string]any{"amount_gte": 500}, http.StatusOK)

	m2, m7, m10 := p0.AddDate(0, 2, 0), p0.AddDate(0, 7, 0), p0.AddDate(0, 10, 0)
	r.ingest(time.Time{}, "shape-5", "calls", 300, m2, nil)
	r.ingest(time.Time{}, "shape-5s", "seats", 10, m2, nil)
	r.run(p0.AddDate(0, 3, 0)) // a month end: nothing to invoice
	if n := r.scanThresholds(m2.Add(time.Hour)); n != 0 {
		t.Errorf("threshold at 300c calls and 1,000c seats fired %d invoices, want 0", n)
	}
	r.ingest(time.Time{}, "shape-5", "calls", 400, m7, nil)
	if n := r.scanThresholds(m7.Add(time.Hour)); n != 1 {
		t.Errorf("threshold at 700c fired %d invoices, want 1", n)
	}
	r.ingest(time.Time{}, "shape-5", "calls", 5000, m10, nil)
	if n := r.scanThresholds(m10.Add(time.Hour)); n != 0 {
		t.Errorf("a second crossing in the year fired %d invoices, want 0", n)
	}
	r.run(p1.Add(time.Hour))

	nm := names(yIDs[0], "calls")
	if invs := r.invoices(sub); len(invs) != 2 {
		t.Fatalf("%d invoices, want one threshold and one year-end invoice: %+v", len(invs), invs)
	}
	fired := r.byReason(sub, "threshold")
	if len(fired) != 1 {
		t.Fatalf("threshold invoices = %+v, want 1", fired)
	}
	wantLines(t, "the threshold invoice", fired[0], nm, `
usage "calls (units)" qty=700 amount=700 price=calls`)
	wantLines(t, "the year-end invoice: the base and the usage after the threshold", r.cycle(sub, p0), nm, `
base_fee "yearly - base fee (qty 1)" amount=120000
usage "calls (units)" qty=5000 amount=5000 price=calls`)
	if invs := r.invoices(seatSub); len(invs) != 1 {
		t.Fatalf("seat subscription: %d invoices, want only the year-end invoice: %+v", len(invs), invs)
	}
	wantLines(t, "the seats' year-end invoice", r.cycle(seatSub, p0), names(sIDs[0], "seats"), `
usage "seats (units)" qty=10 amount=1000 price=seats`)

	// Two subscriptions: a yearly fee and monthly usage.
	fee, _ := r.shapePlan("yearly-fee", "yearly", 120000, "in_advance")
	monthly, mIDs := r.shapePlan("monthly-usage", "monthly", 0, "", perUnit(calls, "1", nil))
	cust2 := r.customer("shape-5b", "")
	feeSub, f0, f1 := r.subscribe(time.Time{}, cust2, fee)
	useSub, u0, u1 := r.subscribe(time.Time{}, cust2, monthly)
	if !f1.Equal(yearAfter(f0, f0.Day())) || !u1.Equal(monthAfter(u0, u0.Day())) {
		t.Fatalf("periods: fee [%v, %v), usage [%v, %v), want a year and a month", f0, f1, u0, u1)
	}
	r.ingest(time.Time{}, "shape-5b", "calls", 250, recent(u0), nil)
	r.run(u1.Add(time.Hour))
	feeInvs := r.invoices(feeSub)
	if len(feeInvs) != 1 || feeInvs[0].reason != "subscription_create" {
		t.Fatalf("fee subscription invoices = %+v, want only the create invoice", feeInvs)
	}
	wantLines(t, "the yearly fee, billed in advance", feeInvs[0], nil, `
base_fee "yearly-fee - base fee (qty 1)" amount=120000`)
	wantLines(t, "the first month's usage", r.cycle(useSub, u0), names(mIDs[0], "calls"), `
usage "calls (units)" qty=250 amount=250 price=calls`)
}

// TestPricingShapes_6_PrepaidCommit: a customer pays 900c for 1,000c of
// credit on a one-off invoice with a commit line (ADR-078). Finalizing it
// grants the credit, and the period's usage invoice draws it down: 300 calls
// at 1c are 300c, all paid from credit, and 700c of credit is left.
func TestPricingShapes_6_PrepaidCommit(t *testing.T) {
	r := newPPRig(t, "shape 6")
	calls := r.meter("calls", "sum")
	plan, ids := r.plan("pro", perUnit(calls, "1", nil))
	cust := r.customer("shape-6", "")
	sub, p0, p1 := r.subscribe(time.Time{}, cust, plan)

	commit := r.must(time.Time{}, http.MethodPost, "/v1/invoices", map[string]any{
		"customer_id": cust,
		"line_items": []map[string]any{{
			"description": "Commit", "line_type": "add_on", "quantity": 1,
			"unit_amount_cents": 900, "commit_granted_cents": 1000,
		}},
	}, http.StatusCreated)
	r.must(time.Time{}, http.MethodPost, "/v1/invoices/"+flowStr(commit, "id")+"/finalize", nil, http.StatusOK)

	r.ingest(time.Time{}, "shape-6", "calls", 300, recent(p0), nil)
	r.run(p1.Add(time.Hour))
	inv := r.cycle(sub, p0)
	wantLines(t, "the period's invoice", inv, names(ids[0], "calls"), `
usage "calls (units)" qty=300 amount=300 price=calls`)
	if m := r.invoiceMoney(inv.id); m != (shapeMoney{subtotal: 300, total: 300, credits: 300, due: 0}) {
		t.Errorf("invoice money = %+v, want subtotal 300, total 300, credits 300, due 0", m)
	}
	bal := flowList(r.must(time.Time{}, http.MethodGet, "/v1/credits/balance/"+cust, nil, http.StatusOK), "data")
	if len(bal) != 1 || flowStr(bal[0], "currency") != "USD" || flowInt(bal[0], "balance_cents") != 700 || flowInt(bal[0], "total_granted") != 1000 {
		t.Errorf("credit balance = %v, want USD 700 left of 1000 granted", bal)
	}
}

// TestPricingShapes_7_SeatsPlusUsage: five seats of a 1,000c base fee and a
// usage price: the base line bills 5 × 1,000c, and 30 calls at 2c bill 60c
// beside it.
func TestPricingShapes_7_SeatsPlusUsage(t *testing.T) {
	r := newPPRig(t, "shape 7")
	calls := r.meter("calls", "sum")
	plan, ids := r.shapePlan("team", "monthly", 1000, "in_arrears", perUnit(calls, "2", nil))
	cust := r.customer("shape-7", "")
	sub, p0, p1 := r.subscribeQty(time.Time{}, cust, plan, 5)
	r.ingest(time.Time{}, "shape-7", "calls", 30, recent(p0), nil)
	r.run(p1.Add(time.Hour))
	wantLines(t, "the period's invoice", r.cycle(sub, p0), names(ids[0], "calls"), `
base_fee "team - base fee (qty 5)" amount=5000
usage "calls (units)" qty=30 amount=60 price=calls`)
}

// TestPricingShapes_8_GrandfatheredRateIsANewPlan: to keep early customers
// on their rate, new customers go on a new plan. Customers on the old plan
// are unchanged by it. An edit on the old plan applies from each of its
// subscribers' own next billing period: A's periods start ten days before
// B's, so for ten days A pays the new rate while B still pays the old one.
func TestPricingShapes_8_GrandfatheredRateIsANewPlan(t *testing.T) {
	r := newPPRig(t, "shape 8")
	calls := r.meter("calls", "sum")
	early, eIDs := r.plan("early", perUnit(calls, "1", nil))
	custA, custB := r.customer("shape-8a", ""), r.customer("shape-8b", "")
	subA, a0, a1 := r.subscribe(time.Now().UTC().AddDate(0, 0, -10), custA, early)
	subB, b0, b1 := r.subscribe(time.Time{}, custB, early)
	if !a1.Before(b1) {
		t.Fatalf("A's period [%v, %v) does not end before B's [%v, %v)", a0, a1, b0, b1)
	}

	// New customers get the new rate on a new plan.
	current, cIDs := r.plan("current", perUnit(calls, "3", nil))
	custC := r.customer("shape-8c", "")
	subC, c0, c1 := r.subscribe(time.Time{}, custC, current)

	r.editTo(early, eIDs[0], "2", "next_period")

	for _, ext := range []string{"shape-8a", "shape-8b", "shape-8c"} {
		r.ingest(time.Time{}, ext, "calls", 10, recent(b0), nil)
	}
	r.run(a1.Add(time.Hour))
	// After A's period ends and before B's does, both use the old plan.
	between := a1.Add(2 * time.Hour)
	r.ingest(between.Add(time.Minute), "shape-8a", "calls", 10, between, nil)
	r.ingest(between.Add(time.Minute), "shape-8b", "calls", 10, between, nil)
	r.run(b1.Add(time.Hour))
	r.run(monthAfter(a1, a0.Day()).Add(time.Hour))
	r.run(c1.Add(time.Hour))

	nm := names(eIDs[0], "early", cIDs[0], "current")
	wantLines(t, "A's period open at the edit: the old rate", r.cycle(subA, a0), nm, `
usage "calls (units)" qty=10 amount=10 price=early`)
	wantLines(t, "A's next period: the new rate", r.cycle(subA, a1), nm, `
usage "calls (units)" qty=10 amount=20 price=early`)
	wantLines(t, "B's period open at the edit, through the days A pays the new rate: the old rate", r.cycle(subB, b0), nm, `
usage "calls (units)" qty=20 amount=20 price=early`)
	wantLines(t, "C on the new plan: its rate", r.cycle(subC, c0), nm, `
usage "calls (units)" qty=10 amount=30 price=current`)
}
