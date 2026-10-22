package litellm

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// TestCanonicalModel_ClaudeIDs runs every Claude model name a LiteLLM 1.83.9
// spend log can carry, the pricing page's ids in each platform's form, and
// probes, against the expected family in the golden list. The list is
// written by testdata/list_claude_model_ids.py from the reviewed rule in the
// design (velox-ops design-litellm-model-families-2026-10-22.md), not from
// this code.
//
// Decision 2 (2026-10-22): a Claude name maps to a family only as the
// family's exact id, or the id plus a date or version ending. Anything else
// keeps its raw name and is unpriced, loudly. It never folds into an older
// family.
func TestCanonicalModel_ClaudeIDs(t *testing.T) {
	f, err := os.Open("testdata/litellm-1.83.9-claude-model-ids.tsv")
	if err != nil {
		t.Fatalf("open golden list: %v", err)
	}
	defer func() { _ = f.Close() }()

	rows := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			t.Fatalf("bad golden line %q", line)
		}
		form, want := parts[1], parts[2]
		rows++
		if got := canonicalModel(form); got != want {
			t.Errorf("canonicalModel(%q) = %q, want %q (%s)", form, got, want, parts[0])
		}
		// No name may match two Claude families: with strict endings the
		// match is unique, so the family list's order cannot matter.
		var hits []string
		s := normalizeModel(form)
		for _, fam := range modelFamilies {
			if strings.HasPrefix(fam.id, "claude-") && matchesFamily(fam, s) {
				hits = append(hits, fam.recipeToken)
			}
		}
		if len(hits) > 1 {
			t.Errorf("%q matches %d Claude families %v, want at most one", form, len(hits), hits)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read golden list: %v", err)
	}
	if rows < 900 {
		t.Fatalf("golden list has %d rows, want the full list (>900)", rows)
	}
}

// pricingPageClaudeIDs is every model id with a price on Anthropic's pricing
// page, checked 2026-10-22
// (https://platform.claude.com/docs/en/about-claude/pricing), and the
// anthropic_style family each must bill under.
var pricingPageClaudeIDs = map[string]string{
	"claude-fable-5-1":           "claude-fable-5.1",
	"claude-mythos-5-1":          "claude-mythos-5.1",
	"claude-fable-5":             "claude-fable-5",
	"claude-mythos-5":            "claude-mythos-5",
	"claude-opus-5-5":            "claude-opus-5.5",
	"claude-opus-5":              "claude-opus-5",
	"claude-opus-4-8":            "claude-opus-4.8",
	"claude-opus-4-7":            "claude-opus-4.7",
	"claude-opus-4-6":            "claude-opus-4.6",
	"claude-opus-4-5":            "claude-opus-4.5",
	"claude-opus-4-5-20251101":   "claude-opus-4.5",
	"claude-opus-4-1":            "claude-opus-4.1",
	"claude-opus-4-1-20250805":   "claude-opus-4.1",
	"claude-opus-4-0":            "claude-opus-4",
	"claude-opus-4-20250514":     "claude-opus-4",
	"claude-sonnet-5-5":          "claude-sonnet-5.5",
	"claude-sonnet-5":            "claude-sonnet-5",
	"claude-sonnet-4-6":          "claude-sonnet-4.6",
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"claude-sonnet-4-0":          "claude-sonnet-4",
	"claude-sonnet-4-20250514":   "claude-sonnet-4",
	"claude-haiku-5-5":           "claude-haiku-5.5",
	"claude-haiku-4-5":           "claude-haiku-4.5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
	"claude-3-5-haiku-20241022":  "claude-3.5-haiku",
}

func TestCanonicalModel_PricingPageIDs(t *testing.T) {
	for id, want := range pricingPageClaudeIDs {
		for _, form := range []string{
			id,
			"anthropic/" + id,
			"bedrock/global.anthropic." + id,
			"bedrock/converse/us.anthropic." + id,
			"jp.anthropic." + id,
			"vertex_ai/" + id + "@default",
			"azure_ai/" + id,
		} {
			if got := canonicalModel(form); got != want {
				t.Errorf("canonicalModel(%q) = %q, want %q", form, got, want)
			}
		}
	}
}

// mustStayRaw are names that are not a known model's id plus a date or
// version ending. Each must keep its raw name (decision 2), whatever older
// family its prefix looks like.
var mustStayRaw = []string{
	"claude-opus-4-9",            // was priced as Claude Opus 4
	"claude-opus-5-6",            // was priced as Claude Opus 5
	"claude-fable-5-2",           // was priced as Claude Fable 5
	"claude-sonnet-4-5-thinking", // not an Anthropic id
	"claude-opus-4-6-fast",       // not an Anthropic id
	"claude-opus-4-6[1m]",        // a context-window flag, not a version
	"claude-sonnet-6",
	"claude-haiku-5",
	"claude-mythos-preview",
	"claude-3-7-sonnet-20250219", // no published price
	"claude-sonnet-4.6",          // an aggregator's own name
	"openrouter/anthropic/claude-sonnet-4.6",
	"azure_ai/my-claude-deployment",
}

func TestCanonicalModel_UnknownVersionsStayRaw(t *testing.T) {
	for _, raw := range mustStayRaw {
		if got := canonicalModel(raw); got != "" {
			t.Errorf("canonicalModel(%q) = %q, want no family (decision 2: an unknown version is never priced as an older model)", raw, got)
		}
	}
}

// haikuCall is a claude-haiku-5-5 call with the given counts. prompt_tokens
// includes the cache reads and writes, as LiteLLM 1.83.9 reports it.
func haikuCall(id string, prompt, read, w5m, w1h, output int64) StandardLoggingPayload {
	u := &Usage{PromptTokens: prompt, CompletionTokens: output, TotalTokens: prompt + output}
	if read > 0 || w5m+w1h > 0 {
		u.PromptTokensDetails = &PromptTokensDetails{CachedTokens: read}
	}
	if w5m+w1h > 0 {
		u.CacheCreationInputTokens = w5m + w1h
		u.PromptTokensDetails.CacheCreationTokens = w5m + w1h
		u.PromptTokensDetails.CacheCreationTokenDetails = &CacheCreationTokenDetails{
			Ephemeral5mInputTokens: &w5m, Ephemeral1hInputTokens: &w1h,
		}
	}
	return StandardLoggingPayload{
		ID: id, CallType: "acompletion", User: "cus_x",
		Model: "claude-haiku-5-5", CustomLLMProvider: "anthropic", Usage: u,
	}
}

// TestMapPayload_HaikuContextTier pins Claude Haiku 5.5's two prices: a
// prompt (input + cache reads + cache writes) over 100,000 tokens takes the
// higher price, for every token of that call, output included. Exactly
// 100,000 is the lower one ("up to 100,000").
func TestMapPayload_HaikuContextTier(t *testing.T) {
	cases := []struct {
		name string
		p    StandardLoggingPayload
		want string
	}{
		{"exactly 100,000", haikuCall("h1", 100_000, 0, 0, 0, 500), ContextTierUpTo100k},
		{"100,001", haikuCall("h2", 100_001, 0, 0, 0, 500), ContextTierOver100k},
		{"cache reads count", haikuCall("h3", 100_001, 99_001, 0, 0, 500), ContextTierOver100k},
		{"cache writes count", haikuCall("h4", 100_001, 0, 50_000, 49_001, 500), ContextTierOver100k},
		{"99,000 input + 1,000 5m write", haikuCall("h5", 100_000, 0, 1_000, 0, 500), ContextTierUpTo100k},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := MapPayload(tc.p)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if len(m.Events) == 0 {
				t.Fatal("no events")
			}
			// Every event of the call carries the tier, output included, and
			// so does the pre-split mapping kept for a resend.
			for _, e := range append(append([]ExternalIngest{}, m.Events...), m.Legacy...) {
				if e.Dimensions["model"] != "claude-haiku-5.5" {
					t.Errorf("%s: model = %v, want claude-haiku-5.5", e.IdempotencyKey, e.Dimensions["model"])
				}
				if got := e.Dimensions[DimContextTier]; got != tc.want {
					t.Errorf("%s: %s = %v, want %q", e.IdempotencyKey, DimContextTier, got, tc.want)
				}
			}
		})
	}
}

// TestMapPayload_HaikuTierOnOddRecords: a record whose prompt_tokens leaves
// out the cache writes (not_in_prompt), or is smaller than its cache reads,
// still has a prompt of at least the cache reads plus writes. The tier reads
// that, so a long prompt is never billed at the lower price because of how
// the record counted it (review F8).
func TestMapPayload_HaikuTierOnOddRecords(t *testing.T) {
	t.Run("writes not in prompt_tokens", func(t *testing.T) {
		// 1,000 fresh input in prompt_tokens; 150,000 writes reported beside it.
		p := haikuCall("odd1", 1_000, 0, 150_000, 0, 10)
		m, err := MapPayload(p)
		if err != nil {
			t.Fatalf("map: %v", err)
		}
		for _, e := range m.Events {
			if got := e.Dimensions[DimContextTier]; got != ContextTierOver100k {
				t.Errorf("%s: %s = %v, want %q", e.IdempotencyKey, DimContextTier, got, ContextTierOver100k)
			}
		}
	})
	t.Run("cache reads over prompt_tokens", func(t *testing.T) {
		p := haikuCall("odd2", 5_000, 120_000, 0, 0, 10)
		m, err := MapPayload(p)
		if err != nil {
			t.Fatalf("map: %v", err)
		}
		for _, e := range m.Events {
			if got := e.Dimensions[DimContextTier]; got != ContextTierOver100k {
				t.Errorf("%s: %s = %v, want %q", e.IdempotencyKey, DimContextTier, got, ContextTierOver100k)
			}
		}
	})
}

// TestMapPayload_ContextTierOnlyOnTieredModels: other models' events carry no
// context_tier, so no existing rule's match can change.
func TestMapPayload_ContextTierOnlyOnTieredModels(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6", "claude-haiku-4-5", "claude-opus-5-5", "gpt-4o", "claude-opus-4-9"} {
		p := haikuCall("c_"+model, 200_000, 0, 0, 0, 10)
		p.Model = model
		m, err := MapPayload(p)
		if err != nil {
			t.Fatalf("map %s: %v", model, err)
		}
		for _, e := range m.Events {
			if v, ok := e.Dimensions[DimContextTier]; ok {
				t.Errorf("%s: event carries %s=%v, want none", model, DimContextTier, v)
			}
		}
	}
}

// TestMapPayload_UnknownClaudeModelKeepsRawName: decision 2. The event keeps
// the raw name on model and model_raw, and the WARN fires once for the call.
func TestMapPayload_UnknownClaudeModelKeepsRawName(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := haikuCall("u1", 2_000, 0, 0, 0, 100)
	p.Model = "anthropic/claude-opus-4-9"
	m, err := MapPayload(p)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(m.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(m.Events))
	}
	for _, e := range m.Events {
		if e.Dimensions["model"] != "anthropic/claude-opus-4-9" || e.Dimensions["model_raw"] != "anthropic/claude-opus-4-9" {
			t.Errorf("%s: model=%v model_raw=%v, want both the raw name", e.IdempotencyKey, e.Dimensions["model"], e.Dimensions["model_raw"])
		}
	}
	if n := strings.Count(buf.String(), "unknown model"); n != 1 {
		t.Errorf("WARN 'unknown model' logged %d times, want once per call. Log:\n%s", n, buf.String())
	}
}
