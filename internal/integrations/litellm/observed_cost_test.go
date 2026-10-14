package litellm

import (
	"math"
	"testing"
)

// payloadWithBreakdown builds a completion with an input and an output half
// (and a cache-read half when cacheRead > 0, with no cache_read_cost).
func payloadWithBreakdown(in, out float64, cacheRead int64) StandardLoggingPayload {
	p := StandardLoggingPayload{
		ID: "call_1", User: "cus_x", Model: "claude-3.5-sonnet",
		CallType: "completion",
		Usage: &Usage{
			PromptTokens: 1000, CompletionTokens: 200,
			PromptTokensDetails: &PromptTokensDetails{CachedTokens: cacheRead},
		},
		CostBreakdown: &CostBreakdown{
			InputCost: in, OutputCost: out, TotalCost: in + out,
		},
	}
	return p
}

func costOf(t *testing.T, evs []ExternalIngest, half string) *int64 {
	t.Helper()
	for _, e := range evs {
		if e.Dimensions["token_type"] == half {
			return e.ObservedCostMicros
		}
	}
	return nil
}

// TestObservedCost_PerHalfOnly is the money assertion behind ADR-079 D4.
// One LiteLLM call fans out to up to three usage events. Stamping a
// whole-call figure on each would multi-count COGS by up to 3x, which is
// precisely why phase 1 stamped nothing. Each half must carry only its own
// side's cost. The cached case is TestObservedCost_CachedCallCountsEachTokenOnce.
func TestObservedCost_PerHalfOnly(t *testing.T) {
	t.Parallel()
	evs, err := MapPayload(payloadWithBreakdown(0.012, 0.045, 0))
	if err != nil {
		t.Fatalf("map: %v", err)
	}

	in := costOf(t, evs, TokenTypeInput)
	if in == nil || *in != 12000 {
		t.Errorf("input half: got %v, want 12000 micros ($0.012)", in)
	}
	out := costOf(t, evs, TokenTypeOutput)
	if out == nil || *out != 45000 {
		t.Errorf("output half: got %v, want 45000 micros ($0.045)", out)
	}
	// And the total across halves must equal the call's total, not a multiple.
	var sum int64
	for _, e := range evs {
		if e.ObservedCostMicros != nil {
			sum += *e.ObservedCostMicros
		}
	}
	if sum != 57000 {
		t.Errorf("summed observed cost = %d micros, want 57000 ($0.057) — "+
			"a larger number means a whole-call figure leaked onto a half", sum)
	}
}

// TestObservedCost_ResponseCostNeverStamped: ResponseCost is a WHOLE-CALL
// figure. It is the obvious fallback when CostBreakdown is absent, and D4
// forbids it for exactly that reason.
func TestObservedCost_ResponseCostNeverStamped(t *testing.T) {
	t.Parallel()
	p := payloadWithBreakdown(0, 0, 0)
	p.CostBreakdown = nil
	p.ResponseCost = 0.057

	evs, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	for _, e := range evs {
		if e.ObservedCostMicros != nil {
			t.Errorf("%s: got %d, want nil — ResponseCost is whole-call and must "+
				"fall to rate-table inference, never be stamped per half",
				e.Dimensions["token_type"], *e.ObservedCostMicros)
		}
	}
}

// TestObservedCost_RejectsNonFinite: a NaN cast to int64 is garbage that
// would silently corrupt margin. Falling back to the table is the honest
// answer, so these must produce nil rather than a clamped number.
func TestObservedCost_RejectsNonFinite(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]float64{
		"NaN":      math.NaN(),
		"+Inf":     math.Inf(1),
		"-Inf":     math.Inf(-1),
		"negative": -0.01,
	} {
		t.Run(name, func(t *testing.T) {
			evs, err := MapPayload(payloadWithBreakdown(v, 0.045, 0))
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if in := costOf(t, evs, TokenTypeInput); in != nil {
				t.Errorf("input half with %s cost: got %d, want nil", name, *in)
			}
			// The healthy half is unaffected — one bad number must not
			// discard the cost we do trust.
			if out := costOf(t, evs, TokenTypeOutput); out == nil || *out != 45000 {
				t.Errorf("output half: got %v, want 45000", out)
			}
		})
	}
}

// TestObservedCost_ZeroIsARealAnswer: a free or fully-credited call costs
// zero. That must be recorded as observed 0, not dropped — otherwise the
// margin card cannot tell "cost was zero" from "we don't know the cost".
func TestObservedCost_ZeroIsARealAnswer(t *testing.T) {
	t.Parallel()
	evs, err := MapPayload(payloadWithBreakdown(0, 0, 0))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	in := costOf(t, evs, TokenTypeInput)
	if in == nil {
		t.Fatal("input half: got nil, want an explicit 0 — zero cost is a fact, not an absence")
	}
	if *in != 0 {
		t.Errorf("input half: got %d, want 0", *in)
	}
}

// TestObservedCost_RoundsHalfUp matches the rate-table path's ROUND() in
// internal/usage/postgres.go, so the two cost sources agree at the boundary
// instead of differing by a micro.
func TestObservedCost_RoundsHalfUp(t *testing.T) {
	t.Parallel()
	// 0.0000155 USD = 15.5 micros -> 16
	evs, err := MapPayload(payloadWithBreakdown(0.0000155, 0, 0))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if in := costOf(t, evs, TokenTypeInput); in == nil || *in != 16 {
		t.Errorf("got %v, want 16 micros (15.5 rounds half UP)", in)
	}
}

// TestObservedCost_AbsentBreakdownFallsToTable pins the no-regression case:
// every pre-existing ingest path sends no CostBreakdown and must keep
// getting rate-table inference.
func TestObservedCost_AbsentBreakdownFallsToTable(t *testing.T) {
	t.Parallel()
	p := payloadWithBreakdown(0, 0, 0)
	p.CostBreakdown = nil

	evs, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("expected events")
	}
	for _, e := range evs {
		if e.ObservedCostMicros != nil {
			t.Errorf("%s: got %d, want nil so the rate table stays in charge",
				e.Dimensions["token_type"], *e.ObservedCostMicros)
		}
	}
}

// cachedAnthropicCall is what LiteLLM 1.83.9 reports for one Anthropic call
// that read 5,000 tokens from the prompt cache and wrote 2,000 to it, with
// 1,000 fresh input tokens and 100 output tokens. The numbers were produced
// by LiteLLM's own cost code (completion_cost with a mocked Anthropic
// response, model claude-sonnet-4-5-20250929, captured from the
// standard_logging_object a callback receives):
//
//   - prompt_tokens is 8,000: LiteLLM adds both cache reads and cache writes
//     to it (llms/anthropic/chat/transformation.py).
//   - input_cost is 0.012 = 1,000 × $3/M + 5,000 × $0.30/M + 2,000 × $3.75/M.
//     LiteLLM's logging spec: cache_read_cost and cache_creation_cost are
//     "reported as a subset of input_cost".
func cachedAnthropicCall() StandardLoggingPayload {
	cacheRead := 0.0015
	return StandardLoggingPayload{
		ID: "chatcmpl-cache", EndUser: "cus_x", Model: "anthropic/claude-sonnet-4-5-20250929",
		CustomLLMProvider: "anthropic", CallType: "acompletion",
		PromptTokens: 8000, CompletionTokens: 100, TotalTokens: 8100,
		Metadata: map[string]any{"usage_object": map[string]any{
			"prompt_tokens": 8000, "completion_tokens": 100, "total_tokens": 8100,
			"prompt_tokens_details":       map[string]any{"cached_tokens": 5000, "cache_creation_tokens": 2000},
			"cache_read_input_tokens":     5000,
			"cache_creation_input_tokens": 2000,
		}},
		ResponseCost: 0.0135,
		CostBreakdown: &CostBreakdown{
			InputCost: 0.012, OutputCost: 0.0015, TotalCost: 0.0135,
			CacheReadCost: &cacheRead,
		},
	}
}

// TestObservedCost_CachedCallCountsEachTokenOnce is the margin bug the
// pre-launch check found. input_cost already holds the cache-read cost, and
// the cache_read event was costed again from the rate table, so cache reads
// were counted twice. Each event must carry the cost of its own tokens:
// input (1,000 fresh + 2,000 cache-write tokens) = input_cost − cache_read_cost,
// cache_read = cache_read_cost, and the three add up to LiteLLM's total.
// Mutation-verify: give the input event the whole input_cost again → red.
func TestObservedCost_CachedCallCountsEachTokenOnce(t *testing.T) {
	t.Parallel()
	evs, err := MapPayload(cachedAnthropicCall())
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	want := map[string]struct{ qty, micros int64 }{
		TokenTypeInput:     {3000, 10500},
		TokenTypeCacheRead: {5000, 1500},
		TokenTypeOutput:    {100, 1500},
	}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d", len(evs), len(want))
	}
	var sum int64
	for _, e := range evs {
		tt, _ := e.Dimensions["token_type"].(string)
		w := want[tt]
		if e.Quantity.IntPart() != w.qty {
			t.Errorf("%s quantity = %s, want %d", tt, e.Quantity, w.qty)
		}
		if e.ObservedCostMicros == nil {
			t.Errorf("%s cost = nil, want %d micros", tt, w.micros)
			continue
		}
		if *e.ObservedCostMicros != w.micros {
			t.Errorf("%s cost = %d, want %d micros", tt, *e.ObservedCostMicros, w.micros)
		}
		sum += *e.ObservedCostMicros
	}
	if sum != 13500 {
		t.Errorf("summed cost = %d micros, want 13500 (LiteLLM's total_cost $0.0135)", sum)
	}
}

// TestObservedCost_CacheReadWithoutItsCostFallsToTable: LiteLLM leaves
// cache_read_cost out when it cannot price cache reads (it reads only the
// Anthropic-style cache_read_input_tokens field, so an OpenAI call with
// prompt_tokens_details.cached_tokens has none) — yet input_cost still holds
// the cached tokens' cost. The input cost cannot be split, so both input
// halves fall to the rate table rather than one of them counting the
// cache reads twice. The output half is unaffected.
func TestObservedCost_CacheReadWithoutItsCostFallsToTable(t *testing.T) {
	t.Parallel()
	p := cachedAnthropicCall()
	p.CostBreakdown.CacheReadCost = nil
	evs, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if c := costOf(t, evs, TokenTypeInput); c != nil {
		t.Errorf("input cost = %d, want nil (input_cost includes cache reads it cannot separate)", *c)
	}
	if c := costOf(t, evs, TokenTypeCacheRead); c != nil {
		t.Errorf("cache_read cost = %d, want nil", *c)
	}
	if c := costOf(t, evs, TokenTypeOutput); c == nil || *c != 1500 {
		t.Errorf("output cost = %v, want 1500", c)
	}
}
