package api

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

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

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

// middlewareName matches the closure a middleware constructor returns, as
// the runtime names it: ".../internal/auth.Require.func1", or, when the
// compiler inlined the constructor into NewServer,
// "...api.NewServer.func15.Require.8". The names are unique in this module:
// session.MiddlewareOrAPIKey is the one API-key authenticator the router
// mounts, and auth.Require, RequireMethod and RequireSession are the only
// functions with those names.
var middlewareName = regexp.MustCompile(`[./](MiddlewareOrAPIKey|Require|RequireMethod|RequireSession)\.(?:func)?\d+$`)

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
// It also checks that a secret key and a dashboard session still pass every
// gate they passed before, so the change takes nothing from them.
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
			case "MiddlewareOrAPIKey":
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
				// /v1/members takes a dashboard session only, as before.
			default:
				if !reached {
					t.Errorf("%s: a %s principal is refused (%d); ADR-133 takes nothing from it", key, kt, rec.Code)
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
