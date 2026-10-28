package litellm

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sagarsuperuser/velox/internal/usage"
)

// The litellm-1.83.9-proxy-*.json records were built by LiteLLM 1.83.9's own
// proxy against a mock upstream (testdata/capture_mock_proxy.py): the route
// type that becomes call_type, the end user from the request headers, the
// provider transformations and the record the generic_api callback posts
// are LiteLLM's. Only the upstream HTTP answers are mocked, so these are not
// captures from the providers.
func proxyRecords(t *testing.T, name string) []StandardLoggingPayload {
	t.Helper()
	raw, err := os.ReadFile("testdata/litellm-1.83.9-proxy-" + name + ".json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var out []StandardLoggingPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return out
}

func proxyRecord(t *testing.T, name string) StandardLoggingPayload {
	t.Helper()
	recs := proxyRecords(t, name)
	if len(recs) != 1 {
		t.Fatalf("fixture %s holds %d records, want 1", name, len(recs))
	}
	return recs[0]
}

// inventory is testdata/litellm-1.83.9-call-types.tsv: every call_type
// string LiteLLM 1.83.9 can put on a record (testdata/list_call_types.py),
// with Hardcount's verdict.
func inventory(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("testdata/litellm-1.83.9-call-types.tsv")
	if err != nil {
		t.Fatalf("open inventory: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 4 || (cols[1] != "bill" && cols[1] != "skip") {
			t.Fatalf("bad inventory row %q", line)
		}
		out[cols[0]] = cols[1]
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	return out
}

// TestCallTypeInventory: the skip list is exactly the inventory's "skip"
// rows. A LiteLLM upgrade regenerates the inventory, and this stays red
// until every new call type is classified.
func TestCallTypeInventory(t *testing.T) {
	rows := inventory(t)
	if len(rows) != 254 {
		t.Errorf("inventory has %d call types, want the 254 LiteLLM 1.83.9 can send", len(rows))
	}
	for ct, verdict := range rows {
		_, skip := nonTokenCallTypes[ct]
		switch {
		case verdict == "skip" && !skip:
			t.Errorf("%s: the inventory skips it, the skip list does not", ct)
		case verdict == "bill" && skip:
			t.Errorf("%s: the inventory bills it, the skip list skips it", ct)
		}
	}
	for ct := range nonTokenCallTypes {
		if _, ok := rows[ct]; !ok {
			t.Errorf("%s is on the skip list but not in the inventory", ct)
		}
	}
	// The call types this change starts billing, by name, so a careless
	// edit to the inventory cannot quietly drop one.
	for _, ct := range []string{"anthropic_messages", "responses", "aresponses", "generate_content",
		"agenerate_content", "generate_content_stream", "agenerate_content_stream", "pass_through_endpoint",
		"llm_passthrough_route", "allm_passthrough_route",
		// Google Interactions through the SDK: litellm.interactions.acreate.
		// Vector stores use the same names, and their records carry no
		// tokens, so they skip as no_tokens.
		"acreate", "create"} {
		if rows[ct] != "bill" {
			t.Errorf("%s: verdict %q, want bill", ct, rows[ct])
		}
	}
	for _, ct := range []string{"aimage_generation", "atranscription", "_arealtime", "aretrieve_batch",
		"aget_responses", "asend_message", "asend_message_streaming", "batch_retrieve", "container_file_content",
		// Container-file endpoints, named in containers/endpoints.json, and
		// the SDK functions they become.
		"aretrieve_container_file", "adelete_container_file", "aretrieve_container_file_content",
		"retrieve_container_file", "delete_container_file", "retrieve_container_file_content",
		"endpoint_func", "async_endpoint_func"} {
		if rows[ct] != "skip" {
			t.Errorf("%s: verdict %q, want skip", ct, rows[ct])
		}
	}
}

// TestMapPayload_EveryCallType maps a record carrying tokens for every call
// type in the inventory: a "skip" type gives no events and moves the
// skipped-with-tokens counter under its name; a "bill" type gives events.
func TestMapPayload_EveryCallType(t *testing.T) {
	rows := inventory(t)
	// Not in LiteLLM 1.83.9's source, but on the old allowlist (chat,
	// chat.completion) or simply unknown: each bills on its tokens.
	for _, ct := range []string{"chat", "chat.completion", "", "future_type"} {
		rows[ct] = "bill"
	}
	for ct, verdict := range rows {
		p := StandardLoggingPayload{ID: "call_" + ct, CallType: ct, Model: "gpt-4o", EndUser: "cus_x",
			PromptTokens: 100, CompletionTokens: 50, StartTime: 1792685793.255407}
		before := promtest.ToFloat64(skippedWithTokens.WithLabelValues(ct))
		m, err := MapPayload(p)
		if err != nil {
			t.Errorf("%s: unexpected error %v", ct, err)
			continue
		}
		moved := promtest.ToFloat64(skippedWithTokens.WithLabelValues(ct)) - before
		if verdict == "skip" {
			if len(m.Events) != 0 || m.Skip != SkipNotTokenBearing || moved != 1 {
				t.Errorf("%s (skip): events=%d skip=%q counter moved %v, want 0, %q, 1", ct, len(m.Events), m.Skip, moved, SkipNotTokenBearing)
			}
			continue
		}
		if len(m.Events) != 2 || m.Skip != "" || moved != 0 {
			t.Errorf("%s (bill): events=%d skip=%q counter moved %v, want 2 events, no skip, 0", ct, len(m.Events), m.Skip, moved)
		}
	}
}

// TestMapPayload_CheckOrder pins the order failure, cache hit, skip list,
// no tokens, no user, no id.
func TestMapPayload_CheckOrder(t *testing.T) {
	yes := true
	img := StandardLoggingPayload{ID: "img_1", CallType: "aimage_generation", Model: "gpt-image-1", EndUser: "cus_x",
		PromptTokens: 50, CompletionTokens: 4160}
	counter := func() float64 { return promtest.ToFloat64(skippedWithTokens.WithLabelValues("aimage_generation")) }

	t.Run("a failed image call is a failure, not a skipped call type", func(t *testing.T) {
		p := img
		p.Status = "failure"
		before := counter()
		m, err := MapPayload(p)
		if err != nil || m.Skip != SkipFailure || counter() != before {
			t.Fatalf("skip=%q err=%v counter moved %v, want %q, no error, no move", m.Skip, err, counter()-before, SkipFailure)
		}
	})
	t.Run("a cache-hit image call is a cache hit", func(t *testing.T) {
		p := img
		p.CacheHit = &yes
		before := counter()
		m, err := MapPayload(p)
		if err != nil || m.Skip != SkipCacheHit || counter() != before {
			t.Fatalf("skip=%q err=%v counter moved %v, want %q, no error, no move", m.Skip, err, counter()-before, SkipCacheHit)
		}
	})
	t.Run("a skip-listed record without a user is skipped, not refused", func(t *testing.T) {
		p := img
		p.EndUser = ""
		m, err := MapPayload(p)
		if err != nil || m.Skip != SkipNotTokenBearing {
			t.Fatalf("skip=%q err=%v, want %q and no error", m.Skip, err, SkipNotTokenBearing)
		}
	})
	t.Run("a usage object of zeros on a skip-listed type does not move the counter", func(t *testing.T) {
		p := StandardLoggingPayload{ID: "img_2", CallType: "aimage_generation", EndUser: "cus_x", Usage: &Usage{}}
		before := counter()
		m, err := MapPayload(p)
		if err != nil || m.Skip != SkipNotTokenBearing || counter() != before {
			t.Fatalf("skip=%q err=%v counter moved %v, want %q, no error, no move", m.Skip, err, counter()-before, SkipNotTokenBearing)
		}
	})
	t.Run("a zero-token record without a user is skipped as no tokens, not refused", func(t *testing.T) {
		p := StandardLoggingPayload{ID: "z_1", CallType: "acompletion", Usage: &Usage{}}
		m, err := MapPayload(p)
		if err != nil || m.Skip != SkipNoTokens || len(m.Events) != 0 {
			t.Fatalf("skip=%q err=%v events=%d, want %q, no error, no events", m.Skip, err, len(m.Events), SkipNoTokens)
		}
	})
	t.Run("a vector-store create record (acreate, no tokens) is skipped as no tokens", func(t *testing.T) {
		p := StandardLoggingPayload{ID: "vs_1", CallType: "acreate", EndUser: "cus_x"}
		m, err := MapPayload(p)
		if err != nil || m.Skip != SkipNoTokens || len(m.Events) != 0 {
			t.Fatalf("skip=%q err=%v events=%d, want %q, no error, no events", m.Skip, err, len(m.Events), SkipNoTokens)
		}
	})
	t.Run("a record with tokens and no user is still refused", func(t *testing.T) {
		p := StandardLoggingPayload{ID: "u_1", CallType: "anthropic_messages", PromptTokens: 10}
		if _, err := MapPayload(p); err != ErrMissingUser {
			t.Fatalf("err = %v, want ErrMissingUser", err)
		}
	})
}

// TestMapPayload_VertexLiveSkipped: Vertex AI Live audio arrives through
// pass-through with the text and audio tokens added together. It is skipped
// and flagged (open decision D7). Hand-built record: the route value is from
// source reading (the WebSocket path), not a capture.
func TestMapPayload_VertexLiveSkipped(t *testing.T) {
	const label = "pass_through_endpoint:/vertex_ai/live"
	live := StandardLoggingPayload{ID: "live_1", CallType: "pass_through_endpoint", Model: "gemini-live-2.5-flash",
		EndUser: "cus_x", PromptTokens: 900, CompletionTokens: 300, StartTime: 1792685793.1,
		Metadata: map[string]any{"user_api_key_request_route": "/vertex_ai/live"}}
	before := promtest.ToFloat64(skippedWithTokens.WithLabelValues(label))
	m, err := MapPayload(live)
	if err != nil || m.Skip != SkipNotTokenBearing || len(m.Events) != 0 {
		t.Fatalf("live: skip=%q err=%v events=%d, want %q and no events", m.Skip, err, len(m.Events), SkipNotTokenBearing)
	}
	if d := promtest.ToFloat64(skippedWithTokens.WithLabelValues(label)) - before; d != 1 {
		t.Errorf("counter %s moved %v, want 1", label, d)
	}

	other := live
	other.Metadata = map[string]any{"user_api_key_request_route": "/vertex_ai/v1/projects/p/locations/l/publishers/google/models/gemini-2.5-flash:generateContent"}
	if m, err := MapPayload(other); err != nil || len(m.Events) != 2 {
		t.Fatalf("other Vertex pass-through: events=%d err=%v, want 2 events", len(m.Events), err)
	}
}

// TestMapPayload_KeyShape pins the idempotency keys: <id>:<role>, the id
// being LiteLLM's call id, which is also the request_id its own spend log
// keys on. So a record rebuilt from that spend log, whose times keep only
// milliseconds, gets the keys of the record Hardcount already stored, and the
// replay in docs/ops/runbook/litellm-spend-gap.md fills only the gap. For a
// call type billed only since 2026-10-22, an id too long for the
// 255-character key limit is replaced by its SHA-256.
func TestMapPayload_KeyShape(t *testing.T) {
	keys := func(t *testing.T, p StandardLoggingPayload) []string {
		t.Helper()
		m, err := MapPayload(p)
		if err != nil || len(m.Events) == 0 {
			t.Fatalf("map %s: events=%d err=%v", p.ID, len(m.Events), err)
		}
		var out []string
		for _, e := range m.Events {
			out = append(out, e.IdempotencyKey)
		}
		return out
	}
	base := StandardLoggingPayload{ID: "call_k", Model: "gpt-4o", EndUser: "cus_x", PromptTokens: 10,
		CompletionTokens: 5, StartTime: 1792685793.255407}

	for _, ct := range []string{"completion", "acompletion", "chat", "chat.completion", "embedding",
		"aembedding", "text_completion", "atext_completion", "anthropic_messages", "aresponses",
		"agenerate_content", "pass_through_endpoint"} {
		p := base
		p.CallType = ct
		if got := keys(t, p)[0]; got != "call_k:input" {
			t.Errorf("%s: key %q, want call_k:input", ct, got)
		}
	}

	t.Run("a record rebuilt from LiteLLM's spend log keys like the live record", func(t *testing.T) {
		// LiteLLM_SpendLogs keeps startTime and endTime as TIMESTAMP(3):
		// milliseconds, rounded or cut. The live record has microseconds.
		for _, name := range []string{"messages-split", "messages-stream-delta-cumulative", "responses",
			"responses-stream", "generate-content", "generate-content-stream", "passthrough-messages"} {
			live := proxyRecord(t, name)
			for _, ms := range []func(float64) float64{math.Round, math.Floor} {
				replay := live
				replay.StartTime = ms(live.StartTime*1000) / 1000
				replay.EndTime = ms(live.EndTime*1000) / 1000
				if a, b := keys(t, live), keys(t, replay); strings.Join(a, ",") != strings.Join(b, ",") {
					t.Errorf("%s: the replay keys %v, the live record %v: the replay would bill the call again", name, b, a)
				}
			}
		}
	})
	t.Run("two streamed calls with the same client-chosen id share keys (decision D6: never bill twice)", func(t *testing.T) {
		// LiteLLM 1.83.9 gives both records the client's x-litellm-call-id.
		// Its own spend log keeps only the first (request_id is its primary
		// key). Hardcount does the same: the second deduplicates.
		recs := proxyRecords(t, "messages-same-call-id")
		if len(recs) != 2 || recs[0].ID != recs[1].ID {
			t.Fatalf("fixture: want two records with one id")
		}
		if a, b := keys(t, recs[0]), keys(t, recs[1]); a[0] != b[0] {
			t.Fatalf("keys %q and %q, want one key: LiteLLM's id", a[0], b[0])
		}
	})
	t.Run("two copies of one record dedup", func(t *testing.T) {
		recs := proxyRecords(t, "responses-bridged-logging-hook")
		if len(recs) != 2 {
			t.Fatalf("fixture: want two copies of one call, got %d", len(recs))
		}
		a, b := keys(t, recs[0]), keys(t, recs[1])
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Fatalf("copies key differently: %v vs %v", a, b)
		}
	})
	t.Run("a Responses id over the limit is hashed", func(t *testing.T) {
		p := proxyRecord(t, "responses")
		if utf8.RuneCountInString(p.ID) <= usage.MaxIdempotencyKeyLen {
			t.Fatalf("fixture id is %d characters; the test needs one over the limit", utf8.RuneCountInString(p.ID))
		}
		first := keys(t, p)
		for _, k := range first {
			if utf8.RuneCountInString(k) > usage.MaxIdempotencyKeyLen || !strings.HasPrefix(k, "sha256-") {
				t.Errorf("key %q (%d characters), want a sha256- key within %d", k, utf8.RuneCountInString(k), usage.MaxIdempotencyKeyLen)
			}
		}
		if again := keys(t, p); strings.Join(again, ",") != strings.Join(first, ",") {
			t.Errorf("a resend keys differently: %v vs %v", first, again)
		}
		other := p
		other.ID = p.ID[:len(p.ID)-1] + "x"
		if keys(t, other)[0] == first[0] {
			t.Error("two ids hashed to one key")
		}
	})
	t.Run("the longest id kept as is still fits the longest role", func(t *testing.T) {
		// 255 characters less ":cache_write_5m" is the longest id kept as
		// is; one more character and it is hashed. Either way every key of a
		// call with both cache-write roles fits, so HoldCall accepts it.
		rec := proxyRecord(t, "messages-split")
		limit := usage.MaxIdempotencyKeyLen - len(":"+TokenTypeCacheWrite5m)
		for _, n := range []int{limit, limit + 1} {
			p := rec
			p.ID = strings.Repeat("r", n)
			ks := keys(t, p)
			if len(ks) != 5 {
				t.Fatalf("id of %d: %d keys, want 5 (every role)", n, len(ks))
			}
			for _, k := range ks {
				if utf8.RuneCountInString(k) > usage.MaxIdempotencyKeyLen {
					t.Errorf("id of %d: key of %d characters, over %d", n, utf8.RuneCountInString(k), usage.MaxIdempotencyKeyLen)
				}
			}
			if hashed := strings.HasPrefix(ks[0], "sha256-"); hashed != (n > limit) {
				t.Errorf("id of %d: hashed=%v, want %v", n, hashed, n > limit)
			}
		}
	})
	t.Run("embeddings keep the id LiteLLM sent (open decision D8)", func(t *testing.T) {
		// LiteLLM 1.83.9 gives an embedding record the client's
		// x-litellm-call-id; both captured calls carry it.
		recs := proxyRecords(t, "embedding-same-call-id")
		for _, r := range recs {
			if got := keys(t, r)[0]; got != r.ID+":input" {
				t.Errorf("embedding key %q, want %q", got, r.ID+":input")
			}
		}
	})
}

// TestMapPayload_ProxyFixtures maps every captured proxy record.
func TestMapPayload_ProxyFixtures(t *testing.T) {
	split := map[string]int64{TokenTypeInput: 1000, TokenTypeCacheRead: 5000, TokenTypeCacheWrite5m: 40000,
		TokenTypeCacheWrite1h: 60000, TokenTypeOutput: 500}
	// Writes with no usable split stay inside input: 106,000 − 5,000 reads.
	unsplit := map[string]int64{TokenTypeInput: 101000, TokenTypeCacheRead: 5000, TokenTypeOutput: 500}
	responses := map[string]int64{TokenTypeInput: 500, TokenTypeCacheRead: 1500, TokenTypeOutput: 300}
	cases := []struct {
		fixture, customer, model, provider, unsplitReason string
		want                                              map[string]int64
	}{
		{"messages-split", "cus_f1-messages-split", "claude-sonnet-4.5", "anthropic", "", split},
		{"messages-no-customer-header", "user_claude-code-session-0f3a", "claude-sonnet-4.5", "anthropic", "", split},
		{"messages-stream-delta-output-only", "cus_f2-messages-stream-delta-output-only", "claude-sonnet-4.5", "anthropic", UnsplitNoSplit, unsplit},
		{"messages-stream-delta-cumulative", "cus_f3-messages-stream-delta-cumulative", "claude-sonnet-4.5", "anthropic", UnsplitNoSplit, unsplit},
		{"messages-stream-delta-cumulative-split", "cus_f3b-messages-stream-delta-cumulative-split", "claude-sonnet-4.5", "anthropic", "", split},
		{"responses", "cus_f5-responses", "gpt-5.4", "openai", "", responses},
		{"responses-stream", "cus_f6-responses-stream", "gpt-5.4", "openai", "", responses},
		{"responses-stream-incomplete", "cus_f6b-responses-stream-incomplete", "gpt-5.4", "openai", "", responses},
		{"responses-bridged", "cus_f7-default-00", "claude-sonnet-4.5", "anthropic", "", split},
		{"responses-bridged-post-call-guardrail", "cus_f7-postcall-00", "claude-sonnet-4.5", "anthropic", UnsplitNoCount, unsplit},
		// No Gemini family exists: the raw id stays on model, unpriced.
		{"generate-content", "cus_f8-generate-content", "gemini/gemini-2.5-flash", "gemini", "", responses},
		{"generate-content-stream", "cus_f8b-generate-content-stream", "gemini/gemini-2.5-flash", "vertex_ai", "", responses},
		{"passthrough-messages", "cus_f9-passthrough-messages", "claude-sonnet-4.5", "anthropic", "", split},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			p := proxyRecord(t, tc.fixture)
			var before float64
			if tc.unsplitReason != "" {
				before = promtest.ToFloat64(cacheWriteUnsplit.WithLabelValues(metricProvider(p.CustomLLMProvider), tc.unsplitReason))
			}
			m, err := MapPayload(p)
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			if got := quantities(t, m.Events); !sameQuantities(got, tc.want) {
				t.Fatalf("quantities %v, want %v", got, tc.want)
			}
			for _, e := range m.Events {
				if e.ExternalCustomerID != tc.customer {
					t.Errorf("customer %q, want %q", e.ExternalCustomerID, tc.customer)
				}
				if e.Dimensions["model"] != tc.model || e.Dimensions["model_raw"] != p.Model || e.Dimensions["provider"] != tc.provider {
					t.Errorf("dims model=%v model_raw=%v provider=%v, want %s, %s, %s", e.Dimensions["model"],
						e.Dimensions["model_raw"], e.Dimensions["provider"], tc.model, p.Model, tc.provider)
				}
				if !strings.HasPrefix(e.IdempotencyKey, p.ID+":") && !strings.HasPrefix(e.IdempotencyKey, "sha256-") {
					t.Errorf("key %q, want <id>:<role>", e.IdempotencyKey)
				}
			}
			if tc.unsplitReason != "" {
				if d := promtest.ToFloat64(cacheWriteUnsplit.WithLabelValues(metricProvider(p.CustomLLMProvider), tc.unsplitReason)) - before; d != 1 {
					t.Errorf("unsplit counter %s moved %v, want 1", tc.unsplitReason, d)
				}
			}
			if tc.unsplitReason == UnsplitNoCount {
				// LiteLLM's input cost covers writes Hardcount cannot see; the
				// input event falls to the rate table.
				if c := byRole(t, m.Events)[TokenTypeInput].ObservedCostMicros; c != nil {
					t.Errorf("input observed cost %d, want none (the rate table prices it)", *c)
				}
			}
		})
	}

	t.Run("failure", func(t *testing.T) {
		m, err := MapPayload(proxyRecord(t, "messages-failure"))
		if err != nil || m.Skip != SkipFailure {
			t.Fatalf("skip=%q err=%v, want %q", m.Skip, err, SkipFailure)
		}
	})
	t.Run("image generation carries tokens and is skipped loudly", func(t *testing.T) {
		p := proxyRecord(t, "image-generation")
		before := promtest.ToFloat64(skippedWithTokens.WithLabelValues(p.CallType))
		m, err := MapPayload(p)
		if err != nil || m.Skip != SkipNotTokenBearing || len(m.Events) != 0 {
			t.Fatalf("skip=%q err=%v events=%d, want %q and no events", m.Skip, err, len(m.Events), SkipNotTokenBearing)
		}
		if d := promtest.ToFloat64(skippedWithTokens.WithLabelValues(p.CallType)) - before; d != 1 {
			t.Errorf("counter for %s moved %v, want 1", p.CallType, d)
		}
	})
	t.Run("a record with no write cost does not count as hidden writes", func(t *testing.T) {
		p := proxyRecord(t, "responses")
		before := promtest.ToFloat64(cacheWriteUnsplit.WithLabelValues("openai", UnsplitNoCount))
		if _, err := MapPayload(p); err != nil {
			t.Fatal(err)
		}
		if d := promtest.ToFloat64(cacheWriteUnsplit.WithLabelValues("openai", UnsplitNoCount)) - before; d != 0 {
			t.Errorf("no_count moved %v on a call without cache writes", d)
		}
	})
}

// TestSpend_SkippedRecordWithTokensIsLogged: a batch of a Claude Code
// /v1/messages call and a gpt-image-1 call. The Messages call bills five
// events; the image call is skipped, and the WARN names its call type.
func TestSpend_SkippedRecordWithTokensIsLogged(t *testing.T) {
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ing := &fakeIngester{}
	h := New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{})
	code, body := postSpend(t, h, proxyRecord(t, "messages-split"), proxyRecord(t, "image-generation"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	var r SpendResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Accepted != 5 || r.Skipped != 1 || len(r.Errors) != 0 {
		t.Fatalf("response %+v, want 5 accepted and 1 skipped", r)
	}
	out := logs.String()
	if !strings.Contains(out, "call_type=aimage_generation") || !strings.Contains(out, "hardcount_litellm_skipped_with_tokens_total") {
		t.Fatalf("no WARN naming the skipped call type; log:\n%s", out)
	}
}

// TestMapPayload_ClaudeOnNewCallTypes: Claude Code on Google Cloud sends
// /v1/messages with model names such as vertex_ai/claude-opus-4-5@20251101.
// Each must reach its own model, never an older one (Opus 4 is 3 times the
// Opus 4.5 price), and a version Hardcount does not know keeps its raw name and
// is unpriced (decision 2 of 2026-10-22). The expected values agree with
// #1351's golden list (testdata/litellm-1.83.9-claude-model-ids.tsv there).
func TestMapPayload_ClaudeOnNewCallTypes(t *testing.T) {
	cases := map[string]string{
		"vertex_ai/claude-opus-4-5@20251101":            "claude-opus-4.5",
		"vertex_ai/claude-opus-4-6@default":             "claude-opus-4.6",
		"vertex_ai/claude-sonnet-4-5@20250929":          "claude-sonnet-4.5",
		"vertex_ai/claude-opus-4-1@20250805":            "claude-opus-4.1",
		"vertex_ai/claude-opus-4@20250514":              "claude-opus-4",
		"vertex_ai/claude-3-5-sonnet-v2@20241022":       "claude-3.5-sonnet",
		"claude-opus-4-0":                               "claude-opus-4",
		"anthropic.claude-3-5-sonnet-20241022-v2:0:51k": "claude-3.5-sonnet",
		"claude-sonnet-4-5-20250929":                    "claude-sonnet-4.5",
		// Unknown versions: raw name, no price.
		"claude-opus-4-9":                    "",
		"vertex_ai/claude-opus-4-9@20270101": "",
		"claude-sonnet-4-5-thinking":         "",
	}
	for model, want := range cases {
		if got := canonicalModel(model); got != want {
			t.Errorf("canonicalModel(%q) = %q, want %q", model, got, want)
		}
		p := StandardLoggingPayload{ID: "msg_1", CallType: "anthropic_messages", Model: model, EndUser: "cus_x",
			PromptTokens: 10, CompletionTokens: 5}
		m, err := MapPayload(p)
		if err != nil || len(m.Events) == 0 {
			t.Fatalf("%s: events=%d err=%v", model, len(m.Events), err)
		}
		dim := want
		if dim == "" {
			dim = model
		}
		for _, e := range m.Events {
			if e.Dimensions["model"] != dim {
				t.Errorf("%s: model dimension %v, want %q", model, e.Dimensions["model"], dim)
			}
		}
	}
}
