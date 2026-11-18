package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// routeFinder finds each request's route for its record (request.Begin)
// before any group runs: whether the router serves the method at the path,
// and if so its pattern, kept only when routeTable has the route, and its path
// parameters. It runs above the Recoverer, where a panic drops the
// connection, so it must not panic for a request: Find and Match read only
// the method and the path, and nothing here is nil that a request can make
// nil.
type routeFinder struct {
	// root is the built router, and served its routes flat (newAllowIndex);
	// both are set once every route is registered.
	root   chi.Routes
	served *allowIndex
}

func (f *routeFinder) route(r *http.Request) request.Route {
	rctx := chi.NewRouteContext()
	rt := request.Route{Method: r.Method, Params: rctx}
	// Find names a mount's route even for a method its sub-router lacks, and
	// the flat index alone takes a trailing slash the router refuses: the
	// router serves the request only when both agree.
	pattern := f.root.Find(rctx, r.Method, routedPath(r))
	if pattern == "" || !f.served.serves(r.Method, routedPath(r)) {
		rt.NoRoute = true
		return rt
	}
	key := routeKey{Method: r.Method, Pattern: canonicalRoute(pattern)}
	if _, ok := routeTable[key]; ok {
		rt.Pattern = key.Pattern
	}
	return rt
}

// routeKeyOf is the request's key in routeTable, from its record.
func routeKeyOf(ctx context.Context) routeKey {
	rt := request.Of(ctx).Route()
	return routeKey{Method: rt.Method, Pattern: rt.Pattern}
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
