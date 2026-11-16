package api

// Tests of the ingest key (ADR-133): the end-to-end walk of a key that sends
// usage and nothing else, and the router walk that checks which routes refuse it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_IngestKey_SendsUsageAndNothingElse walks an ingest key (ADR-133)
// over the real router and database, the way a LiteLLM proxy uses one. The
// owner creates it from the dashboard session; it sends usage through all
// three ingest routes and the events land; minting a secret key, the example
// of a call it may not make, gets the ordinary missing-permission 403 and
// makes no key (TestIngestKey_RefusedOnEveryRouteButUsage checks every
// route).
//
// Mutation: give the ingest key apikey:write, or gate the LiteLLM callback
// on usage:write, and this fails.
func TestE2E_IngestKey_SendsUsageAndNothingElse(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "ingest-key-flow-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@ingest-key-flow.test", "ingest-key-flow-pw-12"
	flowBootstrap(t, ts, "ingest-key-flow-bootstrap", email, password)
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
	raw := flowStr(created.body, "raw_key")
	if !strings.HasPrefix(raw, "hc_ingest_test_") || flowStr(created.body, "key", "key_type") != "ingest" || flowObj(created.body, "key")["livemode"] != false {
		t.Fatalf("created key %v (raw key starts %q), want a test-mode ingest key starting hc_ingest_test_", flowObj(created.body, "key"), raw[:min(len(raw), 15)])
	}
	as := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: raw, body: body})
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

	// A call it may not make, minting a secret key: the ordinary
	// missing-permission 403, and no key is made.
	refused := as("POST", "/v1/api-keys", map[string]any{"name": "escalation", "key_type": "secret"})
	want := "insufficient permissions: this key type does not have apikey:write access"
	if refused.status != 403 || refused.errCode() != "forbidden" || refused.errMessage() != want {
		t.Errorf("POST /v1/api-keys with the ingest key: %d %s, want 403 forbidden %q", refused.status, refused.raw, want)
	}
	keys := asOwner("GET", "/v1/api-keys", nil).want(t, 200, "owner lists keys")
	for _, k := range flowList(keys.body, "data") {
		if flowStr(k, "name") == "escalation" {
			t.Errorf("the ingest key minted a key: %v", k)
		}
	}
}

// ingestKeyRoutes names routes and whether an ingest key reaches them
// (ADR-133). It reaches only sending usage, single and batch, the LiteLLM
// callback, and whoami, which any credential may call to see which account
// and mode it belongs to. Every route the router serves with API-key auth
// and that is not listed here must refuse it; the false entries are the
// ones a reader looks for first (refunds, void, key creation, the other
// usage routes), listed so a renamed route fails here instead of going
// unchecked.
var ingestKeyRoutes = map[string]bool{
	"POST /v1/usage-events":               true,
	"POST /v1/usage-events/batch":         true,
	"POST /v1/integrations/litellm/spend": true,
	"GET /v1/whoami":                      true,

	"GET /v1/usage-events":               false,
	"GET /v1/usage-events/aggregate":     false,
	"GET /v1/usage-events/held":          false,
	"POST /v1/usage-events/backfill":     false,
	"POST /v1/usage-events/held/release": false,
	"POST /v1/usage-events/held/discard": false,
	"POST /v1/api-keys":                  false,
	"GET /v1/api-keys":                   false,
	"DELETE /v1/api-keys/{id}":           false,
	"POST /v1/api-keys/{id}/rotate":      false,
	"POST /v1/invoices/{id}/void":        false,
	"POST /v1/credit-notes":              false,
	"GET /v1/customers":                  false,
	"POST /v1/customers":                 false,
	"POST /v1/test-clocks":               false,
	"GET /v1/webhook_events/stream":      false,
	"GET /v1/exports/usage-events.csv":   false,
	"POST /v1/billing/run":               false,
	"PUT /v1/settings":                   false,
	"POST /v1/subscriptions":             false,
	"POST /v1/credits/grant":             false,
	"GET /v1/audit-log":                  false,
	"POST /v1/meters":                    false,
}

// middlewareName finds a middleware's constructor in its closure's runtime
// name (".../auth.Require.func1", or ".../auth.Require.1" when inlined):
// chi.Walk reports middlewares as bare funcs. Any package's Require matches.
var middlewareName = regexp.MustCompile(`[./](RequireCaller|Require|RequireMethod|RequireSession)\.(?:func)?\d+$`)

// middlewareKind returns which of those constructors made mw, or "".
func middlewareKind(mw func(http.Handler) http.Handler) string {
	m := middlewareName.FindStringSubmatch(runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name())
	if m == nil {
		return ""
	}
	return m[1]
}

// TestIngestKey_RefusedOnEveryRouteButUsage walks the live router. On every
// route that takes an API key, it runs the route's permission gates (the
// auth.Require, RequireMethod and RequireSession middlewares chi.Walk
// reports for it) with an ingest key's principal, and checks the gates let
// it through only on the routes ingestKeyRoutes marks true. Everywhere else
// the answer is the ordinary missing-permission 403. A route with no gate
// at all lets the key through and fails here unless it is listed, so a new
// route cannot open to ingest keys by forgetting its permission.
//
// It also checks that a dashboard session passes every gate, and a secret
// key every gate but a session-only one.
//
// Mutation: give the ingest key any other permission (usage:read, say), or
// gate the usage routes on usage:write again, and this fails.
func TestIngestKey_RefusedOnEveryRouteButUsage(t *testing.T) {
	srv := newTestServer()
	seen := map[string]bool{}
	checked := 0
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			return nil
		}
		route = canonicalRoute(route)
		authed, sessionOnly := false, false
		var gates []func(http.Handler) http.Handler
		for _, mw := range mws {
			switch middlewareKind(mw) {
			case "RequireCaller":
				authed = true
			case "RequireSession":
				sessionOnly = true
				gates = append(gates, mw)
			case "Require", "RequireMethod":
				gates = append(gates, mw)
			}
		}
		if !authed {
			if len(gates) > 0 {
				t.Errorf("%s %s has a permission gate but no API-key auth before it", method, route)
			}
			return nil // a public route: no API key is read
		}
		key := method + " " + route
		seen[key] = true
		checked++

		for _, kt := range []auth.KeyType{auth.KeyTypeIngest, auth.KeyTypeSecret, auth.KeyTypeSession} {
			reached := false
			var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})
			for i := len(gates) - 1; i >= 0; i-- {
				h = gates[i](h)
			}
			ctx := auth.WithKeyType(auth.WithTenantID(postgres.WithLivemode(context.Background(), false), "hc_ten_gate"), kt)
			req := httptest.NewRequest(method, probePath(route), nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			switch {
			case kt == auth.KeyTypeIngest && ingestKeyRoutes[key]:
				if !reached {
					t.Errorf("%s: the ingest key is refused (%d), but it must reach this route", key, rec.Code)
				}
			case kt == auth.KeyTypeIngest:
				body := rec.Body.String()
				if reached || rec.Code != http.StatusForbidden {
					t.Errorf("%s: the ingest key got %d (reached=%v), want 403", key, rec.Code, reached)
				} else if missingPerm, sessionNeeded := strings.Contains(body, "insufficient permissions: this key type does not have"),
					sessionOnly && strings.Contains(body, "requires a dashboard session"); !missingPerm && !sessionNeeded {
					t.Errorf("%s: the ingest key's 403 says %s, want the missing-permission message", key, body)
				}
			case kt == auth.KeyTypeSecret && sessionOnly:
				// /v1/members and PUT /v1/settings take only a dashboard
				// session. TestE2E_Settings_ChangeTakesDashboardSession checks
				// that a secret key is refused on PUT /v1/settings.
			default:
				if !reached {
					t.Errorf("%s: a %s principal is refused (%d), want it let through", key, kt, rec.Code)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if checked < 100 {
		t.Fatalf("walked only %d routes that take an API key; the walk is broken", checked)
	}
	t.Logf("checked %d routes that take an API key", checked)
	var missing []string
	for k := range ingestKeyRoutes {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routes named in ingestKeyRoutes that the router does not serve with API-key auth: %v", missing)
	}
}
