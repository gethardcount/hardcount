package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/integrations/litellm"
)

// TestE2E_TestClock_AnAdvancingClocksCustomersCannotBeChanged: while a test
// clock advances, a request that changes one of its customers answers 409
// clock_advancing and changes nothing, on a record route and on one that
// names the customer in its body (an operator's usage backfill). Usage an app
// or a proxy sends for the customer is accepted: one event, a batch, and a
// LiteLLM spend send. Reads go on, and deleting the clock stops the advance.
func TestE2E_TestClock_AnAdvancingClocksCustomersCannotBeChanged(t *testing.T) {
	r := newDatesRig(t, "Clock Advancing")
	start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	clockID := r.newClock("advancing", start)
	subID, _ := r.subWithItem("on-clock", clockID, r.planID)
	r.call("POST", "/v1/meters", flowReq{body: map[string]any{
		"event_name": litellm.EventNameTokens, "name": "Tokens", "unit": "tokens", "aggregation": "sum",
	}}).want(t, 201, "create tokens meter")
	// The advance is marked and its catch-up does not run, so the clock stays
	// advancing.
	r.call("POST", "/v1/test-clocks/"+clockID+"/advance", flowReq{body: map[string]any{
		"frozen_time": start.AddDate(0, 1, 0).Format(time.RFC3339),
	}}).want(t, 200, "advance clock")
	const refusal = `Test clock "advancing" is advancing, so its customers cannot be changed until it is ready. Try again then.`
	refused := func(what string, res flowResp) {
		t.Helper()
		if res.status != 409 || res.errCode() != "clock_advancing" || flowStr(res.body, "error", "message") != refusal {
			t.Errorf("%s: %d %s, want 409 clock_advancing: %s", what, res.status, res.raw, refusal)
		}
	}

	refused("cancel", r.call("POST", "/v1/subscriptions/"+subID+"/cancel", flowReq{}))
	sub := r.call("GET", "/v1/subscriptions/"+subID, flowReq{}).want(t, 200, "read subscription").body
	if flowStr(sub, "status") != "active" {
		t.Errorf("subscription status %v after the refused cancel, want active", sub["status"])
	}
	event := func(key string) map[string]any {
		return map[string]any{"external_customer_id": "dates-on-clock", "event_name": litellm.EventNameTokens, "quantity": 1, "idempotency_key": key}
	}
	backfill := event("advancing-backfill")
	backfill["timestamp"] = start.Format(time.RFC3339)
	refused("usage backfill", r.call("POST", "/v1/usage-events/backfill", flowReq{body: backfill}))

	// Sent while the clock stands behind its target, it is dated at the
	// target: it takes effect after the advance.
	target := start.AddDate(0, 1, 0)
	if ev := r.call("POST", "/v1/usage-events", flowReq{body: event("advancing-1")}).want(t, 201, "a usage event").body; !at(t, ev, "timestamp").Equal(target) {
		t.Errorf("usage event timestamp %v, want the advance's target %v", ev["timestamp"], target)
	}
	r.call("POST", "/v1/usage-events/batch", flowReq{body: []map[string]any{event("advancing-2"), event("advancing-3")}}).want(t, 201, "a usage batch")
	var call []map[string]any
	if err := json.Unmarshal(e2eLiteLLMBatch(t, 1, "dates-on-clock", 0), &call); err != nil {
		t.Fatalf("decode LiteLLM record: %v", err)
	}
	spend := r.call("POST", "/v1/integrations/litellm/spend", flowReq{body: call}).want(t, 200, "LiteLLM spend")
	if rowErrs, _ := spend.body["errors"].([]any); flowInt(spend.body, "accepted") != 2 || len(rowErrs) != 0 {
		t.Errorf("LiteLLM spend: %s, want the call's two rows accepted", spend.raw)
	}

	r.call("DELETE", "/v1/test-clocks/"+clockID, flowReq{}).want(t, 200, "delete the advancing clock")
}
