package litellm

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// MeterKeyTokens is the single canonical meter every provider mapper writes
// to (ADR-044). Token ROLE rides on the `token_type` dimension, NOT on the
// meter key — so the anthropic_style / openai_style recipes (and the cost
// dashboard) see one `tokens` meter grouped by {model, token_type}, and a new
// provider never re-opens a meter-shape mismatch.
const MeterKeyTokens = "tokens"

// Canonical token-role values for the `token_type` dimension (ADR-044). The
// vocabulary is shared by the recipes and every provider mapper. The LiteLLM
// mapper emits input, cache_read, cache_write_5m, cache_write_1h and output.
const (
	TokenTypeInput        = "input"
	TokenTypeOutput       = "output"
	TokenTypeCacheRead    = "cache_read"
	TokenTypeCacheWrite5m = "cache_write_5m"
	TokenTypeCacheWrite1h = "cache_write_1h"
)

// SkipReason says why a record maps to no events. The handler counts every
// skipped record in SpendResponse.Skipped.
type SkipReason string

const (
	// SkipNotTokenBearing: a call type with no per-token billing model
	// (image generation, moderation, ...).
	SkipNotTokenBearing SkipReason = "not_token_bearing"
	// SkipFailure: LiteLLM reports the call failed (status "failure"). The
	// provider charged nothing, whatever token counts the record carries.
	SkipFailure SkipReason = "failure"
	// SkipCacheHit: LiteLLM answered from its own response cache. The
	// provider charged nothing; LiteLLM's response_cost is 0.
	SkipCacheHit SkipReason = "cache_hit"
	// SkipNoTokens: the record carries no token counts.
	SkipNoTokens SkipReason = "no_tokens"
)

// Mapping is what MapPayload makes of one record: the call's events, or the
// reason it has none.
type Mapping struct {
	// Events are the call's usage events, one per present token role.
	Events []ExternalIngest
	// Legacy is how the mapper before the cache-write split (2026-10-22)
	// recorded the same call: input = prompt − cache_read with the writes
	// inside it, no write events, and that input's pre-split cost. Set only
	// when the call has cache writes that Events splits off. A resend of a
	// call first stored that way must be stored that way again, or the
	// writes bill twice; the store decides which applies from the row
	// already under LegacyKey (usage.PostgresStore.IngestCallResolvingHeld).
	Legacy []ExternalIngest
	// LegacyKey is the call's input key, which Events and Legacy both
	// carry with different quantities. Set with Legacy.
	LegacyKey string
	// Skip is why Events is empty, when the record bills nothing for a
	// reason other than zero token counts.
	Skip SkipReason
}

// modelFamilies maps a normalized model id (lower-cased, provider-prefix
// stripped) to the canonical FAMILY token that the pricing recipes key their
// rules on (ADR-044: model normalization lives in the provider mapper, never
// the matcher). LiteLLM forwards the model string verbatim — a dated snapshot
// (claude-3-5-sonnet-20241022), a provider-prefixed string (azure/gpt-4o-eu),
// or a friendly alias — and the matcher does exact `properties @> dimension_match`
// (the industry-standard primitive: Orb/Lago/OpenMeter/Metronome all match
// exact strings, none family-match). So we canonicalize HERE: longest-prefix
// match the normalized id to a family, then emit the family on the `model`
// dimension (and the raw string on `model_raw` for audit).
//
// Entries are detection-prefix → recipe-token. The detection prefix is in real
// (dashed) API form; the recipe token is whatever the recipe rule uses (note
// anthropic_style keys "claude-3.5-sonnet" with a DOT). Keep in sync with the
// anthropic_style / openai_style recipe `dimension_match.model` values.
var modelFamilies = []modelFamily{
	// Current Anthropic lineup (5 generation + the Opus 4.x line; dated ids
	// like claude-opus-4-8-20260301 match via the prefix+"-" rule, and
	// longest-prefix keeps claude-opus-4-8 ahead of claude-opus-4).
	{"claude-fable-5", "claude-fable-5"},
	{"claude-mythos-5", "claude-mythos-5"},
	{"claude-opus-5", "claude-opus-5"},
	{"claude-opus-4-8", "claude-opus-4.8"},
	{"claude-opus-4-7", "claude-opus-4.7"},
	{"claude-opus-4-6", "claude-opus-4.6"},
	{"claude-opus-4-5", "claude-opus-4.5"},
	{"claude-sonnet-5", "claude-sonnet-5"},
	{"claude-sonnet-4-6", "claude-sonnet-4.6"},
	{"claude-sonnet-4-5", "claude-sonnet-4.5"},
	{"claude-haiku-4-5", "claude-haiku-4.5"},
	// Legacy Anthropic families — proxies mid-migration still route
	// them; removing a detection prefix silently un-families live
	// traffic (the model dim falls back to the raw id and no recipe
	// rule matches). claude-3-7-sonnet is deliberately ABSENT: retired
	// 2026-02-19 with no published price, so a recipe rule for it would
	// carry an invented rate — its traffic keeps the raw id and surfaces
	// as unmatched usage on the operator card instead (post-#551 loud).
	{"claude-opus-4-1", "claude-opus-4.1"},
	{"claude-opus-4", "claude-opus-4"},
	{"claude-sonnet-4", "claude-sonnet-4"},
	{"claude-3-5-sonnet", "claude-3.5-sonnet"},
	{"claude-3-5-haiku", "claude-3.5-haiku"},
	{"claude-3-opus", "claude-3-opus"},
	{"claude-3-sonnet", "claude-3-sonnet"},
	{"claude-3-haiku", "claude-3-haiku"},
	// Current OpenAI lineup. The match rule is equality-or-prefix+"-",
	// so the DOTTED ids (gpt-5.6, gpt-4.1) never collide with the bare
	// gpt-5 entry and need their own rows; -mini/-nano/-pro and the 5.6
	// tier names still win over their base family via longest-prefix.
	{"gpt-5.6-sol", "gpt-5.6-sol"},
	{"gpt-5.6-terra", "gpt-5.6-terra"},
	{"gpt-5.6-luna", "gpt-5.6-luna"},
	{"gpt-5.5-pro", "gpt-5.5-pro"},
	{"gpt-5.5", "gpt-5.5"},
	{"gpt-5.4-mini", "gpt-5.4-mini"},
	{"gpt-5.4-nano", "gpt-5.4-nano"},
	{"gpt-5.4-pro", "gpt-5.4-pro"},
	{"gpt-5.4", "gpt-5.4"},
	{"gpt-5.3-codex", "gpt-5.3-codex"},
	// o-series reasoning models (legacy; per-model page prices).
	{"o1-mini", "o1-mini"},
	{"o1-pro", "o1-pro"},
	{"o1", "o1"},
	{"o3-mini", "o3-mini"},
	{"o3-pro", "o3-pro"},
	{"o3", "o3"},
	{"o4-mini", "o4-mini"},
	{"gpt-5-mini", "gpt-5-mini"},
	{"gpt-5-nano", "gpt-5-nano"},
	{"gpt-5.1", "gpt-5.1"},
	{"gpt-5", "gpt-5"},
	{"gpt-4.1-mini", "gpt-4.1-mini"},
	{"gpt-4.1-nano", "gpt-4.1-nano"},
	{"gpt-4.1", "gpt-4.1"},
	// Legacy OpenAI families.
	{"gpt-4o-mini", "gpt-4o-mini"}, // before gpt-4o (longest-prefix wins)
	{"gpt-4o", "gpt-4o"},
	{"gpt-4-turbo", "gpt-4-turbo"},
	{"gpt-3.5-turbo", "gpt-3.5-turbo"},
	{"text-embedding-3-small", "text-embedding-3-small"},
	{"text-embedding-3-large", "text-embedding-3-large"},
	{"text-embedding-ada-002", "text-embedding-ada-002"},
}

// modelFamily is one entry of modelFamilies: the model's id in API form and
// the token the recipes key their rules on.
type modelFamily struct {
	id, recipeToken string
}

// promptTiers lists, by recipe token, the models whose price depends on the
// prompt's length. Their events carry context_tier.
var promptTiers = map[string]promptTier{}

// promptTier is a model's two prices by prompt length. A prompt counts
// input, cache reads and cache writes (Anthropic's pricing page, "Long
// context pricing").
type promptTier struct {
	// over: a prompt of more tokens than this takes the higher price.
	over int64
	// upTo and above are the context_tier values of the two tiers.
	upTo, above string
}

// The context_tier dimension: set on the events of a model whose price
// depends on the prompt's length.
const (
	DimContextTier      = "context_tier"
	ContextTierUpTo100k = "prompt_up_to_100k"
	ContextTierOver100k = "prompt_over_100k"
)

// providerPrefixes are stripped before family matching — LiteLLM prepends the
// routing provider to the model string for non-OpenAI deployments.
var providerPrefixes = []string{"azure/", "bedrock/", "vertex_ai/", "openai/", "anthropic/", "anthropic.", "us.", "eu.", "apac."}

// normalizeModel lower-cases the raw model string and strips provider
// prefixes.
func normalizeModel(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	for _, p := range providerPrefixes {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

// matchesFamily reports whether the normalized id s belongs to family f.
func matchesFamily(f modelFamily, s string) bool {
	return s == f.id || strings.HasPrefix(s, f.id+"-")
}

// lookupFamily resolves a raw LiteLLM model string to its family, or false
// if no known family matches. Match is longest-prefix so gpt-4o-mini-* beats
// gpt-4o-*.
func lookupFamily(raw string) (modelFamily, bool) {
	s := normalizeModel(raw)
	var best modelFamily
	found := false
	for _, f := range modelFamilies {
		if matchesFamily(f, s) && len(f.id) > len(best.id) {
			best, found = f, true
		}
	}
	return best, found
}

// canonicalModel resolves a raw LiteLLM model string to its canonical recipe
// family token, or "" if no known family matches (caller keeps the raw string
// and logs loudly — we never invent a family, which would mis-price).
func canonicalModel(raw string) string {
	f, _ := lookupFamily(raw)
	return f.recipeToken
}

// MapPayload converts a single LiteLLM StandardLoggingPayload into the
// call's usage events.
//
// Mapping (per ADR-044, superseding ADR-033's two-meter shape), all on the
// `tokens` meter with the role on the token_type dimension:
//
//	prompt − cache_read − cache writes → input
//	prompt_tokens_details.cached_tokens → cache_read
//	cache_creation_token_details.ephemeral_5m_input_tokens → cache_write_5m
//	cache_creation_token_details.ephemeral_1h_input_tokens → cache_write_1h
//	completion_tokens → output
//
// The checks run in this order, each before the next:
//   - call_type isn't token-bearing (image_gen, moderation, etc.): skipped
//   - status is "failure": skipped. A failed call bills nothing whatever
//     token counts it carries, and it is skipped before the user check, so
//     a failed call without a user is not an error.
//   - cache_hit is true (LiteLLM's response cache answered): skipped. Its
//     cost_breakdown holds what the call would have cost; the provider
//     charged nothing.
//   - no user: ErrMissingUser. The adapter refuses to bill anonymous usage
//     to an unknown customer; operators set user="<external_customer_id>".
//   - no token counts: no events
//   - no id: ErrMissingID
//
// ExternalIngest.ExternalCustomerID carries the EXTERNAL id; the handler
// resolves it to the internal customer_id once per call.
func MapPayload(p StandardLoggingPayload) (Mapping, error) {
	if !p.IsTokenBearing() {
		return Mapping{Skip: SkipNotTokenBearing}, nil
	}
	if strings.EqualFold(strings.TrimSpace(p.Status), "failure") {
		return Mapping{Skip: SkipFailure}, nil
	}
	if p.CacheHit != nil && *p.CacheHit {
		return Mapping{Skip: SkipCacheHit}, nil
	}
	user := p.callerID()
	if user == "" {
		return Mapping{}, ErrMissingUser
	}
	usage := p.tokenUsage()
	if usage == nil {
		return Mapping{Skip: SkipNoTokens}, nil
	}
	// Every event key is built from the call id. Without one, every id-less
	// call in a tenant would share the keys ":input", ":output" and
	// ":cache_read", and all but the first would be dropped as duplicates.
	if p.ID == "" {
		return Mapping{}, ErrMissingID
	}

	// Event timestamp: prefer EndTime (when the call actually completed), fall
	// back to StartTime. LiteLLM emits floats in unix-seconds; convert to
	// time.Time without loss for sub-second resolution.
	//
	// When the payload carries NEITHER (a synthetic or test-clock ingest that
	// omits both), leave the timestamp NIL rather than manufacturing
	// time.Now(): usage.Service then supplies the customer's effective-now —
	// the frozen_time of a test-clock-pinned customer, wall-clock otherwise —
	// exactly like the direct /v1/usage-events door (ADR-030, "no timestamp =>
	// frozen_time"). A manufactured time.Now() here is non-nil, so the service
	// skips the clock resolution and silently lands simulated usage at
	// wall-clock, defeating the test clock (the flagship advance-clock →
	// LiteLLM-ingest → billed demo depended on this).
	var ts *time.Time
	switch {
	case p.EndTime > 0:
		t := unixSecondsToTime(p.EndTime)
		ts = &t
	case p.StartTime > 0:
		t := unixSecondsToTime(p.StartTime)
		ts = &t
	}

	// Canonical model family on `model` (what recipe rules match), raw string
	// preserved on `model_raw` for audit/reconciliation (ADR-044 model
	// normalization in the mapper). An unrecognized model keeps its raw string
	// on `model` and is logged loudly — we never invent a family, which would
	// mis-price; it simply won't match a recipe rule (rates $0, loud by absence).
	modelDim := canonicalModel(p.Model)
	if modelDim == "" {
		modelDim = p.Model
		slog.Warn("litellm: unrecognized model — usage won't match recipe pricing rules until an alias is added (see modelFamilies)",
			"model", p.Model, "litellm_id", p.ID)
	}
	dims := map[string]any{
		"model":     modelDim,
		"model_raw": p.Model,
		"provider":  p.CustomLLMProvider,
	}
	// Forward useful operator-set metadata into dimensions for
	// pricing-rule dispatch. We promote `team_id` and `tags`
	// specifically because LiteLLM's spend page uses them as the
	// primary slice axes; everything else stays in the event's
	// metadata column for audit.
	if p.Metadata != nil {
		if v, ok := p.Metadata["team_id"]; ok {
			dims["team_id"] = v
		}
		// request_tags is list-typed on LiteLLM's wire ([]any after JSON
		// decode), but usage ingest accepts scalar dimension values only
		// (validateDimensions) — passing the array through rejected EVERY
		// token event on tagged calls, silently unbilling that traffic
		// (front-door audit 2026-07-05). Join to a stable comma-separated
		// string; scalars pass through unchanged.
		if v, ok := p.Metadata["request_tags"]; ok {
			if tags, isList := v.([]any); isList {
				parts := make([]string, 0, len(tags))
				for _, tag := range tags {
					parts = append(parts, fmt.Sprintf("%v", tag))
				}
				sort.Strings(parts)
				dims["request_tags"] = strings.Join(parts, ",")
			} else {
				dims["request_tags"] = v
			}
		}
	}

	// One event per present role on the single `tokens` meter, role carried as
	// the `token_type` dimension (ADR-044). Roles are ADDITIVE-DISJOINT:
	// prompt_tokens INCLUDES cache-read tokens and, for Anthropic, cache-write
	// tokens, so the uncached input is the remainder. The mapper does this
	// split (the engine never does role math).
	cacheRead := usage.cachedReadTokens()
	uncached := usage.PromptTokens - cacheRead
	if uncached < 0 {
		// Malformed payload (cached > prompt). Don't emit a negative quantity;
		// fall back to billing the whole prompt as uncached input.
		uncached = usage.PromptTokens
		cacheRead = 0
	}

	// Cache writes: each lifetime is its own role, priced by the recipes at
	// 1.25x (5m) and 2x (1h) input. When the split cannot be trusted the
	// writes stay inside input (1.0x), the pre-split mapping, and the
	// fallback is counted where an operator can see it.
	w5m, w1h, reason := usage.cacheWriteSplit(uncached)
	if reason != "" {
		cacheWriteUnsplit.WithLabelValues(metricProvider(p.CustomLLMProvider), reason).Inc()
		slog.Warn("litellm: cache-write tokens have no usable 5m/1h split — billed as input tokens, below the provider's cache-write price (velox_litellm_cache_write_unsplit_total)",
			"reason", reason, "cache_creation_tokens", usage.cacheWriteTokens(),
			"model", modelDim, "litellm_id", p.ID)
	}

	ev := func(tokenType string, qty int64, cost *int64) ExternalIngest {
		return ExternalIngest{
			ExternalCustomerID: user,
			MeterKey:           MeterKeyTokens,
			Quantity:           decimal.NewFromInt(qty),
			Dimensions:         dimsWithTokenType(dims, tokenType),
			IdempotencyKey:     p.ID + ":" + tokenType,
			Timestamp:          ts,
			Metadata:           buildEventMetadata(p, tokenType),
			ObservedCostMicros: cost,
		}
	}
	costs := roleCosts(p, cacheRead, w5m, w1h)

	// mapping is the call's events with uncached input `input` and the
	// given writes.
	mapping := func(input, w5m, w1h int64, inputCost *int64) []ExternalIngest {
		out := make([]ExternalIngest, 0, 5)
		// With writes split off, the input event is emitted even at 0: its
		// key is the claim that a replay of a pre-split record collides with.
		if input > 0 || w5m+w1h > 0 {
			out = append(out, ev(TokenTypeInput, input, inputCost))
		}
		if cacheRead > 0 {
			out = append(out, ev(TokenTypeCacheRead, cacheRead, costs.cacheRead))
		}
		if w5m > 0 {
			out = append(out, ev(TokenTypeCacheWrite5m, w5m, costs.write5m))
		}
		if w1h > 0 {
			out = append(out, ev(TokenTypeCacheWrite1h, w1h, costs.write1h))
		}
		if usage.CompletionTokens > 0 {
			out = append(out, ev(TokenTypeOutput, usage.CompletionTokens, costs.output))
		}
		return out
	}

	if w5m+w1h == 0 {
		return Mapping{Events: mapping(uncached, 0, 0, costs.legacyInput)}, nil
	}
	return Mapping{
		Events:    mapping(uncached-w5m-w1h, w5m, w1h, costs.input),
		Legacy:    mapping(uncached, 0, 0, costs.legacyInput),
		LegacyKey: p.ID + ":" + TokenTypeInput,
	}, nil
}

// dimsWithTokenType returns a copy of base with token_type set, so the two
// per-role events don't share (and overwrite) one dimensions map.
func dimsWithTokenType(base map[string]any, tokenType string) map[string]any {
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["token_type"] = tokenType
	return out
}

// ErrMissingUser is returned by MapPayload when the LiteLLM payload
// has no `user` field set. Operators must configure their LiteLLM
// callers to pass user=<external_customer_id>; without it the
// adapter has no customer to attribute the spend to.
// ErrMissingID is returned by MapPayload when the payload has no call id,
// which every event key is built from. Real LiteLLM proxies always send one.
var ErrMissingID = fmt.Errorf("litellm: payload.id is required (each event's dedup key is built from it)")

var ErrMissingUser = fmt.Errorf("litellm: payload.user is required (set user=<external_customer_id> on the litellm call)")

// ExternalIngest is the adapter's intermediate shape — uses the
// EXTERNAL customer id and meter key. The handler resolves these to
// internal IDs via the existing usage ingest path. Mirrors the
// existing usage.IngestRequest body shape so the resolver is shared.
type ExternalIngest struct {
	ExternalCustomerID string          `json:"external_customer_id"`
	MeterKey           string          `json:"meter_key"`
	Quantity           decimal.Decimal `json:"quantity"`
	Dimensions         map[string]any  `json:"dimensions,omitempty"`
	IdempotencyKey     string          `json:"idempotency_key,omitempty"`
	Timestamp          *time.Time      `json:"timestamp,omitempty"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
	// ObservedCostMicros is LiteLLM's OWN cost for this one half, in
	// micro-dollars — the ADR-079 D4 fast-follow, set only where D4 permits
	// (see observedCostMicros). nil means "fall to rate-table inference".
	ObservedCostMicros *int64 `json:"observed_cost_micros,omitempty"`
}

// buildEventMetadata composes the per-event metadata blob: LiteLLM's
// own metadata + a velox.* namespace for adapter-injected context
// (cost figures, source identifier). Operator can query usage events
// by metadata in the audit dashboard.
func buildEventMetadata(p StandardLoggingPayload, half string) map[string]any {
	out := map[string]any{
		"velox.source":     "litellm",
		"velox.litellm_id": p.ID,
		"velox.call_type":  p.CallType,
		"velox.token_half": half, // the token_type
	}
	if p.ResponseCost > 0 {
		out["velox.litellm_response_cost_usd"] = p.ResponseCost
	}
	if p.CostBreakdown != nil {
		// Surface the relevant half's cost. LiteLLM reports dollars-
		// as-float; we store verbatim so cost reconciliation against
		// the provider invoice is trivial.
		switch half {
		case "input":
			if p.CostBreakdown.InputCost > 0 {
				out["velox.litellm_cost_usd"] = p.CostBreakdown.InputCost
			}
		case "output":
			if p.CostBreakdown.OutputCost > 0 {
				out["velox.litellm_cost_usd"] = p.CostBreakdown.OutputCost
			}
		}
	}
	// Forward LiteLLM's own metadata under a namespaced key so it
	// doesn't collide with our velox.* keys. Operators can still
	// query by `metadata->litellm_metadata->>team_id` etc.
	if len(p.Metadata) > 0 {
		out["litellm_metadata"] = p.Metadata
	}
	return out
}

func unixSecondsToTime(secs float64) time.Time {
	whole := int64(secs)
	nanos := int64((secs - float64(whole)) * 1e9)
	return time.Unix(whole, nanos).UTC()
}

// callCosts is LiteLLM's own cost for each role of one call, in
// micro-dollars; nil means "fall back to the rate table" (see roleCosts).
type callCosts struct {
	// input is the fresh input's cost with the writes split off;
	// legacyInput is the input cost with the writes inside (the pre-split
	// mapping, and the fallback when the writes could not be split).
	input, legacyInput *int64
	cacheRead, output  *int64
	write5m, write1h   *int64
}

// roleCosts returns LiteLLM's own cost for the tokens of each event of one
// call, from cost_breakdown. cacheRead, w5m and w1h are the quantities the
// mapper split off the prompt (0 when it did not split).
//
// It encodes ADR-079 D4, and every clause is load-bearing:
//
//   - Only CostBreakdown is eligible, and each event gets the cost of its own
//     tokens. A single call fans out to up to five events, so stamping any
//     whole-call figure on each would multi-count COGS.
//   - ResponseCost is a whole-call figure and is therefore NEVER used here,
//     however tempting it is as a fallback when CostBreakdown is absent.
//   - input_cost covers the whole prompt, cache reads and cache writes
//     included (LiteLLM's logging spec: cache_read_cost and
//     cache_creation_cost are "reported as a subset of input_cost"). So the
//     cache_read event gets cache_read_cost, a split-off 5m write event gets
//     cache_creation_cost, and the input event gets input_cost minus both.
//     Before 2026-10-14 the input event took all of input_cost while the
//     cache_read event was costed again from the rate table, which counted
//     cache reads twice.
//   - When cache reads happened but cache_read_cost is absent, input_cost
//     cannot be split, so both input-side events fall to the rate table. The
//     same holds for split-off writes without cache_creation_cost.
//   - With 1-hour writes, LiteLLM 1.83.9's figures are not the provider's
//     cost: cache_creation_cost prices every write at the 5-minute rate
//     (cost_calculator.py), and input_cost leaves the 1-hour writes out
//     (observed: 0.153 for 1,000 fresh + 40,000 5m + 60,000 1h tokens on
//     claude-sonnet-4-5, against 0.513). The input and write events then
//     fall to the rate table; a stamped figure would be a made-up COGS.
//   - Non-finite or negative values are rejected rather than clamped. A NaN
//     would otherwise become a garbage int64 and silently corrupt margin;
//     falling back to the rate table is the honest answer.
//   - Zero is a real answer (a free/credited call), not "absent" — it is
//     stamped as 0 with source 'observed' rather than dropped, so the margin
//     card can tell "cost was zero" from "we don't know the cost".
//
// Dollars-float to micro-dollars uses ROUND HALF UP, matching the rate-table
// path's ROUND() in internal/usage/postgres.go so the two sources agree at
// the boundary. The input share is the difference of the rounded figures,
// so the events of one call add up to the rounded input_cost.
func roleCosts(p StandardLoggingPayload, cacheRead, w5m, w1h int64) callCosts {
	b := p.CostBreakdown
	if b == nil {
		return callCosts{}
	}
	var c callCosts
	c.output = dollarsToMicros(b.OutputCost)
	if b.CacheReadCost != nil {
		c.cacheRead = dollarsToMicros(*b.CacheReadCost)
	}

	// The input cost with the writes inside: input_cost minus the reads.
	c.legacyInput = dollarsToMicros(b.InputCost)
	if c.legacyInput != nil && cacheRead > 0 {
		c.legacyInput = minus(c.legacyInput, c.cacheRead)
	}

	if w5m+w1h == 0 || w1h > 0 || b.CacheCreationCost == nil {
		return c
	}
	c.write5m = dollarsToMicros(*b.CacheCreationCost)
	c.input = minus(c.legacyInput, c.write5m)
	if c.input == nil {
		c.write5m = nil
	}
	return c
}

// minus is a − b, or nil when either is unknown or b exceeds a (the figures
// do not nest, so neither is trustworthy).
func minus(a, b *int64) *int64 {
	if a == nil || b == nil || *b > *a {
		return nil
	}
	d := *a - *b
	return &d
}

// dollarsToMicros converts LiteLLM's dollars-as-float to micro-dollars,
// rounding half up, or returns nil for a value that is not a real cost.
func dollarsToMicros(dollars float64) *int64 {
	if math.IsNaN(dollars) || math.IsInf(dollars, 0) || dollars < 0 {
		return nil
	}
	micros := int64(math.Floor(dollars*1e6 + 0.5))
	return &micros
}
