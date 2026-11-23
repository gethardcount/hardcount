package api

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/integrations/litellm"
)

// TestE2E_TestClock_AnAdvancingClocksCustomersCannotBeChanged: while a test
// clock advances, a request that changes one of its customers answers 409
// clock_advancing and changes nothing, on a record route and on one that
// names the customer in its body. A send of many records refuses only that
// customer's: the usage batch names it among its refused events, and the
// LiteLLM spend callback, whose proxy never sends a 4xx again, lists it in
// errors[] and saves the other calls. Reads go on, and deleting the clock
// stops the advance.
func TestE2E_TestClock_AnAdvancingClocksCustomersCannotBeChanged(t *testing.T) {
	r := newDatesRig(t, "Clock Advancing")
	start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	clockID := r.newClock("advancing", start)
	subID, _ := r.subWithItem("on-clock", clockID, r.planID)
	r.subWithItem("off-clock", "", r.planID)
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
	event := func(customer, key string) map[string]any {
		return map[string]any{"external_customer_id": "dates-" + customer, "event_name": litellm.EventNameTokens, "quantity": 1, "idempotency_key": key}
	}
	refused("usage event", r.call("POST", "/v1/usage-events", flowReq{body: event("on-clock", "advancing-1")}))

	batch := r.call("POST", "/v1/usage-events/batch", flowReq{body: []map[string]any{event("on-clock", "advancing-2"), event("off-clock", "advancing-3")}})
	if batch.status != 422 || batch.errCode() != "batch_rejected" || !slices.Contains(flowStrings(batch.body, "errors"), "event[0]: "+refusal) {
		t.Errorf("usage batch: %d %s, want 422 batch_rejected naming event[0]: %s", batch.status, batch.raw, refusal)
	}

	var calls []map[string]any
	for _, customer := range []string{"on-clock", "off-clock"} {
		var one []map[string]any
		if err := json.Unmarshal(e2eLiteLLMBatch(t, 1, "dates-"+customer, 0), &one); err != nil {
			t.Fatalf("decode LiteLLM record: %v", err)
		}
		one[0]["id"] = "chatcmpl-" + customer
		calls = append(calls, one[0])
	}
	spend := r.call("POST", "/v1/integrations/litellm/spend", flowReq{body: calls}).want(t, 200, "LiteLLM spend")
	rowErrs, _ := spend.body["errors"].([]any)
	if flowInt(spend.body, "accepted") != 2 || len(rowErrs) != 1 ||
		flowStr(rowErrs[0].(map[string]any), "id") != "chatcmpl-on-clock" || flowStr(rowErrs[0].(map[string]any), "error") != refusal {
		t.Errorf("LiteLLM spend: %s, want the off-clock call accepted (2 rows) and the on-clock call in errors[]: %s", spend.raw, refusal)
	}

	r.call("DELETE", "/v1/test-clocks/"+clockID, flowReq{}).want(t, 200, "delete the advancing clock")
}
