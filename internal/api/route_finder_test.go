package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/go-chi/chi/v5"
)

// builtRouter is the production router, built once for the tests that look
// requests up on it.
var builtRouter = sync.OnceValue(func() chi.Routes { return newTestServer().router })

// begin is the server's root middleware that makes each request's record,
// on the built router.
func begin() func(http.Handler) http.Handler {
	return request.Begin((&routeFinder{root: builtRouter()}).route)
}

// lookedUp is the request as the middleware after the root sees it: with its
// record, and the route found on the built router.
func lookedUp(method, path string) *http.Request {
	var got *http.Request
	begin()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	return got
}

// routeTable is keyed on the canonical pattern (canonicalRoute), and
// routeFinder finds it at the root, before any group's middleware or mounted
// sub-router runs. So for every route, inside /v1 or not, and those inside
// mounted sub-routers and inline groups included, the finder must give the
// route's canonical pattern and fill its path parameters. A path made from
// each pattern finds it. (chi.Walk prints an inline group as "/*/", as in
// /v1/auth/*/login; Find returns /v1/auth/login, and canonicalRoute makes the
// two agree.) A path the router does not have gets no pattern.
func TestRouteFinder_FindsEveryRouteAndItsParams(t *testing.T) {
	param := regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)
	checked := 0
	err := chi.Walk(builtRouter(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = canonicalRoute(route)
		if strings.Contains(route, "*") || strings.Contains(route, ":") {
			t.Errorf("%s %s: a wildcard or regex pattern the tables cannot key on", method, route)
			return nil
		}
		path := param.ReplaceAllString(route, "p_$1")
		rt := request.Of(lookedUp(method, path).Context()).Route()
		if rt.Method != method || rt.Pattern != route {
			t.Errorf("%s %s: found as %s %s, want %s %s", method, path, rt.Method, rt.Pattern, method, route)
			return nil
		}
		for _, p := range param.FindAllStringSubmatch(route, -1) {
			if got := rt.Params.URLParam(p[1]); got != "p_"+p[1] {
				t.Errorf("%s %s: param %s = %q, want %q", method, route, p[1], got, "p_"+p[1])
			}
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked < 150 {
		t.Fatalf("checked %d routes, want every route of the router (190 today)", checked)
	}
	// No route serves these, though chi's Find names a mount for some: they
	// have no pattern, so Idempotency keeps no key and chi answers 404 or
	// 405. A mount's root with a trailing slash is served, under its key.
	for _, c := range []struct{ method, path, want string }{
		{http.MethodPost, "/v1/not/a/route", ""},
		{http.MethodGet, "/v1/invoices/create_preview", ""},
		{http.MethodGet, "/v1/invoices/create_preview/", ""},
		{http.MethodPost, "/v1/customers/x/usage", ""},
		{http.MethodPost, "/v1/invoices/x/typo", ""},
		{http.MethodPost, "/v1/customers/", "/v1/customers"},
	} {
		if got := request.Of(lookedUp(c.method, c.path).Context()).Route().Pattern; got != c.want {
			t.Errorf("%s %s: pattern %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// A server that would serve a route the route table does not list does not
// start, and names the route (mustListEveryRoute).
func TestServer_RefusesToStartWithARouteTheTableDoesNotList(t *testing.T) {
	key := routeKey{Method: http.MethodGet, Pattern: "/health"}
	saved, had := routeTable[key]
	delete(routeTable, key)
	t.Cleanup(func() {
		if had {
			routeTable[key] = saved
		}
	})
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "GET /health") {
			t.Errorf("building a server with GET /health unlisted recovered %v, want a refusal naming GET /health", r)
		}
	}()
	newTestServer()
}
