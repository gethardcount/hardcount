package litellm

import (
	"encoding/json"
	"os"
	"testing"
)

// TestMapPayload_RealProxyRecord maps a record captured from a real LiteLLM
// proxy (v1.83.9, the generic_api callback configured exactly as
// docs/integrations/litellm.md says, one chat completion sent with
// user="cus_acme_corp" and metadata.team_id). The other tests in this package
// build payloads by hand; this one pins the adapter to what the proxy sends.
//
// A real record has no "user" field and no "usage" object: the caller's user
// arrives as "end_user", and the token counts are top-level, with the full
// usage object under metadata.usage_object. Before this test the adapter read
// only "user" and "usage", so every real record was refused as "payload.user
// is required" inside a 200 response and nothing was recorded.
func TestMapPayload_RealProxyRecord(t *testing.T) {
	raw, err := os.ReadFile("testdata/litellm-1.83.9-generic-api.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var batch []StandardLoggingPayload
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("fixture holds %d records, want 1", len(batch))
	}

	out, err := MapPayload(batch[0])
	if err != nil {
		t.Fatalf("a real proxy record was refused: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d events, want 2 (input and output)", len(out))
	}
	in, outE := out[0], out[1]

	if in.ExternalCustomerID != "cus_acme_corp" || outE.ExternalCustomerID != "cus_acme_corp" {
		t.Errorf("customer: got %q and %q, want cus_acme_corp on both", in.ExternalCustomerID, outE.ExternalCustomerID)
	}
	if in.Dimensions["token_type"] != TokenTypeInput || in.Quantity.IntPart() != 10 {
		t.Errorf("input event: token_type=%v quantity=%s, want input and 10", in.Dimensions["token_type"], in.Quantity)
	}
	if outE.Dimensions["token_type"] != TokenTypeOutput || outE.Quantity.IntPart() != 20 {
		t.Errorf("output event: token_type=%v quantity=%s, want output and 20", outE.Dimensions["token_type"], outE.Quantity)
	}
	// The proxy prefixes the provider ("anthropic/claude-sonnet-4-5-20250929").
	if in.Dimensions["model"] != "claude-sonnet-4.5" {
		t.Errorf("model: got %v, want the recipe family claude-sonnet-4.5", in.Dimensions["model"])
	}
	if in.Dimensions["team_id"] != "team_engineering" {
		t.Errorf("team_id: got %v, want team_engineering", in.Dimensions["team_id"])
	}
	if in.IdempotencyKey == "" || in.IdempotencyKey == outE.IdempotencyKey {
		t.Errorf("idempotency keys: %q and %q, want two distinct non-empty keys", in.IdempotencyKey, outE.IdempotencyKey)
	}
	if in.Timestamp == nil {
		t.Error("timestamp: nil, want the call's end time")
	}
}

// TestCallerIDAndTokenUsage_Sources pins which field wins when a record
// carries the caller or the token counts in more than one place.
func TestCallerIDAndTokenUsage_Sources(t *testing.T) {
	t.Run("caller: user, then end_user, then the metadata copy", func(t *testing.T) {
		cases := []struct {
			name string
			p    StandardLoggingPayload
			want string
		}{
			{"user wins", StandardLoggingPayload{User: "a", EndUser: "b"}, "a"},
			{"end_user when user is blank", StandardLoggingPayload{User: "  ", EndUser: "b"}, "b"},
			{"metadata copy last", StandardLoggingPayload{Metadata: map[string]any{"user_api_key_end_user_id": "c"}}, "c"},
			{"a non-string metadata value is ignored", StandardLoggingPayload{Metadata: map[string]any{"user_api_key_end_user_id": 7}}, ""},
			{"none", StandardLoggingPayload{}, ""},
		}
		for _, tc := range cases {
			if got := tc.p.callerID(); got != tc.want {
				t.Errorf("%s: callerID = %q, want %q", tc.name, got, tc.want)
			}
		}
	})

	t.Run("no caller anywhere is still refused", func(t *testing.T) {
		_, err := MapPayload(StandardLoggingPayload{CallType: "acompletion", PromptTokens: 5})
		if err != ErrMissingUser {
			t.Fatalf("err = %v, want ErrMissingUser", err)
		}
	})

	t.Run("cache counts come from metadata.usage_object", func(t *testing.T) {
		// A real record for a cached Anthropic call: top-level counts carry no
		// cache detail, metadata.usage_object does. prompt_tokens includes
		// the cached part, so the uncached input is 1000 - 600.
		p := StandardLoggingPayload{
			ID: "call_cache", CallType: "acompletion", Model: "anthropic/claude-sonnet-4-5-20250929",
			EndUser: "cus_acme_corp", PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050,
			Metadata: map[string]any{"usage_object": map[string]any{
				"prompt_tokens": float64(1000), "completion_tokens": float64(50), "total_tokens": float64(1050),
				"prompt_tokens_details": map[string]any{"cached_tokens": float64(600)},
			}},
		}
		out, err := MapPayload(p)
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		got := map[string]int64{}
		for _, e := range out {
			got[e.Dimensions["token_type"].(string)] = e.Quantity.IntPart()
		}
		want := map[string]int64{TokenTypeInput: 400, TokenTypeCacheRead: 600, TokenTypeOutput: 50}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("events = %v, want exactly %v", got, want)
		}
	})

	t.Run("top-level counts alone are enough", func(t *testing.T) {
		p := StandardLoggingPayload{ID: "call_top", CallType: "acompletion", Model: "gpt-4o", EndUser: "cus_x", PromptTokens: 7, CompletionTokens: 3}
		out, err := MapPayload(p)
		if err != nil || len(out) != 2 {
			t.Fatalf("out=%d err=%v, want 2 events and no error", len(out), err)
		}
		if out[0].Quantity.IntPart() != 7 || out[1].Quantity.IntPart() != 3 {
			t.Errorf("quantities %s and %s, want 7 and 3", out[0].Quantity, out[1].Quantity)
		}
	})

	t.Run("an explicit usage object still wins", func(t *testing.T) {
		p := StandardLoggingPayload{PromptTokens: 99, Usage: &Usage{PromptTokens: 1, CompletionTokens: 2}}
		if u := p.tokenUsage(); u == nil || u.PromptTokens != 1 {
			t.Errorf("tokenUsage = %+v, want the explicit usage object", u)
		}
	})

	t.Run("no counts anywhere maps to nothing", func(t *testing.T) {
		out, err := MapPayload(StandardLoggingPayload{CallType: "acompletion", EndUser: "cus_x"})
		if err != nil || len(out) != 0 {
			t.Errorf("out=%d err=%v, want no events and no error", len(out), err)
		}
	})
}
