package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Every list route pages one way: `limit` and `offset` in, `{data, total}`
// out. limit is 1 to 100 and defaults to 50; offset is 0 or more. A value
// outside that, or one that is not a whole number, is a 422 naming the
// parameter.
//
// TestListRoutes_OnePagingStyle walks every GET route the router serves. A
// route is either a list (listRoutes: it is called and must answer that
// shape) or named in notListRoutes with the reason it is not one. A new GET
// route that is neither fails the test, so a future list cannot ship with a
// cursor, a different envelope or a silent cap.

// notListRoutes are the GET routes that answer something other than a list
// of resources. Keys are the chi pattern.
var notListRoutes = map[string]string{
	"/health":       "health check",
	"/health/ready": "health check",
	"/metrics":      "Prometheus scrape",

	"/v1/whoami":                             "one object: the caller",
	"/v1/settings":                           "one object: the tenant's settings",
	"/v1/settings/email":                     "one object: email delivery status",
	"/v1/settings/stripe":                    "settings: one row per mode, so at most two",
	"/v1/tenants/{id}":                       "one resource",
	"/v1/customers/{id}":                     "one resource",
	"/v1/customers/{id}/billing-profile":     "one object of the customer",
	"/v1/customers/{id}/margin":              "one object of the customer",
	"/v1/customers/{id}/usage":               "one object: the customer's usage in a period",
	"/v1/customers/{customer_id}/overview":   "one object of the customer",
	"/v1/meters/{id}":                        "one resource",
	"/v1/plans/{id}":                         "one resource",
	"/v1/subscriptions/{id}":                 "one resource",
	"/v1/invoices/{id}":                      "one resource",
	"/v1/invoices/{id}/pdf":                  "a PDF",
	"/v1/credit-notes/{id}":                  "one resource",
	"/v1/credit-notes/{id}/pdf":              "a PDF",
	"/v1/dunning/policies/{id}":              "one resource",
	"/v1/dunning/runs/{id}":                  "one resource",
	"/v1/test-clocks/{id}":                   "one resource",
	"/v1/webhook-endpoints/endpoints/{id}":   "one resource",
	"/v1/recipes/{key}":                      "one resource",
	"/v1/credit-notes/stats":                 "counts",
	"/v1/dunning/stats":                      "counts",
	"/v1/audit-log/filters":                  "the values for the audit page's filter menus",
	"/v1/usage-events/aggregate":             "totals over the filtered events",
	"/v1/usage-events/held":                  "a summary: the first customers' held groups, plus totals that count them all",
	"/v1/credits/balance/{customer_id}":      "one customer's balance, one row per currency",
	"/v1/credits/balance/{customer_id}/live": "one object: the customer's balance now",
	"/v1/credits/grants/{customer_id}":       "one object: the customer's grants and their subtotals",
	"/v1/invoices/{id}/payment-timeline":     "one invoice's timeline, with its own truncated flag",
	"/v1/subscriptions/{id}/timeline":        "one subscription's timeline, with its own truncated flag",
	"/v1/webhook_events/{id}/deliveries":     "one event's delivery history; attempt numbers count across all of it",
	"/v1/webhook_events/stream":              "server-sent events",
	"/v1/members":                            "the team page: members and pending invitations in one answer (dashboard session only)",
	"/v1/analytics/overview":                 "one object of figures",
	"/v1/analytics/revenue-chart":            "a chart series for one period",
	"/v1/analytics/mrr-movement":             "a chart series for one period",
	"/v1/analytics/usage":                    "a chart series for one period",
	"/v1/exports/audit-log.csv":              "a CSV download of every matching row",
	"/v1/exports/customers.csv":              "a CSV download of every matching row",
	"/v1/exports/invoices.csv":               "a CSV download of every matching row",
	"/v1/exports/subscriptions.csv":          "a CSV download of every matching row",
	"/v1/exports/usage-events.csv":           "a CSV download of every matching row",
	"/v1/auth/invite/{token}":                "one object: the invitation behind a link",
	"/v1/auth/password-reset/check":          "one object: whether a reset link works",
	"/v1/public/invoices/{token}":            "the hosted invoice page's data",
	"/v1/public/invoices/{token}/pdf":        "a PDF",
	"/v1/public/payment-updates/{token}":     "the payment-update page's data",
	"/v1/public/cost-dashboard/{token}":      "the public cost dashboard's data",
}

// listRoutesNotSeeded are list routes the fixture below cannot fill with two
// rows, so only their shape and their limit and offset checks run.
var listRoutesNotSeeded = map[string]string{
	"/v1/customers/{customer_id}/payment-methods": "a payment method comes from Stripe",
	"/v1/invoices/{id}/disputes":                  "a dispute comes from Stripe",
	"/v1/customers/{id}/sent-emails":              "an email row is written by the email worker's flow, not by these requests",
}

// unpagedSets are the notListRoutes that answer a set of rows in one
// response, with no total and no paging. docs/api-surface.md names each in
// its Lists section, so a caller does not page one expecting a 422 or a
// next page.
var unpagedSets = []string{
	"/v1/usage-events/held",
	"/v1/subscriptions/{id}/timeline",
	"/v1/invoices/{id}/payment-timeline",
	"/v1/webhook_events/{id}/deliveries",
	"/v1/members",
	"/v1/credits/balance/{customer_id}",
	"/v1/credits/grants/{customer_id}",
}

// TestListsDoc_NamesTheSetsThatDoNotPage: the Lists section says every list
// pages one way, so it must name the routes that answer a set without
// paging.
func TestListsDoc_NamesTheSetsThatDoNotPage(t *testing.T) {
	doc, err := os.ReadFile("../../docs/api-surface.md")
	if err != nil {
		t.Fatal(err)
	}
	_, lists, ok := strings.Cut(string(doc), "\n## Lists\n")
	if !ok {
		t.Fatal("docs/api-surface.md has no Lists section")
	}
	lists, _, _ = strings.Cut(lists, "\n## ")
	for _, route := range unpagedSets {
		if !strings.Contains(lists, "`GET "+route+"`") {
			t.Errorf("the Lists section of docs/api-surface.md does not name `GET %s`, which does not page", route)
		}
	}
}

func TestListRoutes_OnePagingStyle(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Postgres")
	}
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "One Paging Style Co")
	testutil.CreateTestTenant(t, db, "One Paging Style Second Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	createAPIKeyWithMode(t, db, tenantID, false)
	platform := createPlatformKey(t, db, tenantID)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		req := flowReq{bearer: key, body: body}
		if strings.HasPrefix(path, "/v1/tenants") {
			req.bearer = platform
		}
		return flowDo(t, ts, method, path, req)
	}
	id := func(r flowResp) string { return flowStr(r.body, "id") }

	// The fixture: at least two rows in every list it can fill.
	meterA := id(call("POST", "/v1/meters", map[string]any{
		"event_name": "paging_a", "name": "Paging A", "unit": "call", "aggregation": "sum",
	}).want(t, 201, "meter A"))
	meterB := id(call("POST", "/v1/meters", map[string]any{
		"event_name": "paging_b", "name": "Paging B", "unit": "call", "aggregation": "sum",
	}).want(t, 201, "meter B"))
	planBody := func(code string, meters ...string) map[string]any {
		prices := []map[string]any{}
		for _, m := range meters {
			prices = append(prices, map[string]any{"meter_id": m, "mode": "per_unit", "unit_amount_cents": "10"})
		}
		return map[string]any{
			"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
			"base_amount_cents": 1000, "base_bill_timing": "in_arrears", "prices": prices,
		}
	}
	planA := call("POST", "/v1/plans", planBody("paging-a", meterA)).want(t, 201, "plan A").body
	planB := id(call("POST", "/v1/plans", planBody("paging-b", meterA, meterB)).want(t, 201, "plan B"))
	priceA := flowStr(flowList(planA, "prices")[0], "id")
	frozen := time.Now().UTC().Format(time.RFC3339)
	clk := id(call("POST", "/v1/test-clocks", map[string]any{"name": "paging 1", "frozen_time": frozen}).want(t, 201, "clock 1"))
	call("POST", "/v1/test-clocks", map[string]any{"name": "paging 2", "frozen_time": frozen}).want(t, 201, "clock 2")
	customer := func(ext, clockID string) string {
		body := map[string]any{"external_id": ext, "display_name": ext, "email": ext + "@paging.test"}
		if clockID != "" {
			body["test_clock_id"] = clockID
		}
		return id(call("POST", "/v1/customers", body).want(t, 201, "customer "+ext))
	}
	subscribe := func(code, customerID string) string {
		return id(call("POST", "/v1/subscriptions", map[string]any{
			"code": code, "display_name": code, "customer_id": customerID,
			"items": []map[string]any{{"plan_id": flowStr(planA, "id")}}, "billing_time": "anniversary",
		}).want(t, 201, "subscription "+code))
	}
	cusA, cusB := customer("paging-a", ""), customer("paging-b", "")
	subA := subscribe("paging-sub-a", cusA)
	subscribe("paging-sub-b", cusB)
	subscribe("paging-sub-c1", customer("paging-c1", clk))
	subscribe("paging-sub-c2", customer("paging-c2", clk))
	for i := range 2 {
		call("POST", "/v1/usage-events", map[string]any{
			"external_customer_id": "paging-a", "event_name": "paging_a", "quantity": 1,
			"idempotency_key": fmt.Sprintf("paging-%d", i),
		}).want(t, 201, "usage event")
	}
	for i := range 2 {
		call("POST", "/v1/dunning/policies", map[string]any{
			"name": fmt.Sprintf("Paging policy %d", i), "enabled": true, "retry_days": []int{3, 3},
		}).want(t, 201, "dunning policy")
	}
	line := []map[string]any{{"description": "work", "quantity": 1, "unit_amount_cents": 1000}}
	call("POST", "/v1/invoices", map[string]any{"customer_id": cusA, "line_items": line}).want(t, 201, "draft invoice")
	var finalInvoice string
	for range 2 {
		finalInvoice = id(call("POST", "/v1/invoices", map[string]any{"customer_id": cusB, "line_items": line}).want(t, 201, "invoice"))
		call("POST", "/v1/invoices/"+finalInvoice+"/finalize", nil).want(t, 200, "finalize")
	}
	for range 2 {
		call("POST", "/v1/credit-notes", map[string]any{
			"invoice_id": finalInvoice, "reason": "paging",
			"lines": []map[string]any{{"description": "x", "quantity": 1, "unit_amount_cents": 100}},
		}).want(t, 201, "credit note")
	}
	for _, c := range []string{cusA, cusB} {
		call("POST", "/v1/price-overrides", map[string]any{
			"customer_id": c, "price_id": priceA, "mode": "per_unit", "unit_amount_cents": "5",
		}).want(t, 201, "price override")
	}
	for _, c := range []string{cusA, cusA, cusB} {
		call("POST", "/v1/credits/grant", map[string]any{
			"customer_id": c, "currency": "USD", "amount_cents": 50, "description": "paging",
		}).want(t, 201, "credit grant")
	}
	endpoint := ""
	for i, u := range []string{"https://example.com/paging-1", "https://example.com/paging-2"} {
		ep := call("POST", "/v1/webhook-endpoints/endpoints", map[string]any{"url": u}).want(t, 201, "webhook endpoint").body
		endpoint = flowStr(flowObj(ep, "endpoint"), "id")
		// Deliveries give the stats list a row for each endpoint, and the
		// last endpoint two rows in its own delivery list, so its paging and
		// its total are checked against the other endpoint's row. The
		// dispatcher writes deliveries, and this server does not run it.
		for range i + 1 {
			seedBypass(t, db, `WITH e AS (
					INSERT INTO webhook_events (tenant_id, event_type, payload) VALUES ($1, 'invoice.created', '{}') RETURNING id)
				INSERT INTO webhook_deliveries (tenant_id, webhook_endpoint_id, webhook_event_id, status)
				SELECT $1, $2, e.id, 'succeeded' FROM e`, tenantID, endpoint)
		}
	}
	webhookEvent := flowStr(flowList(call("GET", "/v1/webhook-endpoints/events?limit=1", nil).want(t, 200, "webhook events").body, "data")[0], "id")
	for _, model := range []string{"paging-1", "paging-2"} {
		call("PUT", "/v1/provider-costs", map[string]any{
			"provider": "openai", "model": model, "token_type": "input", "unit_cost_cents": "0.0001",
		}).want(t, 200, "provider cost")
	}
	for _, recipe := range []string{"anthropic_style", "openai_style"} {
		call("POST", "/v1/recipes/"+recipe+"/instantiate", map[string]any{}).want(t, 201, "recipe "+recipe)
	}

	// listRoutes maps every list route's pattern to a path the fixture fills.
	listRoutes := map[string]string{
		"/v1/api-keys":                                    "/v1/api-keys",
		"/v1/audit-log":                                   "/v1/audit-log",
		"/v1/credit-notes":                                "/v1/credit-notes",
		"/v1/credits/balances":                            "/v1/credits/balances",
		"/v1/credits/ledger/{customer_id}":                "/v1/credits/ledger/" + cusA,
		"/v1/customers":                                   "/v1/customers",
		"/v1/customers/{customer_id}/payment-methods":     "/v1/customers/" + cusA + "/payment-methods",
		"/v1/customers/{id}/sent-emails":                  "/v1/customers/" + cusB + "/sent-emails",
		"/v1/dunning/policies":                            "/v1/dunning/policies",
		"/v1/dunning/runs":                                "/v1/dunning/runs",
		"/v1/invoices":                                    "/v1/invoices",
		"/v1/invoices/{id}/disputes":                      "/v1/invoices/" + finalInvoice + "/disputes",
		"/v1/meters":                                      "/v1/meters",
		"/v1/plans":                                       "/v1/plans",
		"/v1/plans/{id}/prices":                           "/v1/plans/" + planB + "/prices",
		"/v1/price-overrides":                             "/v1/price-overrides",
		"/v1/provider-costs":                              "/v1/provider-costs",
		"/v1/recipes":                                     "/v1/recipes",
		"/v1/recipes/instances":                           "/v1/recipes/instances",
		"/v1/subscriptions":                               "/v1/subscriptions",
		"/v1/tenants":                                     "/v1/tenants",
		"/v1/test-clocks":                                 "/v1/test-clocks",
		"/v1/test-clocks/{id}/customers":                  "/v1/test-clocks/" + clk + "/customers",
		"/v1/test-clocks/{id}/subscriptions":              "/v1/test-clocks/" + clk + "/subscriptions",
		"/v1/usage-events":                                "/v1/usage-events",
		"/v1/webhook-endpoints/endpoints":                 "/v1/webhook-endpoints/endpoints",
		"/v1/webhook-endpoints/endpoints/stats":           "/v1/webhook-endpoints/endpoints/stats",
		"/v1/webhook-endpoints/endpoints/{id}/deliveries": "/v1/webhook-endpoints/endpoints/" + endpoint + "/deliveries",
		"/v1/webhook-endpoints/events":                    "/v1/webhook-endpoints/events",
	}

	var gets []string
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet {
			gets = append(gets, canonicalRoute(route))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk router: %v", err)
	}
	sort.Strings(gets)
	live := map[string]bool{}
	for _, route := range gets {
		live[route] = true
		_, isList := listRoutes[route]
		_, notList := notListRoutes[route]
		switch {
		case isList && notList:
			t.Errorf("GET %s is in both listRoutes and notListRoutes", route)
		case !isList && !notList:
			t.Errorf("GET %s is not classified. A list answers {data, total} and takes limit and offset: add it to listRoutes. "+
				"Anything else goes in notListRoutes with the reason", route)
		}
	}
	for route := range listRoutes {
		if !live[route] {
			t.Errorf("listRoutes names GET %s, which the router does not serve", route)
		}
	}
	for route := range notListRoutes {
		if !live[route] {
			t.Errorf("notListRoutes names GET %s, which the router does not serve", route)
		}
	}
	for route := range listRoutesNotSeeded {
		if _, ok := listRoutes[route]; !ok {
			t.Errorf("listRoutesNotSeeded names %s, which is not in listRoutes", route)
		}
	}

	with := func(path, query string) string {
		if strings.Contains(path, "?") {
			return path + "&" + query
		}
		return path + "?" + query
	}
	// page reads one page and checks the envelope: exactly data and total.
	page := func(t *testing.T, path string) ([]json.RawMessage, int) {
		t.Helper()
		got := call("GET", path, nil)
		if got.status != 200 {
			t.Fatalf("GET %s = %d, want 200; body %s", path, got.status, clip(got.raw))
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(got.raw, &env); err != nil {
			t.Fatalf("GET %s: body is not a JSON object: %s", path, clip(got.raw))
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "data,total" {
			t.Fatalf("GET %s answers keys %v, want exactly [data total]", path, keys)
		}
		var data []json.RawMessage
		if err := json.Unmarshal(env["data"], &data); err != nil || data == nil {
			t.Fatalf("GET %s: data is not an array: %s", path, env["data"])
		}
		var total int
		if err := json.Unmarshal(env["total"], &total); err != nil {
			t.Fatalf("GET %s: total is not a whole number: %s", path, env["total"])
		}
		return data, total
	}

	for _, route := range gets {
		path, ok := listRoutes[route]
		if !ok {
			continue
		}
		t.Run(route, func(t *testing.T) {
			data, total := page(t, path)
			if len(data) > 50 {
				t.Errorf("default page holds %d rows, want at most 50", len(data))
			}
			if total < len(data) {
				t.Errorf("total %d is below the %d rows on the page", total, len(data))
			}
			// The fixture's lists fit on the default page, so total must
			// count exactly its rows: a total that counts rows outside the
			// list (another endpoint's, another customer's) is caught here.
			if total <= 50 && len(data) != total {
				t.Errorf("the default page holds %d rows but total is %d", len(data), total)
			}

			for _, bad := range []struct{ query, param string }{
				{"limit=0", "limit"}, {"limit=101", "limit"}, {"limit=-1", "limit"}, {"limit=ten", "limit"},
				{"limit=1.5", "limit"}, {"offset=-1", "offset"}, {"offset=x", "offset"},
				// The removed cursor is refused, not ignored: a client that
				// still sends it would read page 1 again as if it were page 2.
				{"after=abc", "after"},
			} {
				got := call("GET", with(path, bad.query), nil)
				if got.status != 422 || flowStr(got.body, "error", "param") != bad.param ||
					flowStr(got.body, "error", "type") != "invalid_request_error" {
					t.Errorf("GET %s?%s = %d %s, want 422 with param %q", route, bad.query, got.status, clip(got.raw), bad.param)
				}
			}
			if got := call("GET", with(path, "limit=100&offset=0"), nil); got.status != 200 {
				t.Errorf("GET %s?limit=100 = %d, want 200 (100 is the maximum); body %s", route, got.status, clip(got.raw))
			}

			if _, skip := listRoutesNotSeeded[route]; skip {
				return
			}
			if total < 2 {
				t.Fatalf("total = %d: the fixture must put two rows in this list, or name it in listRoutesNotSeeded", total)
			}
			first, t1 := page(t, with(path, "limit=1"))
			second, t2 := page(t, with(path, "limit=1&offset=1"))
			if len(first) != 1 || len(second) != 1 {
				t.Fatalf("limit=1 pages hold %d and %d rows, want 1 each", len(first), len(second))
			}
			if t1 != total || t2 != total {
				t.Errorf("total moved with the page: %d, %d, %d", total, t1, t2)
			}
			if string(first[0]) == string(second[0]) {
				t.Errorf("offset=1 answered the same row as offset=0: %s", clip(first[0]))
			}
			if string(first[0]) != string(data[0]) || (len(data) > 1 && string(second[0]) != string(data[1])) {
				t.Errorf("the limit=1 pages are not rows 1 and 2 of the default page")
			}
			past, t3 := page(t, with(path, fmt.Sprintf("offset=%d", total)))
			if len(past) != 0 || t3 != total {
				t.Errorf("offset=total answers %d rows and total %d, want 0 rows and total %d", len(past), t3, total)
			}
		})
	}

	// A set that does not page answers the same with or without the paging
	// parameters, as docs/api-surface.md says.
	unpagedPaths := map[string]string{
		"/v1/usage-events/held":              "/v1/usage-events/held",
		"/v1/subscriptions/{id}/timeline":    "/v1/subscriptions/" + subA + "/timeline",
		"/v1/invoices/{id}/payment-timeline": "/v1/invoices/" + finalInvoice + "/payment-timeline",
		"/v1/webhook_events/{id}/deliveries": "/v1/webhook_events/" + webhookEvent + "/deliveries",
		"/v1/credits/balance/{customer_id}":  "/v1/credits/balance/" + cusA,
		"/v1/credits/grants/{customer_id}":   "/v1/credits/grants/" + cusA,
	}
	for _, route := range unpagedSets {
		if _, ok := notListRoutes[route]; !ok {
			t.Errorf("unpagedSets names %s, which is not in notListRoutes", route)
		}
		path, ok := unpagedPaths[route]
		if !ok {
			continue // /v1/members answers only a dashboard session
		}
		t.Run("unpaged "+route, func(t *testing.T) {
			plain := call("GET", path, nil).want(t, 200, "plain")
			paged := call("GET", with(path, "limit=1&offset=1&after=x"), nil).want(t, 200, "with paging parameters")
			if string(plain.raw) != string(paged.raw) {
				t.Errorf("limit, offset and after changed the answer:\n%s\n%s", clip(plain.raw), clip(paged.raw))
			}
		})
	}

	// An unknown route and a wrong method answer the JSON error envelope too.
	t.Run("unknown route", func(t *testing.T) {
		got := call("GET", "/v1/no-such-route", nil)
		if got.status != 404 || got.errCode() != "route_not_found" || flowStr(got.body, "error", "request_id") == "" {
			t.Errorf("GET /v1/no-such-route = %d %s, want 404 with code route_not_found and a request_id", got.status, clip(got.raw))
		}
	})
	t.Run("wrong method", func(t *testing.T) {
		req, _ := http.NewRequest("DELETE", ts.URL+"/v1/meters", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != 405 || flowStr(body, "error", "code") != "method_not_allowed" {
			t.Errorf("DELETE /v1/meters = %d %v, want 405 with code method_not_allowed", resp.StatusCode, body)
		}
		if allow := resp.Header.Get("Allow"); allow != "GET, POST" {
			t.Errorf("Allow = %q, want %q", allow, "GET, POST")
		}
	})
}

// TestPlans_ListPagesPastFiveHundred is the silent cap: the plan list read at
// most 500 rows, so a tenant's 501st plan was in no answer and nothing said
// so. It is now on page 6 at limit 100, and total counts it.
func TestPlans_ListPagesPastFiveHundred(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Postgres")
	}
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Five Hundred Plans Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	const n = 501
	first := ""
	for i := range n {
		p := call("POST", "/v1/plans", map[string]any{
			"code": fmt.Sprintf("plan-%03d", i), "name": fmt.Sprintf("Plan %03d", i), "currency": "USD",
			"billing_interval": "monthly", "base_amount_cents": 100, "base_bill_timing": "in_arrears",
		}).want(t, 201, "create plan")
		if i == 0 {
			first = flowStr(p.body, "id")
		}
	}

	seen := map[string]bool{}
	for offset := 0; offset < n+100; offset += 100 {
		got := call("GET", fmt.Sprintf("/v1/plans?limit=100&offset=%d", offset), nil).want(t, 200, "list plans")
		if total := flowInt(got.body, "total"); total != n {
			t.Fatalf("offset %d: total = %d, want %d", offset, total, n)
		}
		for _, p := range flowList(got.body, "data") {
			seen[flowStr(p, "id")] = true
		}
	}
	if len(seen) != n {
		t.Errorf("paging through the plans found %d, want %d", len(seen), n)
	}
	// Newest first, so the first plan created is the one row of the last page.
	last := call("GET", "/v1/plans?limit=100&offset=500", nil).want(t, 200, "last page")
	if rows := flowList(last.body, "data"); len(rows) != 1 || flowStr(rows[0], "id") != first {
		t.Errorf("offset 500 holds %d rows, want only the first plan created (%s); body %s", len(rows), first, clip(last.raw))
	}
	if got := call("GET", "/v1/plans?limit=500", nil); got.status != 422 || flowStr(got.body, "error", "param") != "limit" {
		t.Errorf("GET /v1/plans?limit=500 = %d %s, want 422 naming limit", got.status, clip(got.raw))
	}
}

// TestLists_ReachRowsPastOldReadCaps covers the lists whose store read stopped
// at a fixed row count while total counted only what was read: a customer's
// payment methods (100) and a test clock's customers and subscriptions
// (1000). Paging at 100 must find every row, total must count them all, and
// the rows past the old cap must be on the last page. The rows are inserted
// in one statement, so they share created_at and only the id keeps the order
// stable from page to page.
func TestLists_ReachRowsPastOldReadCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Postgres")
	}
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Past The Caps Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	owner := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "cards-owner", "display_name": "Cards Owner",
	}).want(t, 201, "customer").body, "id")
	clk := flowStr(call("POST", "/v1/test-clocks", map[string]any{
		"name": "past the caps", "frozen_time": time.Now().UTC().Format(time.RFC3339),
	}).want(t, 201, "clock").body, "id")

	const cards, onClock = 105, 1005
	seedBypass(t, db, `INSERT INTO payment_methods (id, tenant_id, customer_id, stripe_payment_method_id, card_brand, card_last4)
		SELECT 'hc_pm_cap' || g, $1, $2, 'pm_cap' || g, 'visa', '4242' FROM generate_series(1, $3) g`,
		tenantID, owner, cards)
	seedBypass(t, db, `INSERT INTO customers (id, tenant_id, external_id, display_name, status, test_clock_id)
		SELECT 'hc_cus_cap' || g, $1, 'clock-cap-' || g, 'Clock cap ' || g, 'active', $2 FROM generate_series(1, $3) g`,
		tenantID, clk, onClock)
	seedBypass(t, db, `INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at, test_clock_id)
		SELECT 'hc_sub_cap' || g, $1, 'clock-cap-' || g, 'Clock cap ' || g, 'hc_cus_cap' || g, 'active', 'calendar',
			now(), now() + interval '1 month', now() + interval '1 month', $2
		FROM generate_series(1, $3) g`,
		tenantID, clk, onClock)

	for _, c := range []struct {
		path string
		want int
	}{
		{"/v1/customers/" + owner + "/payment-methods", cards},
		{"/v1/test-clocks/" + clk + "/customers", onClock},
		{"/v1/test-clocks/" + clk + "/subscriptions", onClock},
	} {
		t.Run(c.path, func(t *testing.T) {
			seen := map[string]bool{}
			for offset := 0; offset < c.want+100; offset += 100 {
				got := call("GET", fmt.Sprintf("%s?limit=100&offset=%d", c.path, offset), nil).want(t, 200, "list")
				if total := int(flowInt(got.body, "total")); total != c.want {
					t.Fatalf("offset %d: total = %d, want %d", offset, total, c.want)
				}
				for _, row := range flowList(got.body, "data") {
					id := flowStr(row, "id")
					if seen[id] {
						t.Errorf("offset %d: row %s was already on an earlier page", offset, id)
					}
					seen[id] = true
				}
			}
			if len(seen) != c.want {
				t.Errorf("paging found %d rows, want %d", len(seen), c.want)
			}
			last := call("GET", fmt.Sprintf("%s?limit=100&offset=%d", c.path, c.want-1), nil).want(t, 200, "last row")
			if rows := flowList(last.body, "data"); len(rows) != 1 {
				t.Errorf("offset %d holds %d rows, want the last one", c.want-1, len(rows))
			}
		})
	}
}

// seedBypass runs one test-mode insert with RLS off, for rows no route of
// this server writes.
func seedBypass(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, q)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// clip shortens a body for a failure message.
func clip(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
