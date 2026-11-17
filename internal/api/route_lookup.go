package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// routeFinder finds each request's route for its record (request.Begin):
// one Find on the built router, which gives the route's pattern and fills
// its path parameters before any group's middleware or mounted sub-router
// runs (TestRouteLookup_FindsEveryRouteAndItsParams). The pattern is kept
// only when routeTable holds the route, so a path that matches no route,
// and a mount asked for a method its sub-router lacks, have none.
//
// It runs at the root above the Recoverer, as do three of the record's
// readers: the trace attributes, the metrics and the access log. A panic
// there is not recovered by ours, and net/http drops the connection. So it
// must not be able to panic for a request: Find on the built router reads
// only the method and the path, and nothing here is nil that a request can
// make nil. A missing record in those readers can only be a mis-wired
// server, and request.Of logs it before it panics.
type routeFinder struct {
	// root is the built router, set once every route is registered.
	root chi.Routes
}

func (f *routeFinder) route(r *http.Request) request.Route {
	rctx := chi.NewRouteContext()
	rt := request.Route{Method: r.Method, Params: rctx}
	// A miss keys as "", not as canonicalRoute(""), which is "/".
	if pattern := f.root.Find(rctx, r.Method, routedPath(r)); pattern != "" {
		key := routeKey{Method: r.Method, Pattern: canonicalRoute(pattern)}
		if _, ok := routeTable[key]; ok {
			rt.Pattern = key.Pattern
		}
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
