package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/go-chi/chi/v5"
)

// newTestServer builds the production Server on a zero DB. NewServer runs
// engine.MustValidate after it wires the engine, so every test that calls
// this also proves that no engine collaborator is missing.
func newTestServer() *Server {
	// A zero DB has no connection, so a test may only send requests that are
	// answered without the database, such as /health or one with no
	// credentials, which gets 401.
	db := &postgres.DB{}
	return NewServer(db, nil, nil)
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var body map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body: got %v", body)
	}
}

func TestAuthRequired_AllProtectedRoutes(t *testing.T) {
	srv := newTestServer()

	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/customers"},
		{"POST", "/v1/customers"},
		{"GET", "/v1/meters"},
		{"GET", "/v1/plans"},
		{"GET", "/v1/plans/pln_123/prices"},
		{"GET", "/v1/subscriptions"},
		{"GET", "/v1/usage-events"},
		{"GET", "/v1/invoices"},
		{"GET", "/v1/api-keys"},
		{"GET", "/v1/dunning/policy"},
		{"GET", "/v1/credits/balance/cus_123"},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			req := httptest.NewRequest(r.method, r.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: got %d, want 401", r.method, r.path, rec.Code)
			}
			var body map[string]any
			_ = json.NewDecoder(rec.Body).Decode(&body)
			errObj, _ := body["error"].(map[string]any)
			if errObj == nil || errObj["type"] != "authentication_error" {
				t.Errorf("%s %s error type: got %v, want authentication_error", r.method, r.path, body["error"])
			}
			// Rate limiting runs after auth, so a 401 carries no rate-limit
			// headers.
			if got := rec.Header().Get("X-RateLimit-Limit"); got != "" {
				t.Errorf("%s %s: X-RateLimit-Limit = %q on an unauthenticated request, want none", r.method, r.path, got)
			}
		})
	}
}

// TestRouterChainOrder: every authenticated route runs authentication, then
// its rate limit, then Idempotency where it has it, then its timeout where it
// has one, then the clock door, last. The door reads the named record as the
// caller's tenant and mode, its lookups are bounded by the timeout, and a
// replay costs no lookup and never stores the door's 503. The stream has no
// timeout and no Idempotency, the exports and the usage ingest no
// Idempotency, and every other /v1 route all five. The usage ingest (every
// route under /v1/usage-events and /v1/integrations/litellm) counts against
// the ingest bucket, and every other route against the general one: LiteLLM
// retries only on 5xx, so a 429 from the general bucket would drop its event
// for good.
func TestRouterChainOrder(t *testing.T) {
	steps := []chainStep{authMW, limiterMW, idempotencyMW, timeoutMW, doorMW}
	want := map[string][]bool{ // which of steps a route has, in order
		"GET /v1/webhook_events/stream":       {true, true, false, false, true},
		"GET /v1/exports/customers.csv":       {true, true, false, true, true},
		"POST /v1/usage-events":               {true, true, false, true, true},
		"POST /v1/integrations/litellm/spend": {true, true, false, true, true},
		"POST /v1/subscriptions/{id}/cancel":  {true, true, true, true, true},
	}
	srv := newTestServer()
	seen := map[string]bool{}
	authed := 0
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		at := make([]int, len(steps))
		for i, step := range steps {
			at[i] = chainIndex(mws, step)
		}
		if at[0] < 0 {
			return nil // not authenticated: a public route, health or metrics
		}
		authed++
		path := canonicalRoute(route)
		key := method + " " + path
		last, lastName := -1, ""
		for i, idx := range at {
			if idx < 0 {
				continue
			}
			if idx < last {
				t.Errorf("%s: %s comes before %s; want authentication, rate limit, Idempotency, timeout, door", key, steps[i].name, lastName)
			}
			last, lastName = idx, steps[i].name
		}
		if at[1] < 0 || at[4] < 0 {
			t.Errorf("%s: authenticated but without %s and %s", key, steps[1].name, steps[4].name)
		} else {
			bucket, limiter := "general", srv.rateLimiter
			if strings.HasPrefix(path, "/v1/usage-events") || strings.HasPrefix(path, "/v1/integrations/litellm") {
				bucket, limiter = "ingest", srv.ingestLimiter
			}
			if limiterOf(mws[at[1]]) != reflect.ValueOf(limiter).Pointer() {
				t.Errorf("%s: counts against the wrong limiter; want the %s bucket", key, bucket)
			}
		}
		if has, ok := want[key]; ok {
			seen[key] = true
			for i := range steps {
				if (at[i] >= 0) != has[i] {
					t.Errorf("%s: %s present %v, want %v", key, steps[i].name, at[i] >= 0, has[i])
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("%s not found among the authenticated routes", key)
		}
	}
	if authed < 100 {
		t.Errorf("only %d authenticated routes: did the walk break?", authed)
	}
}

// TestRouter_CrossSiteWriteToTheStreamOrAnExportIsRefused: a cross-site
// write that carries a session cookie answers 403 on every /v1 path, the
// event stream and the exports included, since they share /v1's CSRF guard.
// They were mounted outside it and answered 401 or 405.
func TestRouter_CrossSiteWriteToTheStreamOrAnExportIsRefused(t *testing.T) {
	srv := NewServer(&postgres.DB{}, nil, nil)
	for _, path := range []string{"/v1/webhook_events/stream", "/v1/exports/customers.csv"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Origin", "https://attacker.example")
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "a-session"})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s with a session cookie = %d, want 403 (CSRF)", path, rec.Code)
		}
	}
}

// limiterOf is the limiter that the handler m makes counts against. The field
// is unexported, so the test reads it by reflection.
func limiterOf(m func(http.Handler) http.Handler) uintptr {
	return reflect.ValueOf(m(http.NotFoundHandler())).FieldByName("limiter").Pointer()
}
