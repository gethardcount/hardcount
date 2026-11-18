package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/platform/request"
)

// routeFinder finds each request's route for its record (request.Begin): one
// Find on the built router gives its pattern and path parameters before any
// group runs. The pattern is kept only when routeTable has the route, so a
// path no route matches, and a mount asked for a method its sub-router lacks,
// get none. It runs above the Recoverer, where a panic drops the connection.
// So it must not panic for a request: Find reads only the method and the
// path, and nothing here is nil that a request can make nil.
type routeFinder struct {
	// root is the built router, set once every route is registered.
	root chi.Routes
}

func (f *routeFinder) route(r *http.Request) request.Route {
	rctx := chi.NewRouteContext()
	return request.Route{Method: r.Method, Pattern: f.pattern(rctx, r.Method, routedPath(r)), Params: rctx}
}

// methods lists the methods a route serves at r's path, by the same rule as
// route, for a 405's Allow header.
func (f *routeFinder) methods(r *http.Request) []string {
	var out []string
	for _, m := range routeMethods {
		if f.pattern(chi.NewRouteContext(), m, routedPath(r)) != "" {
			out = append(out, m)
		}
	}
	return out
}

// pattern is the canonical pattern of the route that serves method at path,
// and "" when routeTable lists none. Find fills rctx's path parameters.
func (f *routeFinder) pattern(rctx *chi.Context, method, path string) string {
	// A miss keys as "", not as canonicalRoute(""), which is "/".
	p := f.root.Find(rctx, method, path)
	if p == "" {
		return ""
	}
	key := routeKey{Method: method, Pattern: canonicalRoute(p)}
	if _, ok := routeTable[key]; !ok {
		return ""
	}
	return key.Pattern
}

// noRoute answers a request no route serves: 405, with the methods a route
// serves at its path in Allow, or 404 when none does. Inside /v1's
// authentication the permission check gives it (auth.RequirePermission), so
// no group step or handler runs for such a request.
type noRoute struct{ finder *routeFinder }

func (n noRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if methods := n.finder.methods(r); len(methods) > 0 {
		respond.MethodNotAllowed(w, r, methods)
		return
	}
	respond.RouteNotFound(w, r)
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
