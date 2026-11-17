package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// matchedRoute is the route a request matched, found once at the edge
// (routeLookup): its method and canonical pattern, which key the route
// tables, and its path parameters. A path the router does not have has an
// empty pattern, or one no table holds.
type matchedRoute struct {
	key    routeKey
	params *chi.Context
}

type matchedRouteKey struct{}

// routeOf is the route the request matched (routeLookup), or the zero value
// before the lookup has run.
func routeOf(ctx context.Context) matchedRoute {
	m, _ := ctx.Value(matchedRouteKey{}).(matchedRoute)
	return m
}

// routeLookup finds each request's route once, at the root, before any
// group's middleware runs. Find on the built router gives the route's
// pattern and fills its path parameters before the mounted sub-routers run
// (TestRouteLookup_FindsEveryRouteAndItsParams). It runs for every
// request, inside /v1 or not. The clock door, the idempotency layer's
// show-once check (answersWithShowOnceSecret) and the audit detector
// (auditRoute) read it through routeOf.
type routeLookup struct {
	// root is the built router, set once every route is registered.
	root chi.Routes
}

func (l *routeLookup) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		m := matchedRoute{params: rctx, key: routeKey{Method: r.Method}}
		if pattern := l.root.Find(rctx, r.Method, routedPath(r)); pattern != "" {
			m.key.Pattern = canonicalRoute(pattern)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), matchedRouteKey{}, m)))
	})
}

// routedPath is the path chi routes the request on: the escaped path when
// the URL has one, so a path parameter holding an escaped slash finds the
// same route, with the same parameter, as the handler.
func routedPath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	return r.URL.Path
}
