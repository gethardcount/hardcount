// Package litellm adapts LiteLLM Proxy's StandardLoggingPayload to
// Velox usage events. LiteLLM is the de-facto AI-API gateway for
// self-hosted LLM stacks (Anthropic / OpenAI / Bedrock / etc.) — this
// adapter is the wedge integration: drop a `generic_api` callback in
// LiteLLM, get per-call token + cost tracking in Velox out of the
// box. See docs/integrations/litellm.md + ADR-033.
package litellm

import (
	"encoding/json"
	"strings"
)

// StandardLoggingPayload mirrors LiteLLM Proxy's per-call log shape:
// https://docs.litellm.ai/docs/proxy/logging_spec
//
// We deliberately accept the LiteLLM wire shape as-is rather than
// reshape it on the partner side. Operator just configures LiteLLM's
// generic_api callback at this endpoint with a Velox API key, and
// every completion lands in Velox without any glue code.
//
// Fields are a deliberate subset of LiteLLM's spec — only what the
// adapter actually consumes. LiteLLM's payload carries many more
// fields (prompts, responses, raw API params, etc.); accepting them
// in the wire shape but only using a subset keeps forward-compat
// when LiteLLM adds new fields.
type StandardLoggingPayload struct {
	// ID is LiteLLM's per-call identifier. Used as the idempotency
	// key prefix on every usage event derived from this payload.
	// Replays with the same id are silently deduped by the usage
	// store's UNIQUE (tenant_id, livemode, idempotency_key) — the
	// dedup scope is tenant-wide, so the mapper suffixes this id
	// with the token type to keep sibling events distinct.
	ID string `json:"id"`

	// CallType narrows what kind of LLM operation this was —
	// "completion", "embedding", "image_generation", etc. We only
	// emit usage events for token-bearing call types (completion,
	// chat, embedding). Other types are accepted but skipped.
	CallType string `json:"call_type"`

	// Model is the resolved model name LiteLLM dispatched to
	// (e.g. "claude-3-5-sonnet-20241022", "gpt-4o-2024-08-06").
	// Stamped as a dimension on every emitted usage event so the
	// operator can break cost down by model in the dashboard.
	Model string `json:"model"`

	// CustomLLMProvider is LiteLLM's inferred upstream provider
	// — "anthropic", "openai", "bedrock", "vertex_ai", etc.
	// Stamped as a dimension; lets operators slice spend by
	// provider for procurement / multi-cloud cost attribution.
	CustomLLMProvider string `json:"custom_llm_provider"`

	// User is LiteLLM's caller identity, conventionally the
	// end-user identifier the partner passes to litellm.completion
	// (`user="..."`). We treat this as the Velox external_customer_id
	// — operators set their LiteLLM call's user= to the customer's
	// Velox external_id. Missing → that row is rejected with a per-row
	// entry in the response's errors[] (the batch still answers 200);
	// the adapter refuses to bill "anonymous" usage to "unknown customer".
	User string `json:"user"`

	// EndUser is where a real LiteLLM proxy puts the caller's `user=` value:
	// the logged record has no "user" field at all. User above is kept for
	// hand-built and older payloads; callerID picks whichever is set.
	EndUser string `json:"end_user"`

	// Usage carries the token counts the adapter maps to two
	// usage events (input + output). Embedding calls carry only
	// prompt_tokens; the output event is skipped when 0.
	Usage *Usage `json:"usage,omitempty"`

	// PromptTokens / CompletionTokens / TotalTokens are the token counts as a
	// real LiteLLM proxy sends them: top-level, with no "usage" object. The
	// full usage object, including cache counts, rides in
	// metadata.usage_object. tokenUsage resolves the three places.
	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`

	// Status is "success" or "failure". LiteLLM sends a failed call (a 429,
	// a 5xx, a timeout) to the same callback. MapPayload skips a failure
	// whatever token counts it carries: the provider charged nothing.
	Status string `json:"status,omitempty"`

	// CacheHit is true when LiteLLM answered from its own response cache
	// (not the provider's prompt cache). The record then carries the cached
	// response's full token counts, response_cost 0 and an id ending in
	// "_cache_hit<time>", and the provider charged nothing. MapPayload skips
	// it. Null and false both mean a call that reached the provider.
	CacheHit *bool `json:"cache_hit,omitempty"`

	// SavedCacheCost is what a cache hit would have cost had it reached the
	// provider. Decoded so the field is named here; it is never a cost.
	SavedCacheCost float64 `json:"saved_cache_cost,omitempty"`

	// ResponseCost is LiteLLM's pre-computed dollar cost for this
	// call (legacy single-field; cost_breakdown is the newer
	// per-component shape). NOTE (ADR-079 census correction): this is
	// accepted on the wire and stamped into the mapper's in-memory
	// metadata, but the ingest path DROPS it — usage_events has no
	// metadata column and IngestInput carries none, so it is never
	// persisted. It is also a WHOLE-CALL figure: a call maps to up to
	// five per-role events, so stamping it per event would multi-count
	// COGS. Velox's COGS comes from the operator's provider_cost_rates
	// table at ingest (ADR-079), or per role from CostBreakdown (never
	// from this field; see observedCostMicros).
	ResponseCost float64 `json:"response_cost,omitempty"`

	// CostBreakdown is LiteLLM's per-component cost split. Same
	// audit-only role as ResponseCost — informs the operator,
	// doesn't drive billing.
	CostBreakdown *CostBreakdown `json:"cost_breakdown,omitempty"`

	// StartTime / EndTime are unix-seconds floats (LiteLLM's
	// convention). EndTime is the event timestamp, else StartTime.
	// When both are zero the mapper leaves the timestamp empty and
	// the usage service stamps the customer's effective now: the
	// test clock's frozen_time for a pinned customer, wall-clock
	// otherwise. The event is not dropped (see mapper.go).
	StartTime float64 `json:"startTime,omitempty"`
	EndTime   float64 `json:"endTime,omitempty"`

	// Metadata is a free-form map LiteLLM passes through from the
	// caller. team_id and request_tags are promoted into event
	// DIMENSIONS (the mapper's dims block); the rest is currently
	// dropped at ingest — usage_events has no metadata column
	// (ADR-079 census; an earlier version of this comment claimed
	// otherwise).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Usage is LiteLLM's token-count subset (OpenAI-compatible shape).
//
// Prompt-cache fields (ADR-044 cache roles): LiteLLM normalizes every
// provider's cache accounting into this OpenAI-compatible shape. Two facts the
// mapper relies on, checked against LiteLLM 1.83.9:
//   - prompt_tokens INCLUDES cached (read) tokens, so the uncached input is
//     prompt_tokens − prompt_tokens_details.cached_tokens (additive-disjoint).
//   - cache_creation (write) tokens are ALSO in prompt_tokens for Anthropic
//     (llms/anthropic/chat/transformation.py adds them), and
//     prompt_tokens_details.cache_creation_token_details carries their 5m/1h
//     split (ephemeral_5m_input_tokens, ephemeral_1h_input_tokens), copied
//     from Anthropic's usage.cache_creation. The mapper splits them off the
//     input into cache_write_5m and cache_write_1h (see cacheWriteSplit).
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	// PromptTokensDetails carries the OpenAI-shape cache-read count, and for
	// Anthropic the cache-write count and its 5m/1h split.
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`

	// CacheReadInputTokens / CacheCreationInputTokens are Anthropic's top-level
	// cache fields, surfaced verbatim by LiteLLM. CacheReadInputTokens mirrors
	// PromptTokensDetails.CachedTokens and CacheCreationInputTokens mirrors
	// PromptTokensDetails.CacheCreationTokens (we take the max of each pair).
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// callerID is the customer reference on the record: the `user=` the caller
// passed to LiteLLM. A real proxy logs it as end_user (and again as
// metadata.user_api_key_end_user_id); hand-built payloads use user.
func (p *StandardLoggingPayload) callerID() string {
	for _, v := range []string{p.User, p.EndUser} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	if s, ok := p.Metadata["user_api_key_end_user_id"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// tokenUsage is the record's token counts, from the first place that has
// them: an explicit usage object, then metadata.usage_object (what a real
// proxy sends, and the only place it carries cache counts), then the bare
// top-level counts. Nil when the record carries no counts at all.
func (p *StandardLoggingPayload) tokenUsage() *Usage {
	if p.Usage != nil {
		return p.Usage
	}
	if obj, ok := p.Metadata["usage_object"].(map[string]any); ok {
		// Round-trip through JSON so the nested details decode exactly as
		// they would on the top-level usage field.
		if raw, err := json.Marshal(obj); err == nil {
			var u Usage
			if json.Unmarshal(raw, &u) == nil && (u.PromptTokens != 0 || u.CompletionTokens != 0) {
				return &u
			}
		}
	}
	if p.PromptTokens != 0 || p.CompletionTokens != 0 {
		return &Usage{PromptTokens: p.PromptTokens, CompletionTokens: p.CompletionTokens, TotalTokens: p.TotalTokens}
	}
	return nil
}

// PromptTokensDetails is the OpenAI-compatible breakdown of prompt tokens.
type PromptTokensDetails struct {
	// CachedTokens is the subset of prompt_tokens served from cache (a cache
	// hit / read). prompt_tokens already includes these.
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	// CacheCreationTokens is the subset of prompt_tokens written to the
	// provider's prompt cache (Anthropic). Mirrors cache_creation_input_tokens.
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	// CacheCreationTokenDetails splits CacheCreationTokens by cache lifetime.
	CacheCreationTokenDetails *CacheCreationTokenDetails `json:"cache_creation_token_details,omitempty"`
}

// CacheCreationTokenDetails is LiteLLM's copy of Anthropic's
// usage.cache_creation: the cache-write tokens by lifetime. Each is null
// when Anthropic did not send it.
type CacheCreationTokenDetails struct {
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens,omitempty"`
}

// Reasons a call's cache writes could not be split by lifetime. The mapper
// then bills them as input, as before the split existed, and counts the
// reason on velox_litellm_cache_write_unsplit_total.
const (
	// UnsplitNoSplit: the record has cache-write tokens but no 5m/1h split.
	UnsplitNoSplit = "no_split"
	// UnsplitSumMismatch: the split does not add up to the cache-write count.
	UnsplitSumMismatch = "sum_mismatch"
	// UnsplitNotInPrompt: the cache writes do not fit inside prompt_tokens
	// minus the cache reads, so prompt_tokens cannot include them.
	UnsplitNotInPrompt = "not_in_prompt"
)

// cacheWriteTokens is the cache-write count: the larger of the two places
// LiteLLM reports it.
func (u *Usage) cacheWriteTokens() int64 {
	w := u.CacheCreationInputTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CacheCreationTokens > w {
		w = u.PromptTokensDetails.CacheCreationTokens
	}
	if w < 0 {
		return 0
	}
	return w
}

// cacheWriteSplit returns the call's 5-minute and 1-hour cache-write tokens,
// or a reason they cannot be split off the input. uncached is prompt_tokens
// minus the cache reads, which LiteLLM 1.83.9 makes include the writes.
//
// It succeeds only when the split is present, neither part is negative, and
// the parts add up to the cache-write count: a split that does not account
// for every write token would bill some of them at the wrong price. A call
// with no cache writes returns 0, 0 and no reason.
func (u *Usage) cacheWriteSplit(uncached int64) (w5m, w1h int64, reason string) {
	total := u.cacheWriteTokens()
	var d *CacheCreationTokenDetails
	if u.PromptTokensDetails != nil {
		d = u.PromptTokensDetails.CacheCreationTokenDetails
	}
	if d != nil {
		if d.Ephemeral5mInputTokens != nil {
			w5m = *d.Ephemeral5mInputTokens
		}
		if d.Ephemeral1hInputTokens != nil {
			w1h = *d.Ephemeral1hInputTokens
		}
	}
	switch {
	case total == 0 && w5m == 0 && w1h == 0:
		return 0, 0, ""
	case d == nil || (d.Ephemeral5mInputTokens == nil && d.Ephemeral1hInputTokens == nil):
		return 0, 0, UnsplitNoSplit
	case w5m < 0 || w1h < 0 || w5m+w1h != total:
		return 0, 0, UnsplitSumMismatch
	case w5m+w1h > uncached:
		return 0, 0, UnsplitNotInPrompt
	}
	return w5m, w1h, ""
}

// cachedReadTokens returns the cache-read token count, taking the max of the
// OpenAI-shape (prompt_tokens_details.cached_tokens) and Anthropic-shape
// (cache_read_input_tokens) fields — LiteLLM may populate either depending on
// provider/version, and they represent the same quantity.
func (u *Usage) cachedReadTokens() int64 {
	read := u.CacheReadInputTokens
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > read {
		read = u.PromptTokensDetails.CachedTokens
	}
	if read < 0 {
		return 0
	}
	return read
}

// CostBreakdown is LiteLLM's per-component cost split (cents NOT
// supported upstream — dollars-as-float, per their wire). Velox
// stores the value verbatim on the usage event metadata; billing
// math runs through the operator's configured rating rules, not
// these floats.
type CostBreakdown struct {
	// InputCost is the cost of the whole prompt: fresh input, cache reads
	// and cache writes. LiteLLM's logging spec reports cache_read_cost and
	// cache_creation_cost "as a subset of input_cost".
	InputCost  float64 `json:"input_cost"`
	OutputCost float64 `json:"output_cost"`
	// CacheReadCost is the part of InputCost spent on cache reads. LiteLLM
	// leaves it out when it is zero or when it could not price cache reads,
	// so nil means "unknown", not "free".
	CacheReadCost *float64 `json:"cache_read_cost,omitempty"`
	// CacheCreationCost is LiteLLM's figure for the cache writes. LiteLLM
	// 1.83.9 computes it as every write token at the 5-minute rate
	// (cost_calculator.py), so it is the writes' real cost only when the call
	// has no 1-hour writes. Nil means "unknown", as for CacheReadCost.
	CacheCreationCost *float64 `json:"cache_creation_cost,omitempty"`
	ToolUsageCost     float64  `json:"tool_usage_cost,omitempty"`
	TotalCost         float64  `json:"total_cost"`
}

// SpendRequest is the wire shape POSTed to /v1/integrations/litellm/spend.
// Accepts either a single payload (LiteLLM's default callback shape)
// or a batch (some self-hosted setups buffer + flush). The handler
// normalizes both into a slice before mapping.
type SpendRequest struct {
	// Single is set when LiteLLM POSTs one call per request.
	// Single and Batch are mutually exclusive — the handler picks
	// whichever is non-empty.
	Single *StandardLoggingPayload `json:",inline,omitempty"`
	// Batch is set when LiteLLM (or a downstream buffer) sends
	// multiple calls per request.
	Batch []StandardLoggingPayload `json:"events,omitempty"`
}

// IsTokenBearing reports whether this call type produces tokens
// Velox should map to usage events. Other types (image_generation,
// moderation, etc.) are accepted by the adapter but skipped without
// emitting events — they don't have a clean per-token billing model.
func (p *StandardLoggingPayload) IsTokenBearing() bool {
	switch p.CallType {
	case "completion", "acompletion", "chat", "chat.completion",
		"embedding", "aembedding",
		"text_completion", "atext_completion":
		return true
	default:
		return false
	}
}
