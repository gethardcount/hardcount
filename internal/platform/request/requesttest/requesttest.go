// Package requesttest gives tests a context holding a request's record, as
// the server's root makes one (request.Begin), for code that reads or writes
// the record outside an HTTP round trip.
package requesttest

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// Context returns parent holding a new record, for a request that matched
// no route.
func Context(parent context.Context) context.Context {
	var ctx context.Context
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
	Root(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() })).
		ServeHTTP(httptest.NewRecorder(), req)
	return ctx
}

// Root is h behind the server's root: each request gets a record, for a
// request that matched no route.
func Root(h http.Handler) http.Handler {
	return request.Begin(func(r *http.Request) request.Route { return request.Route{Method: r.Method} })(h)
}
