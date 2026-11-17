package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
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

// TestRouterChains: on every route the chain runs in this order, each step
// where the route has it: the CSRF guard, authentication, the rate limit,
// Idempotency, the timeout, then the clock door (router.go says why the door
// is last). Each authenticated route is in one of the four /v1 groups, read
// from its chain: its limiter, whether it has Idempotency, and its time
// limit. The ingest, export and stream groups hold exactly the routes listed
// here, and every other authenticated route is an ordinary one.
func TestRouterChains(t *testing.T) {
	srv := newTestServer()
	type group struct {
		name        string
		limiter     *mw.RateLimiter
		idempotency bool
		timeout     time.Duration // 0: none
	}
	ordinary := group{"ordinary", srv.rateLimiter, true, 30 * time.Second}
	ingest := group{"ingest", srv.ingestLimiter, false, 30 * time.Second}
	exports := group{"exports", srv.rateLimiter, false, exportTimeout}
	stream := group{"stream", srv.rateLimiter, false, 0}
	want := map[group][]string{
		ingest: {
			"GET /v1/usage-events", "GET /v1/usage-events/aggregate", "GET /v1/usage-events/held",
			"POST /v1/integrations/litellm/spend", "POST /v1/usage-events", "POST /v1/usage-events/backfill",
			"POST /v1/usage-events/batch", "POST /v1/usage-events/held/discard", "POST /v1/usage-events/held/release",
		},
		exports: {
			"GET /v1/exports/audit-log.csv", "GET /v1/exports/customers.csv", "GET /v1/exports/invoices.csv",
			"GET /v1/exports/subscriptions.csv", "GET /v1/exports/usage-events.csv",
		},
		stream: {"GET /v1/webhook_events/stream"},
	}
	steps := []chainStep{csrfMW, authMW, limiterMW, idempotencyMW, timeoutMW, doorMW}
	const csrf, authn, limit, idem, timeout, door = 0, 1, 2, 3, 4, 5
	got := map[group][]string{}
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := method + " " + canonicalRoute(route)
		at := make([]int, len(steps))
		last, lastName := -1, ""
		for i, step := range steps {
			if at[i] = chainIndex(mws, step); at[i] < 0 {
				continue
			}
			if at[i] < last {
				t.Errorf("%s: %s comes before %s", key, step.name, lastName)
			}
			last, lastName = at[i], step.name
		}
		if at[authn] < 0 {
			return nil // a public route: only the order applies
		}
		for _, i := range []int{csrf, limit, door} {
			if at[i] < 0 {
				t.Errorf("%s: authenticated but without %s", key, steps[i].name)
				return nil
			}
		}
		g := group{idempotency: at[idem] >= 0}
		for _, l := range []*mw.RateLimiter{srv.rateLimiter, srv.ingestLimiter} {
			if makesSame(mws[at[limit]], l.Middleware()) {
				g.limiter = l
			}
		}
		if at[timeout] >= 0 {
			for _, d := range []time.Duration{30 * time.Second, exportTimeout} {
				if makesSame(mws[at[timeout]], mw.Timeout(d)) {
					g.timeout = d
				}
			}
		}
		for _, known := range []group{ordinary, ingest, exports, stream} {
			if g.limiter == known.limiter && g.idempotency == known.idempotency && g.timeout == known.timeout {
				got[known] = append(got[known], key)
				return nil
			}
		}
		t.Errorf("%s: in none of the four groups: its limiter, Idempotency %v and time limit %v match none", key, g.idempotency, g.timeout)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for g, routes := range want {
		sort.Strings(got[g])
		if strings.Join(got[g], "\n") != strings.Join(routes, "\n") {
			t.Errorf("the %s group holds:\n  %s\nwant:\n  %s", g.name, strings.Join(got[g], "\n  "), strings.Join(routes, "\n  "))
		}
	}
	if len(got[ordinary]) < 100 {
		t.Errorf("only %d ordinary routes: did the walk break?", len(got[ordinary]))
	}
}

// TestRouter_CrossSiteWriteToTheStreamOrAnExportIsRefused: a cross-site
// write (any method but GET, HEAD, OPTIONS or TRACE, from another site,
// unless it carries only an API key) answers 403 on the event stream and the
// exports, as on every other signed-in /v1 route. They were mounted outside
// the CSRF guard and answered 401, 404, 405 or 429.
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
