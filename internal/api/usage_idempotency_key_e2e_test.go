package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/integrations/litellm"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestUsageIngest_IdempotencyKeyRequired_E2E runs #980 through the real
// server and real Postgres. Before, an event with no idempotency_key was
// stored, and a client that missed the response and sent it again stored it
// twice. Now the three ingest routes refuse it with 422 and write nothing.
// The Idempotency-Key header does not stand in for the body key: it caches
// one HTTP response for 24 hours, while the body key dedups each event for
// good. The LiteLLM door sets its own keys and keeps ingesting.
//
// Mutation-verify: delete the idempotency_key check in usage.resolve → the
// keyless posts answer 201 and the row count goes up.
func TestUsageIngest_IdempotencyKeyRequired_E2E(t *testing.T) {
	skipShort(t)
	db := testutil.SetupTestDB(t)
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	ts := httptest.NewServer(NewServer(db, clk, nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Usage Key Required")
	bearer := "Bearer " + createTestAPIKey(t, db, tenantID)

	post := func(path, idemHeader string, body any) (int, http.Header, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", bearer)
		if idemHeader != "" {
			req.Header.Set("Idempotency-Key", idemHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, resp.Header, out
	}
	rows := func() int {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
			t.Fatalf("count usage_events: %v", err)
		}
		return n
	}
	mustCreate := func(path string, body map[string]any) {
		t.Helper()
		if code, _, out := post(path, "", body); code != http.StatusCreated {
			t.Fatalf("POST %s: status %d body %v", path, code, out)
		}
	}

	mustCreate("/v1/customers", map[string]any{"external_id": "cus_key", "display_name": "Key Customer"})
	mustCreate("/v1/meters", map[string]any{"key": "api_calls", "name": "API Calls", "unit": "calls", "aggregation": "sum"})

	event := func(key any) map[string]any {
		e := map[string]any{"external_customer_id": "cus_key", "event_name": "api_calls", "quantity": 1}
		if key != nil {
			e["idempotency_key"] = key
		}
		return e
	}
	refused := func(name string, code int, out map[string]any) {
		t.Helper()
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d body %v, want 422", name, code, out)
		}
	}

	// Header-only: the shape the old docs/ai-pricing.md example used.
	code, _, out := post("/v1/usage-events", "hdr-only-1", event(nil))
	refused("single, header only", code, out)
	if errObj, _ := out["error"].(map[string]any); errObj["param"] != "idempotency_key" {
		t.Errorf("single, header only: error %v, want param idempotency_key", out["error"])
	}
	code, _, out = post("/v1/usage-events", "", event("   "))
	refused("single, whitespace key", code, out)

	code, _, out = post("/v1/usage-events/batch", "hdr-only-2", []any{event("batch-a"), event(nil), event("batch-c")})
	refused("batch, one keyless index", code, out)
	if errsList, _ := out["errors"].([]any); len(errsList) != 1 || errsList[0] != "event[1]: idempotency_key is required — give each event its own key and send the same key when you retry that event" {
		t.Errorf("batch errors = %v, want only event[1] naming idempotency_key", out["errors"])
	}

	backfill := event(nil)
	backfill["timestamp"] = "2026-03-01T00:00:00Z"
	code, _, out = post("/v1/usage-events/backfill", "hdr-only-3", backfill)
	refused("backfill, header only", code, out)

	if n := rows(); n != 0 {
		t.Fatalf("usage_events rows after refused posts = %d, want 0", n)
	}

	// Control: with a key, the event lands once and a resend replays it.
	if code, _, out = post("/v1/usage-events", "", event("evt-1")); code != http.StatusCreated {
		t.Fatalf("keyed single: status %d body %v", code, out)
	}
	code, hdr, out := post("/v1/usage-events", "", event("evt-1"))
	if code != http.StatusOK || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("keyed resend: status %d replayed=%q body %v, want 200 replayed", code, hdr.Get("Idempotent-Replayed"), out)
	}
	batch := []any{event("evt-2"), event("evt-3")}
	if code, _, out = post("/v1/usage-events/batch", "", batch); code != http.StatusCreated || out["ingested"] != float64(2) {
		t.Fatalf("keyed batch: status %d body %v, want 201 ingested 2", code, out)
	}
	if code, _, out = post("/v1/usage-events/batch", "", batch); code != http.StatusCreated || out["deduplicated"] != float64(2) {
		t.Fatalf("keyed batch resend: status %d body %v, want 201 deduplicated 2", code, out)
	}
	if n := rows(); n != 3 {
		t.Fatalf("usage_events rows after keyed posts = %d, want 3", n)
	}

	// The LiteLLM door builds its own keys and is not affected.
	mustCreate("/v1/meters", map[string]any{"key": litellm.MeterKeyTokens, "name": "Tokens", "unit": "tokens", "aggregation": "sum"})
	spend := map[string]any{"events": []litellm.StandardLoggingPayload{{
		ID: "chatcmpl-key-1", CallType: "completion", Model: "claude-3-5-sonnet-20241022",
		CustomLLMProvider: "anthropic", User: "cus_key",
		Usage:     &litellm.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		StartTime: float64(now.Unix()), EndTime: float64(now.Unix()) + 1,
	}}}
	if code, _, out = post("/v1/integrations/litellm/spend", "", spend); code != http.StatusOK {
		t.Fatalf("litellm spend: status %d body %v", code, out)
	}
	if n := rows(); n != 5 {
		t.Fatalf("usage_events rows after LiteLLM spend = %d, want 5 (input + output)", n)
	}
}
