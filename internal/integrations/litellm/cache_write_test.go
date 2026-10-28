package litellm

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sagarsuperuser/velox/internal/usage"
)

// The litellm-1.83.9-mock-anthropic-*.json records were built by LiteLLM
// 1.83.9's own code against a mock Anthropic endpoint (see
// testdata/capture_mock_anthropic.py): LiteLLM's transformation, cost
// calculation and logging produced them; only the upstream HTTP response is
// mocked. They are not captures from Anthropic itself.
func mockAnthropicRecord(t *testing.T, name string) StandardLoggingPayload {
	t.Helper()
	raw, err := os.ReadFile("testdata/litellm-1.83.9-mock-anthropic-" + name + ".json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var p StandardLoggingPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return p
}

// byRole indexes a call's events by token_type.
func byRole(t *testing.T, evs []ExternalIngest) map[string]ExternalIngest {
	t.Helper()
	out := map[string]ExternalIngest{}
	for _, e := range evs {
		tt, _ := e.Dimensions["token_type"].(string)
		if _, dup := out[tt]; dup {
			t.Fatalf("two events with token_type %q", tt)
		}
		out[tt] = e
	}
	return out
}

func quantities(t *testing.T, evs []ExternalIngest) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for tt, e := range byRole(t, evs) {
		out[tt] = e.Quantity.IntPart()
	}
	return out
}

func sameQuantities(got, want map[string]int64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			return false
		}
	}
	return true
}

// TestMapPayload_CacheWriteSplit is the flag-2 call: LiteLLM 1.83.9 sends
// prompt_tokens 101,000 for 1,000 fresh input tokens, 40,000 5-minute cache
// writes and 60,000 1-hour cache writes, with the split in
// metadata.usage_object.prompt_tokens_details.cache_creation_token_details.
// Each write lifetime becomes its own event, priced by the recipe at 1.25x
// and 2x input, and the input event keeps only the fresh tokens.
// Before the fix the call billed input 101,000 at 1.0x: $0.3105 for a call
// Anthropic charges $0.5205.
// Mutation-verify: drop the write subtraction from input → input 101000.
func TestMapPayload_CacheWriteSplit(t *testing.T) {
	p := mockAnthropicRecord(t, "cache-write-5m-1h")
	m, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	want := map[string]int64{TokenTypeInput: 1000, TokenTypeCacheWrite5m: 40000, TokenTypeCacheWrite1h: 60000, TokenTypeOutput: 500}
	if got := quantities(t, m.Events); !sameQuantities(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for tt, e := range byRole(t, m.Events) {
		if e.IdempotencyKey != p.ID+":"+tt {
			t.Errorf("%s key = %q, want %q", tt, e.IdempotencyKey, p.ID+":"+tt)
		}
		if e.Dimensions["model"] != "claude-sonnet-4.5" {
			t.Errorf("%s model = %v", tt, e.Dimensions["model"])
		}
	}
	// The pre-split mapping of the same call, for a replay of a call first
	// recorded before the fix: input carries the writes, no write events.
	wantLegacy := map[string]int64{TokenTypeInput: 101000, TokenTypeOutput: 500}
	if got := quantities(t, m.Legacy); !sameQuantities(got, wantLegacy) {
		t.Fatalf("legacy events = %v, want %v", got, wantLegacy)
	}
	if m.LegacyKey != p.ID+":"+TokenTypeInput {
		t.Fatalf("legacy key = %q, want the call's input key", m.LegacyKey)
	}
}

// Cache reads and cache writes split off the same prompt: every token is in
// exactly one event.
func TestMapPayload_CacheWriteSplitWithCacheRead(t *testing.T) {
	five, one := int64(1000), int64(2000)
	p := StandardLoggingPayload{
		ID: "rw1", CallType: "acompletion", EndUser: "cus_x", Model: "anthropic/claude-sonnet-4-5-20250929", CustomLLMProvider: "anthropic",
		Usage: &Usage{
			PromptTokens: 10000, CompletionTokens: 50, CacheReadInputTokens: 2000, CacheCreationInputTokens: 3000,
			PromptTokensDetails: &PromptTokensDetails{CachedTokens: 2000, CacheCreationTokens: 3000,
				CacheCreationTokenDetails: &CacheCreationTokenDetails{Ephemeral5mInputTokens: &five, Ephemeral1hInputTokens: &one}},
		},
	}
	m, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	want := map[string]int64{TokenTypeInput: 5000, TokenTypeCacheRead: 2000, TokenTypeCacheWrite5m: 1000, TokenTypeCacheWrite1h: 2000, TokenTypeOutput: 50}
	if got := quantities(t, m.Events); !sameQuantities(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	wantLegacy := map[string]int64{TokenTypeInput: 8000, TokenTypeCacheRead: 2000, TokenTypeOutput: 50}
	if got := quantities(t, m.Legacy); !sameQuantities(got, wantLegacy) {
		t.Fatalf("legacy = %v, want %v", got, wantLegacy)
	}
}

// A call whose cache writes are its whole prompt still carries an input
// event (quantity 0): that key is the claim a replay of a pre-split record
// collides with (see usage.PostgresStore.IngestCallResolvingHeld).
func TestMapPayload_CacheWriteSplitKeepsAZeroInput(t *testing.T) {
	five, one := int64(3000), int64(0)
	p := StandardLoggingPayload{
		ID: "rw0", CallType: "acompletion", EndUser: "cus_x", Model: "claude-sonnet-4-5", CustomLLMProvider: "anthropic",
		Usage: &Usage{PromptTokens: 3000, CompletionTokens: 5, CacheCreationInputTokens: 3000,
			PromptTokensDetails: &PromptTokensDetails{CacheCreationTokenDetails: &CacheCreationTokenDetails{Ephemeral5mInputTokens: &five, Ephemeral1hInputTokens: &one}}},
	}
	m, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	want := map[string]int64{TokenTypeInput: 0, TokenTypeCacheWrite5m: 3000, TokenTypeOutput: 5}
	if got := quantities(t, m.Events); !sameQuantities(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// When the split cannot be trusted the call is mapped as before the split
// existed, so no write token is billed at the wrong lifetime's price, and
// the fallback is counted where an operator can see it, not only in a log.
// Mutation-verify: drop the sum check in cacheWriteSplit → sum_mismatch
// splits; stop incrementing the counter → the counter assertions fail.
func TestMapPayload_CacheWriteFallback(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }
	cases := []struct {
		name   string
		usage  Usage
		reason string
		want   map[string]int64
	}{
		{
			name:   "no split",
			usage:  Usage{PromptTokens: 7296, CompletionTokens: 400, CacheCreationInputTokens: 7277},
			reason: UnsplitNoSplit,
			want:   map[string]int64{TokenTypeInput: 7296, TokenTypeOutput: 400},
		},
		{
			name: "split does not add up",
			usage: Usage{PromptTokens: 101000, CompletionTokens: 500, CacheCreationInputTokens: 100000,
				PromptTokensDetails: &PromptTokensDetails{CacheCreationTokens: 100000, CacheCreationTokenDetails: &CacheCreationTokenDetails{Ephemeral5mInputTokens: i64(30000), Ephemeral1hInputTokens: i64(60000)}}},
			reason: UnsplitSumMismatch,
			want:   map[string]int64{TokenTypeInput: 101000, TokenTypeOutput: 500},
		},
		{
			// The parts add up and fit in the prompt, but one is negative:
			// splitting would bill 10 more tokens at the 1-hour price than
			// were written. Mutation-verify: drop the negative-part check
			// in cacheWriteSplit → a 100,010-token 1h row.
			name: "a negative part",
			usage: Usage{PromptTokens: 101000, CompletionTokens: 500, CacheCreationInputTokens: 100000,
				PromptTokensDetails: &PromptTokensDetails{CacheCreationTokens: 100000, CacheCreationTokenDetails: &CacheCreationTokenDetails{Ephemeral5mInputTokens: i64(-10), Ephemeral1hInputTokens: i64(100010)}}},
			reason: UnsplitSumMismatch,
			want:   map[string]int64{TokenTypeInput: 101000, TokenTypeOutput: 500},
		},
		{
			name: "writes not inside prompt_tokens",
			usage: Usage{PromptTokens: 19, CompletionTokens: 400, CacheCreationInputTokens: 7277,
				PromptTokensDetails: &PromptTokensDetails{CacheCreationTokenDetails: &CacheCreationTokenDetails{Ephemeral5mInputTokens: i64(7277), Ephemeral1hInputTokens: i64(0)}}},
			reason: UnsplitNotInPrompt,
			want:   map[string]int64{TokenTypeInput: 19, TokenTypeOutput: 400},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.usage
			p := StandardLoggingPayload{ID: "fb-" + strings.ReplaceAll(tc.name, " ", "-"), CallType: "completion", User: "cus_x",
				Model: "claude-3-5-sonnet-20241022", CustomLLMProvider: "anthropic", Usage: &u}
			before := promtest.ToFloat64(cacheWriteUnsplit.WithLabelValues("anthropic", tc.reason))
			m, err := MapPayload(p)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if got := quantities(t, m.Events); !sameQuantities(got, tc.want) {
				t.Fatalf("events = %v, want %v (the pre-split mapping)", got, tc.want)
			}
			if m.Legacy != nil {
				t.Fatalf("legacy = %v, want nil: the fallback IS the old mapping", m.Legacy)
			}
			if d := promtest.ToFloat64(cacheWriteUnsplit.WithLabelValues("anthropic", tc.reason)) - before; d != 1 {
				t.Fatalf("hardcount_litellm_cache_write_unsplit_total{anthropic,%s} moved by %v, want 1", tc.reason, d)
			}
		})
	}
}

// A call with no cache writes is unchanged, and an unknown provider does not
// become a metric label.
func TestMapPayload_NoCacheWritesUnchanged(t *testing.T) {
	m, err := MapPayload(spendPayload("plain"))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if m.Legacy != nil || m.LegacyKey != "" {
		t.Fatalf("legacy mapping set for a call without cache writes: %+v", m)
	}
	if metricProvider("evil-é-provider") != "other" {
		t.Fatal("an unknown provider must fold into other")
	}
}

// A LiteLLM response-cache hit reached no provider: it carries the cached
// response's token counts and cost_breakdown's would-have-been cost, and
// LiteLLM's own response_cost is 0. It maps to no events, so nothing is
// billed and no COGS is recorded.
// Mutation-verify: remove the cache_hit check → events with observed cost.
func TestMapPayload_CacheHitSkipped(t *testing.T) {
	p := mockAnthropicRecord(t, "cache-hit")
	if p.CacheHit == nil || !*p.CacheHit || p.CostBreakdown == nil || p.CostBreakdown.TotalCost == 0 {
		t.Fatalf("fixture is not a cache hit with a cost_breakdown: %+v", p)
	}
	m, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(m.Events) != 0 || m.Skip != SkipCacheHit {
		t.Fatalf("cache hit mapped to %d events (skip %q), want none and SkipCacheHit", len(m.Events), m.Skip)
	}

	// cache_hit null (the split fixture) or false is a real call.
	no := false
	p.CacheHit = &no
	p.ID = "not-a-hit"
	if m, err := MapPayload(p); err != nil || len(m.Events) != 2 || m.Skip != "" {
		t.Fatalf("cache_hit=false: %d events, skip %q, err %v; want input and output", len(m.Events), m.Skip, err)
	}
}

// A failed call is skipped because LiteLLM says it failed, not because its
// token counts happen to be zero, and before the user check, so a failed
// call with no user is skipped, not refused.
// Mutation-verify: remove the status check → the token-carrying failure
// bills 2 events; move it after the user check → ErrMissingUser.
func TestMapPayload_FailureSkipped(t *testing.T) {
	p := mockAnthropicRecord(t, "failure")
	if p.Status != "failure" {
		t.Fatalf("fixture status = %q", p.Status)
	}
	m, err := MapPayload(p)
	if err != nil || len(m.Events) != 0 || m.Skip != SkipFailure {
		t.Fatalf("failure record: %d events, skip %q, err %v; want none and SkipFailure", len(m.Events), m.Skip, err)
	}

	withTokens := p
	withTokens.Status = "FAILURE"
	withTokens.PromptTokens, withTokens.CompletionTokens = 1000, 500
	withTokens.Metadata = nil
	if m, err := MapPayload(withTokens); err != nil || len(m.Events) != 0 || m.Skip != SkipFailure {
		t.Fatalf("failure carrying 1000/500 tokens: %d events, skip %q, err %v; want none", len(m.Events), m.Skip, err)
	}

	anon := withTokens
	anon.User, anon.EndUser = "", ""
	if m, err := MapPayload(anon); err != nil || m.Skip != SkipFailure {
		t.Fatalf("failure with no user: skip %q, err %v; want SkipFailure and no error", m.Skip, err)
	}
}

// ReleaseHeld finds a held cache-write row's input row from its key alone
// (usage.CallInputKey), so it must agree with the keys this mapper writes:
// every write row maps to the call's input key, no other row does.
func TestCallInputKey_MatchesMapperKeys(t *testing.T) {
	m, err := MapPayload(mockAnthropicRecord(t, "cache-write-5m-1h"))
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	roles := byRole(t, m.Events)
	inputKey := roles[TokenTypeInput].IdempotencyKey
	for tt, e := range roles {
		got, ok := usage.CallInputKey(e.IdempotencyKey)
		isWrite := tt == TokenTypeCacheWrite5m || tt == TokenTypeCacheWrite1h
		if ok != isWrite || (isWrite && got != inputKey) {
			t.Errorf("CallInputKey(%q) = %q, %v; want %q, %v", e.IdempotencyKey, got, ok, inputKey, isWrite)
		}
	}
}
