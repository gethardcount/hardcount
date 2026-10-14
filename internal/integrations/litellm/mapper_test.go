package litellm

import (
	"errors"
	"testing"
)

func TestMapPayload_HappyPath(t *testing.T) {
	p := StandardLoggingPayload{
		ID:                "litellm_call_abc123",
		CallType:          "completion",
		Model:             "claude-3-5-sonnet-20241022",
		CustomLLMProvider: "anthropic",
		User:              "cus_acme",
		Usage: &Usage{
			PromptTokens:     1200,
			CompletionTokens: 350,
			TotalTokens:      1550,
		},
		ResponseCost: 0.018,
		Metadata: map[string]any{
			"team_id": "team_eng",
			"x_other": "ignored-by-dim-promotion",
		},
		StartTime: 1700000000.1,
		EndTime:   1700000003.456,
	}

	out, err := MapPayload(p)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 events (input + output), got %d", len(out))
	}

	in, outE := out[0], out[1]

	// ADR-044: one `tokens` meter; role rides on the token_type dimension.
	if in.MeterKey != MeterKeyTokens {
		t.Errorf("input meter key: got %q, want %q", in.MeterKey, MeterKeyTokens)
	}
	if in.Dimensions["token_type"] != TokenTypeInput {
		t.Errorf("input dim token_type: got %v, want %q", in.Dimensions["token_type"], TokenTypeInput)
	}
	if in.Quantity.IntPart() != 1200 {
		t.Errorf("input quantity: got %s, want 1200", in.Quantity)
	}
	if in.ExternalCustomerID != "cus_acme" {
		t.Errorf("input customer: got %q, want cus_acme", in.ExternalCustomerID)
	}
	if in.IdempotencyKey != "litellm_call_abc123:input" {
		t.Errorf("input idempotency: got %q", in.IdempotencyKey)
	}
	// ADR-044 model normalization: dated API string → canonical recipe family
	// on `model`; raw string preserved on `model_raw`.
	if in.Dimensions["model"] != "claude-3.5-sonnet" {
		t.Errorf("input dim model: got %v, want canonical family claude-3.5-sonnet", in.Dimensions["model"])
	}
	if in.Dimensions["model_raw"] != "claude-3-5-sonnet-20241022" {
		t.Errorf("input dim model_raw: got %v, want verbatim claude-3-5-sonnet-20241022", in.Dimensions["model_raw"])
	}
	if in.Dimensions["provider"] != "anthropic" {
		t.Errorf("input dim provider: got %v", in.Dimensions["provider"])
	}
	if in.Dimensions["team_id"] != "team_eng" {
		t.Errorf("input dim team_id: got %v (metadata promotion failed)", in.Dimensions["team_id"])
	}
	// x_other is NOT promoted (only the curated keys are).
	if _, ok := in.Dimensions["x_other"]; ok {
		t.Error("input dim should not carry non-promoted metadata keys")
	}

	if outE.MeterKey != MeterKeyTokens {
		t.Errorf("output meter key: got %q, want %q", outE.MeterKey, MeterKeyTokens)
	}
	if outE.Dimensions["token_type"] != TokenTypeOutput {
		t.Errorf("output dim token_type: got %v, want %q", outE.Dimensions["token_type"], TokenTypeOutput)
	}
	// The two events must NOT share one dimensions map (token_type would alias).
	if in.Dimensions["token_type"] == outE.Dimensions["token_type"] {
		t.Error("input and output events share a dimensions map — token_type aliased")
	}
	if outE.Quantity.IntPart() != 350 {
		t.Errorf("output quantity: got %s, want 350", outE.Quantity)
	}
	if outE.IdempotencyKey != "litellm_call_abc123:output" {
		t.Errorf("output idempotency: got %q", outE.IdempotencyKey)
	}
}

// TestCanonicalModel pins the model-normalization table (ADR-044): real
// LiteLLM model strings (dated snapshots, provider-prefixed, aliases) resolve
// to the recipe family token; unknown models resolve to "" (caller keeps raw).
func TestCanonicalModel(t *testing.T) {
	cases := map[string]string{
		"claude-3-5-sonnet-20241022":         "claude-3.5-sonnet",
		"claude-3-5-sonnet-20240620":         "claude-3.5-sonnet",
		"claude-3-opus-20240229":             "claude-3-opus",
		"claude-3-haiku-20240307":            "claude-3-haiku",
		"gpt-4o-2024-08-06":                  "gpt-4o",
		"gpt-4o-mini-2024-07-18":             "gpt-4o-mini", // longest-prefix: not gpt-4o
		"gpt-4o-mini":                        "gpt-4o-mini",
		"gpt-4o":                             "gpt-4o",
		"gpt-4-turbo-2024-04-09":             "gpt-4-turbo",
		"gpt-3.5-turbo-0125":                 "gpt-3.5-turbo",
		"azure/gpt-4o-eu":                    "gpt-4o", // provider prefix stripped
		"bedrock/claude-3-5-sonnet-20241022": "claude-3.5-sonnet",
		"anthropic.claude-3-haiku-20240307":  "claude-3-haiku",
		"text-embedding-3-small":             "text-embedding-3-small",
		"GPT-4O-2024-08-06":                  "gpt-4o", // case-insensitive
		"some-unknown-model-v9":              "",       // unrecognized → "" (raw kept upstream)
		// Current-generation families (2026-07-05 refresh).
		"claude-opus-4-5-20251101":             "claude-opus-4.5",
		"claude-sonnet-4-5-20250929":           "claude-sonnet-4.5",
		"claude-haiku-4-5-20251001":            "claude-haiku-4.5",
		"anthropic/claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"gpt-5-2025-08-07":                     "gpt-5",
		"gpt-5-mini-2025-08-07":                "gpt-5-mini", // longest-prefix: not gpt-5
		"gpt-5.1":                              "gpt-5.1",    // dotted id ≠ the bare gpt-5 entry
		"gpt-4.1-mini":                         "gpt-4.1-mini",
		"gpt-4.1-2025-04-14":                   "gpt-4.1",
		"gpt-9-experimental":                   "", // unknown FUTURE model still falls through
	}
	for raw, want := range cases {
		if got := canonicalModel(raw); got != want {
			t.Errorf("canonicalModel(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestMapPayload_CacheRead covers the cache_read role (ADR-044): prompt_tokens
// INCLUDES cached tokens, so the mapper splits them additive-disjoint —
// input = prompt_tokens − cached, cache_read = cached.
func TestMapPayload_CacheRead(t *testing.T) {
	p := StandardLoggingPayload{
		ID: "c1", CallType: "completion", User: "cus_x",
		Model: "claude-3-5-sonnet-20241022", CustomLLMProvider: "anthropic",
		Usage: &Usage{
			PromptTokens:         7296, // includes the 7277 cached
			CompletionTokens:     400,
			PromptTokensDetails:  &PromptTokensDetails{CachedTokens: 7277},
			CacheReadInputTokens: 7277,
		},
	}
	out, err := MapPayload(p)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	got := map[string]int64{}
	for _, e := range out {
		got[e.Dimensions["token_type"].(string)] = e.Quantity.IntPart()
		if e.Dimensions["model"] != "claude-3.5-sonnet" {
			t.Errorf("event model: got %v, want claude-3.5-sonnet", e.Dimensions["model"])
		}
	}
	// uncached input = 7296 − 7277 = 19; cache_read = 7277; output = 400.
	if got[TokenTypeInput] != 19 {
		t.Errorf("input qty: got %d, want 19 (uncached = prompt − cached)", got[TokenTypeInput])
	}
	if got[TokenTypeCacheRead] != 7277 {
		t.Errorf("cache_read qty: got %d, want 7277", got[TokenTypeCacheRead])
	}
	if got[TokenTypeOutput] != 400 {
		t.Errorf("output qty: got %d, want 400", got[TokenTypeOutput])
	}
	// Additive-disjoint: input + cache_read == prompt_tokens.
	if got[TokenTypeInput]+got[TokenTypeCacheRead] != 7296 {
		t.Errorf("input + cache_read = %d, want 7296 (= prompt_tokens)", got[TokenTypeInput]+got[TokenTypeCacheRead])
	}
}

// TestMapPayload_CacheWriteDeferred: cache-write (cache_creation) tokens are
// NOT yet emitted as events (LiteLLM exposes no 5m/1h TTL split). The fully-
// cached-write call (cached_tokens=0, cache_creation>0) emits only output here.
func TestMapPayload_CacheWriteDeferred(t *testing.T) {
	p := StandardLoggingPayload{
		ID: "w1", CallType: "completion", User: "cus_x",
		Model: "claude-3-5-sonnet-20241022", CustomLLMProvider: "anthropic",
		Usage: &Usage{
			PromptTokens:             19, // prompt_tokens excludes cache_creation
			CompletionTokens:         400,
			CacheCreationInputTokens: 7277,
		},
	}
	out, err := MapPayload(p)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for _, e := range out {
		if e.Dimensions["token_type"] == "cache_write" || e.Dimensions["token_type"] == "cache_write_5m" || e.Dimensions["token_type"] == "cache_write_1h" {
			t.Errorf("cache-write should be deferred (not emitted), got event token_type=%v", e.Dimensions["token_type"])
		}
	}
	// input (19) + output (400) only.
	if len(out) != 2 {
		t.Errorf("expected 2 events (input + output; cache-write deferred), got %d", len(out))
	}
}

func TestMapPayload_NonTokenBearingSkipped(t *testing.T) {
	cases := []string{"image_generation", "moderation", "speech", "transcription"}
	for _, ct := range cases {
		p := StandardLoggingPayload{
			ID: "x", CallType: ct, User: "cus_x",
			Usage: &Usage{PromptTokens: 100},
		}
		out, err := MapPayload(p)
		if err != nil {
			t.Errorf("%s: unexpected: %v", ct, err)
		}
		if len(out) != 0 {
			t.Errorf("%s: expected 0 events, got %d", ct, len(out))
		}
	}
}

func TestMapPayload_MissingUser(t *testing.T) {
	p := StandardLoggingPayload{
		ID: "x", CallType: "completion", User: "",
		Usage: &Usage{PromptTokens: 100},
	}
	_, err := MapPayload(p)
	if !errors.Is(err, ErrMissingUser) {
		t.Fatalf("expected ErrMissingUser, got %v", err)
	}
}

func TestMapPayload_ZeroTokens(t *testing.T) {
	// Empty response / failed call → both halves zero. Mapper
	// emits no events; handler counts as "skipped" not "error."
	p := StandardLoggingPayload{
		ID: "x", CallType: "completion", User: "cus_x",
		Usage: &Usage{PromptTokens: 0, CompletionTokens: 0},
	}
	out, err := MapPayload(p)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected 0 events for zero tokens, got %d", len(out))
	}
}

func TestMapPayload_EmbeddingOnlyPromptTokens(t *testing.T) {
	// Embedding calls only have prompt_tokens — no completion.
	// Mapper emits one input event, no output event.
	p := StandardLoggingPayload{
		ID: "emb1", CallType: "embedding", User: "cus_x",
		Model:             "text-embedding-3-large",
		CustomLLMProvider: "openai",
		Usage:             &Usage{PromptTokens: 500, CompletionTokens: 0},
	}
	out, err := MapPayload(p)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 event (input only), got %d", len(out))
	}
	if out[0].MeterKey != MeterKeyTokens {
		t.Errorf("embedding event meter: got %q, want %q", out[0].MeterKey, MeterKeyTokens)
	}
	if out[0].Dimensions["token_type"] != TokenTypeInput {
		t.Errorf("embedding token_type: got %v, want %q", out[0].Dimensions["token_type"], TokenTypeInput)
	}
}

func TestMapPayload_CostBreakdownSurfaced(t *testing.T) {
	p := StandardLoggingPayload{
		ID: "x", CallType: "completion", User: "cus_x",
		Usage: &Usage{PromptTokens: 100, CompletionTokens: 50},
		CostBreakdown: &CostBreakdown{
			InputCost:  0.012,
			OutputCost: 0.045,
			TotalCost:  0.057,
		},
	}
	out, _ := MapPayload(p)
	if len(out) != 2 {
		t.Fatalf("expected 2 events, got %d", len(out))
	}
	if got := out[0].Metadata["velox.litellm_cost_usd"]; got != 0.012 {
		t.Errorf("input event input_cost: got %v, want 0.012", got)
	}
	if got := out[1].Metadata["velox.litellm_cost_usd"]; got != 0.045 {
		t.Errorf("output event output_cost: got %v, want 0.045", got)
	}
	if got := out[0].Metadata["velox.source"]; got != "litellm" {
		t.Errorf("velox.source: got %v", got)
	}
}

func TestMapPayload_TimestampFromEndTime(t *testing.T) {
	p := StandardLoggingPayload{
		ID: "x", CallType: "completion", User: "cus_x",
		Usage:     &Usage{PromptTokens: 1},
		StartTime: 1700000000,
		EndTime:   1700000003.5,
	}
	out, _ := MapPayload(p)
	if len(out) == 0 {
		t.Fatal("expected at least one event")
	}
	if out[0].Timestamp == nil {
		t.Fatal("timestamp not set")
	}
	// EndTime takes precedence over StartTime.
	if out[0].Timestamp.Unix() != 1700000003 {
		t.Errorf("timestamp seconds: got %d, want 1700000003", out[0].Timestamp.Unix())
	}
}

// TestMapPayload_RequestTagsListBecomesScalar pins the front-door-audit fix:
// LiteLLM sends request_tags as a JSON list; usage ingest accepts scalar
// dimension values only. Pre-fix the []any passed through and EVERY token
// event on tagged calls was rejected at ingest — tagged traffic silently
// unbilled. The mapper now joins to a sorted comma-separated string.
func TestMapPayload_RequestTagsListBecomesScalar(t *testing.T) {
	p := StandardLoggingPayload{
		ID: "call_tags", CallType: "completion", Model: "gpt-4o", User: "cus_x",
		Usage:    &Usage{PromptTokens: 10, CompletionTokens: 5},
		Metadata: map[string]any{"request_tags": []any{"prod", "batch"}},
	}
	out, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no events")
	}
	for _, ev := range out {
		got, ok := ev.Dimensions["request_tags"].(string)
		if !ok {
			t.Fatalf("request_tags dimension = %T (%v), want scalar string", ev.Dimensions["request_tags"], ev.Dimensions["request_tags"])
		}
		if got != "batch,prod" {
			t.Errorf("request_tags = %q, want sorted joined \"batch,prod\"", got)
		}
	}
	// Scalar tags still pass through untouched.
	p.Metadata = map[string]any{"request_tags": "prod"}
	out, err = MapPayload(p)
	if err != nil {
		t.Fatalf("map scalar: %v", err)
	}
	if got := out[0].Dimensions["request_tags"]; got != "prod" {
		t.Errorf("scalar request_tags = %v, want \"prod\"", got)
	}
}

// TestMapPayload_TimestampSourcing pins the test-clock fix: the mapper must NOT
// manufacture a wall-clock timestamp when the payload omits both EndTime and
// StartTime. It leaves Timestamp nil so usage.Service supplies the customer's
// effective-now (a test-clock-pinned customer's frozen_time) — the flagship
// advance-clock → LiteLLM-ingest → billed demo depends on this. A non-nil
// time.Now() here is treated as an explicit timestamp and silently defeats the
// clock, landing simulated usage at wall-clock.
func TestMapPayload_TimestampSourcing(t *testing.T) {
	base := StandardLoggingPayload{
		ID: "call_ts", CallType: "completion", Model: "claude-3-5-sonnet-20241022",
		CustomLLMProvider: "anthropic", User: "cus_acme",
		Usage: &Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}

	t.Run("no start/end leaves nil timestamp (service applies frozen_time)", func(t *testing.T) {
		out, err := MapPayload(base) // StartTime/EndTime both zero
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if len(out) == 0 {
			t.Fatal("expected events")
		}
		for _, e := range out {
			if e.Timestamp != nil {
				t.Errorf("timestamp must be nil when payload omits start/end (got %v) — a non-nil value defeats the test clock", *e.Timestamp)
			}
		}
	})

	t.Run("EndTime is used verbatim, not wall-clock", func(t *testing.T) {
		p := base
		p.EndTime = 1700000003.456
		out, err := MapPayload(p)
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		want := unixSecondsToTime(1700000003.456)
		for _, e := range out {
			if e.Timestamp == nil || !e.Timestamp.Equal(want) {
				t.Errorf("timestamp: got %v, want EndTime %v", e.Timestamp, want)
			}
		}
	})

	t.Run("only StartTime present uses StartTime", func(t *testing.T) {
		p := base
		p.StartTime = 1700000000.1
		out, err := MapPayload(p)
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		want := unixSecondsToTime(1700000000.1)
		for _, e := range out {
			if e.Timestamp == nil || !e.Timestamp.Equal(want) {
				t.Errorf("timestamp: got %v, want StartTime %v", e.Timestamp, want)
			}
		}
	})
}

// Every event key is built from the call id. Without one, all id-less calls
// in a tenant would share ":input"/":output" and all but the first would be
// dropped as duplicates.
func TestMapPayload_MissingID(t *testing.T) {
	p := StandardLoggingPayload{
		ID: "", CallType: "completion", User: "acme",
		Usage: &Usage{PromptTokens: 100, CompletionTokens: 10},
	}
	if _, err := MapPayload(p); !errors.Is(err, ErrMissingID) {
		t.Fatalf("expected ErrMissingID, got %v", err)
	}
}
