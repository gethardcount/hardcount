package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_IngestKey_SendsUsageAndNothingElse walks an ingest key (ADR-133)
// over the real router and database, the way a LiteLLM proxy uses one. The
// owner creates it from the dashboard session; it sends usage through all
// three ingest routes and the events land; every other call it makes gets
// the ordinary missing-permission 403 and changes nothing, including a
// refund, a void and minting a key. A rotation keeps its type.
//
// Mutation: give the ingest key usage:write or customer:read, or gate the
// LiteLLM callback on usage:write again, and this fails.
func TestE2E_IngestKey_SendsUsageAndNothingElse(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "ingest-key-flow-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@ingest-key-flow.test", "ingest-key-flow-pw-12"
	boot := flowBootstrap(t, ts, "ingest-key-flow-bootstrap", email, password)
	tenantID := flowStr(boot, "tenant", "id")
	owner := login(t, ts, email, password)
	asOwner := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{cookie: owner, body: body})
	}

	cust := asOwner("POST", "/v1/customers", map[string]any{"external_id": "acme-ingest", "display_name": "Acme"}).want(t, 201, "create customer")
	customerID := flowStr(cust.body, "id")
	asOwner("POST", "/v1/meters", map[string]any{"event_name": "api_calls", "name": "API calls", "unit": "calls", "aggregation": "sum"}).want(t, 201, "create meter")
	asOwner("POST", "/v1/meters", map[string]any{"event_name": litellm.EventNameTokens, "name": "Tokens", "unit": "tokens", "aggregation": "sum"}).want(t, 201, "create tokens meter")

	// Create: the dashboard asks for key_type ingest and gets an
	// hc_ingest_test_ key in the session's mode.
	created := asOwner("POST", "/v1/api-keys", map[string]any{"name": "LiteLLM proxy", "key_type": "ingest"}).want(t, 201, "create an ingest key")
	keyID, raw := flowStr(created.body, "key", "id"), flowStr(created.body, "raw_key")
	if !strings.HasPrefix(raw, "hc_ingest_test_") || flowStr(created.body, "key", "key_type") != "ingest" || flowObj(created.body, "key")["livemode"] != false {
		t.Fatalf("created key %v (raw key starts %q), want a test-mode ingest key starting hc_ingest_test_", flowObj(created.body, "key"), raw[:min(len(raw), 15)])
	}
	listed := false
	for _, k := range flowList(asOwner("GET", "/v1/api-keys", nil).want(t, 200, "list keys").body, "data") {
		if flowStr(k, "id") == keyID {
			listed = flowStr(k, "key_type") == "ingest"
		}
	}
	if !listed {
		t.Errorf("key %s is not listed with key_type ingest", keyID)
	}
	as := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: raw, body: body})
	}

	// whoami needs no permission: it names the key, its type and account.
	who := as("GET", "/v1/whoami", nil).want(t, 200, "whoami with the ingest key").body
	if flowStr(who, "key_id") != keyID || flowStr(who, "key_type") != "ingest" || flowStr(who, "tenant_id") != tenantID {
		t.Errorf("whoami = %v, want key %s, type ingest, account %s", who, keyID, tenantID)
	}

	// Send usage: single, batch and the LiteLLM callback.
	event := func(key string) map[string]any {
		return map[string]any{"external_customer_id": "acme-ingest", "event_name": "api_calls", "quantity": 1, "idempotency_key": key}
	}
	as("POST", "/v1/usage-events", event("ingest-1")).want(t, 201, "single event with the ingest key")
	batch := as("POST", "/v1/usage-events/batch", []any{event("ingest-2"), event("ingest-3")}).want(t, 201, "batch with the ingest key")
	if flowInt(batch.body, "ingested") != 2 {
		t.Errorf("batch answer %s, want ingested 2", batch.raw)
	}
	now := time.Now().UTC()
	spend := as("POST", "/v1/integrations/litellm/spend", map[string]any{"events": []litellm.StandardLoggingPayload{{
		ID: "chatcmpl-ingest-key-1", CallType: "completion", Model: "claude-3-5-sonnet-20241022",
		CustomLLMProvider: "anthropic", User: "acme-ingest",
		Usage:     &litellm.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		StartTime: float64(now.Unix()), EndTime: float64(now.Unix()) + 1,
	}}}).want(t, 200, "LiteLLM callback with the ingest key")
	if flowInt(spend.body, "accepted") != 2 {
		t.Errorf("LiteLLM answer %s, want accepted 2 (input and output)", spend.raw)
	}
	events := asOwner("GET", "/v1/usage-events?customer_id="+customerID+"&limit=100", nil).want(t, 200, "owner lists usage")
	if got := len(flowList(events.body, "data")); got != 5 {
		t.Errorf("the owner sees %d usage events, want 5 (1 single, 2 batch, 2 from LiteLLM): %s", got, events.raw)
	}

	// Everything else: the ordinary missing-permission 403, and nothing
	// changes.
	finalInvoice := "hc_inv_ingest_probe"
	for _, c := range []struct {
		method, path string
		body         any
		perm         string
	}{
		{"GET", "/v1/customers", nil, "customer:read"},
		{"POST", "/v1/customers", map[string]any{"external_id": "sneaky", "display_name": "Sneaky"}, "customer:write"},
		{"GET", "/v1/usage-events", nil, "usage:read"},
		{"POST", "/v1/usage-events/backfill", event("ingest-backfill"), "usage:write"},
		{"POST", "/v1/usage-events/held/release", map[string]any{"external_customer_id": "acme-ingest"}, "usage:write"},
		{"POST", "/v1/invoices/" + finalInvoice + "/void", nil, "invoice:write"},
		{"POST", "/v1/credit-notes", map[string]any{"invoice_id": finalInvoice, "reason": "duplicate", "refund_type": "refund"}, "invoice:write"},
		{"POST", "/v1/api-keys", map[string]any{"name": "escalation", "key_type": "secret"}, "apikey:write"},
		{"POST", "/v1/api-keys/" + keyID + "/rotate", nil, "apikey:write"},
		{"DELETE", "/v1/api-keys/" + keyID, nil, "apikey:write"},
		{"GET", "/v1/settings", nil, "apikey:write"},
		{"POST", "/v1/billing/run", nil, "invoice:write"},
		{"GET", "/v1/exports/customers.csv", nil, "customer:read"},
		{"GET", "/v1/webhook_events/stream", nil, "apikey:read"},
	} {
		got := as(c.method, c.path, c.body)
		want := "insufficient permissions: this key type does not have " + c.perm + " access"
		if got.status != 403 || got.errCode() != "forbidden" || got.errMessage() != want {
			t.Errorf("%s %s with the ingest key: %d %s, want 403 forbidden %q", c.method, c.path, got.status, got.raw, want)
		}
	}
	custs := asOwner("GET", "/v1/customers", nil).want(t, 200, "owner lists customers")
	if n := len(flowList(custs.body, "data")); n != 1 {
		t.Errorf("the owner sees %d customers, want 1: the refused create wrote one", n)
	}
	keys := asOwner("GET", "/v1/api-keys", nil).want(t, 200, "owner lists keys")
	for _, k := range flowList(keys.body, "data") {
		if flowStr(k, "name") == "escalation" {
			t.Errorf("the ingest key minted a key: %v", k)
		}
		if flowStr(k, "id") == keyID && flowStr(k, "revoked_at") != "" {
			t.Error("the ingest key revoked itself")
		}
	}

	// A rotation by the owner keeps the type.
	rot := asOwner("POST", "/v1/api-keys/"+keyID+"/rotate", nil).want(t, 200, "owner rotates the ingest key")
	if newRaw := flowStr(rot.body, "raw_key"); !strings.HasPrefix(newRaw, "hc_ingest_test_") || flowStr(rot.body, "new_key", "key_type") != "ingest" {
		t.Errorf("rotation gave %v, want an ingest key", flowObj(rot.body, "new_key"))
	}
}
