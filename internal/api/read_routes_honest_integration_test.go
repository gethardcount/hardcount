package api

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestReadRoutesHonest drives the read routes from the 2026-10-21 sweep
// (D-06, D-07, D-10, D-11, D-14) through the real router and database. Each
// case pairs the wrong input with a control that must still answer 200, so
// a check that refuses everything cannot pass.
func TestReadRoutesHonest(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Read Routes Honest Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}

	ruleID := flowStr(call("POST", "/v1/rating-rules", map[string]any{
		"rule_key": "honest_calls", "name": "Honest calls", "mode": "per_unit",
		"currency": "USD", "unit_amount_cents": 10,
	}).want(t, 201, "create rating rule").body, "id")
	meterID := flowStr(call("POST", "/v1/meters", map[string]any{
		"event_name": "honest_calls", "name": "Honest calls", "unit": "call",
		"aggregation": "sum", "rating_rule_version_id": ruleID,
	}).want(t, 201, "create meter").body, "id")
	planID := flowStr(call("POST", "/v1/plans", map[string]any{
		"code": "honest-plan", "name": "Honest Plan", "currency": "USD",
		"billing_interval": "monthly", "base_amount_cents": 1000,
		"base_bill_timing": "in_arrears", "meter_ids": []string{meterID},
	}).want(t, 201, "create plan").body, "id")
	// One instant for the clock and for "today" below: an event with no
	// timestamp lands at the clock's frozen_time, so a second time.Now()
	// would name the next day if midnight UTC fell between the two.
	frozen := time.Now().UTC()
	clockID := flowStr(call("POST", "/v1/test-clocks", map[string]any{
		"name": "honest clock", "frozen_time": frozen.Format(time.RFC3339),
	}).want(t, 201, "create test clock").body, "id")
	customerID := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "honest-cust", "display_name": "Honest Customer",
		"email": "billing@honest.test", "test_clock_id": clockID,
	}).want(t, 201, "create customer").body, "id")
	sub := call("POST", "/v1/subscriptions", map[string]any{
		"code": "honest-sub", "display_name": "Honest Subscription", "customer_id": customerID,
		"items":        []map[string]any{{"plan_id": planID, "quantity": 1}},
		"billing_time": "anniversary",
	}).want(t, 201, "create subscription").body
	subID := flowStr(sub, "id")
	call("POST", "/v1/usage-events", map[string]any{
		"external_customer_id": "honest-cust", "event_name": "honest_calls",
		"quantity": 3, "idempotency_key": "honest-1",
	}).want(t, 201, "ingest usage event")

	// D-06: a read route keyed by a customer or meter id answers 404 for an
	// id that does not exist, not 200 with zeros or an empty list.
	t.Run("unknown id is 404", func(t *testing.T) {
		const missing = "vlx_cus_does_not_exist"
		routes := []struct{ unknown, known string }{
			{"/v1/credits/balance/" + missing, "/v1/credits/balance/" + customerID},
			{"/v1/credits/ledger/" + missing, "/v1/credits/ledger/" + customerID},
			{"/v1/credits/grants/" + missing, "/v1/credits/grants/" + customerID},
			{"/v1/customers/" + missing + "/sent-emails", "/v1/customers/" + customerID + "/sent-emails"},
			{"/v1/customers/" + missing + "/margin", "/v1/customers/" + customerID + "/margin"},
			{"/v1/meters/vlx_mtr_does_not_exist/pricing-rules", "/v1/meters/" + meterID + "/pricing-rules"},
		}
		for _, r := range routes {
			if got := call("GET", r.unknown, nil); got.status != 404 {
				t.Errorf("GET %s = %d, want 404 (body %s)", r.unknown, got.status, got.raw)
			} else if code := got.errCode(); code != "not_found" {
				t.Errorf("GET %s error code = %q, want not_found", r.unknown, code)
			}
			if got := call("GET", r.known, nil); got.status != 200 {
				t.Errorf("GET %s = %d, want 200 for an id that exists (body %s)", r.known, got.status, got.raw)
			}
		}
	})

	// D-10: the usage list and aggregate take RFC 3339 or YYYY-MM-DD and
	// refuse anything else. A plain date used to be dropped, so the totals
	// came back for all time.
	t.Run("usage date filters are strict", func(t *testing.T) {
		for _, base := range []string{"/v1/usage-events", "/v1/usage-events/aggregate"} {
			future := call("GET", base+"?from=2030-01-01", nil).want(t, 200, base+" plain future date")
			if n := flowInt(future.body, "total_events"); base == "/v1/usage-events/aggregate" && n != 0 {
				t.Errorf("%s?from=2030-01-01 total_events = %d, want 0: the date must filter", base, n)
			}
			if n := flowInt(future.body, "total"); base == "/v1/usage-events" && n != 0 {
				t.Errorf("%s?from=2030-01-01 total = %d, want 0: the date must filter", base, n)
			}
			today := frozen.Format("2006-01-02")
			within := call("GET", base+"?from="+today+"&to="+today, nil).want(t, 200, base+" plain date today")
			if n := flowInt(within.body, "total_events"); base == "/v1/usage-events/aggregate" && n != 1 {
				t.Errorf("%s today total_events = %d, want 1: a date-only `to` includes the day", base, n)
			}
			if n := flowInt(within.body, "total"); base == "/v1/usage-events" && n != 1 {
				t.Errorf("%s today total = %d, want 1: a date-only `to` includes the day", base, n)
			}
			for _, bad := range []string{"from=2030-13-01", "to=yesterday", "from=2030-01-02&to=2030-01-01"} {
				got := call("GET", base+"?"+bad, nil)
				if got.status != 422 {
					t.Errorf("%s?%s = %d, want 422 (body %s)", base, bad, got.status, got.raw)
				}
			}
		}
	})

	// D-11: cost and margin treat every provider rate as dollars, so a rate
	// in another currency is refused instead of stored.
	t.Run("provider cost rates are USD only", func(t *testing.T) {
		rate := func(currency any) flowResp {
			body := map[string]any{"provider": "anthropic", "model": "claude-x", "token_type": "input", "unit_cost_cents": "0.0003"}
			if currency != nil {
				body["currency"] = currency
			}
			return call("PUT", "/v1/provider-costs", body)
		}
		if got := rate("EUR"); got.status != 422 {
			t.Errorf("EUR rate = %d, want 422 (body %s)", got.status, got.raw)
		}
		for _, c := range []any{nil, "USD", "usd"} {
			got := rate(c).want(t, 200, "USD rate")
			if cur := flowStr(got.body, "currency"); cur != "USD" {
				t.Errorf("rate with currency %v stored %q, want USD", c, cur)
			}
		}
	})

	// D-14: a non-graduated rule reads tiers as [], not null, and
	// an item with no metadata leaves the field out instead of sending
	// "e30=" (base64 of "{}").
	t.Run("empty values read as empty", func(t *testing.T) {
		rule := call("GET", "/v1/rating-rules/"+ruleID, nil).want(t, 200, "read rating rule")
		if !bytes.Contains(rule.raw, []byte(`"tiers":[]`)) {
			t.Errorf("rating rule tiers is not []: %s", rule.raw)
		}
		override := call("POST", "/v1/price-overrides", map[string]any{
			"customer_id": customerID, "rating_rule_version_id": ruleID,
			"mode": "per_unit", "unit_amount_cents": 5, "reason": "honest test",
		}).want(t, 201, "create price override")
		if !bytes.Contains(override.raw, []byte(`"tiers":[]`)) {
			t.Errorf("price override tiers is not []: %s", override.raw)
		}
		read := call("GET", "/v1/subscriptions/"+subID, nil).want(t, 200, "read subscription")
		items := flowList(read.body, "items")
		if len(items) != 1 {
			t.Fatalf("subscription has %d items, want 1", len(items))
		}
		if m, ok := items[0]["metadata"]; ok {
			t.Errorf("item with no metadata sends metadata = %#v, want it left out", m)
		}
	})

	// D-07: the test clock's subscription list returns the same subscription
	// the subscription read does: name, billing time and items included.
	t.Run("test clock subscriptions are whole", func(t *testing.T) {
		list := flowList(call("GET", "/v1/test-clocks/"+clockID+"/subscriptions", nil).want(t, 200, "list clock subscriptions").body, "data")
		if len(list) != 1 {
			t.Fatalf("clock lists %d subscriptions, want 1", len(list))
		}
		got := list[0]
		if s := flowStr(got, "display_name"); s != "Honest Subscription" {
			t.Errorf("display_name = %q, want Honest Subscription", s)
		}
		if s := flowStr(got, "billing_time"); s != "anniversary" {
			t.Errorf("billing_time = %q, want anniversary", s)
		}
		items, _ := got["items"].([]any)
		if len(items) != 1 {
			t.Errorf("items = %v, want the one item", got["items"])
		}
	})
}
