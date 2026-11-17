package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// matchedRoute is the route a request matched, found once at the root
// (routeLookup): its method and canonical pattern, which key routeTable, and
// its path parameters. A path that matches no route has an empty pattern; a
// mount point asked for a method its sub-router lacks has the mount's
// pattern, which the table does not hold.
type matchedRoute struct {
	key    routeKey
	params *chi.Context
}

type matchedRouteKey struct{}

// routeOf is the route the request matched (routeLookup). It panics when the
// lookup has not run: that is a mis-wired server, and no reader may take
// "not looked up" for "matched no route". The Recoverer answers 500 and logs
// it.
func routeOf(ctx context.Context) matchedRoute {
	m, ok := ctx.Value(matchedRouteKey{}).(matchedRoute)
	if !ok {
		panic("api: the route lookup did not run: routeLookup.Middleware must be on the root router, above every reader of routeOf")
	}
	return m
}

// routeLookup finds each request's route once, at the root, before any
// group's middleware runs. Find on the built router gives the route's
// pattern and fills its path parameters before the mounted sub-routers run
// (TestRouteLookup_FindsEveryRouteAndItsParams). It runs for every request
// but a CORS preflight, which mw.CORS answers before it. The clock door, the
// idempotency layer's show-once check (answersWithShowOnceSecret) and the
// audit detector (auditRoute) read it through routeOf.
type routeLookup struct {
	// root is the built router, set once every route is registered.
	root chi.Routes
}

func (l *routeLookup) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		m := matchedRoute{params: rctx, key: routeKey{Method: r.Method}}
		// A miss keys as "", not as canonicalRoute(""), which is "/".
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
