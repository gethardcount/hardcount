// Package requesttest gives tests a request's record: request.Begin with a
// route that holds only the method, and no mw.TrustedClientIP in front, so
// the record's client address is the request's TCP peer.
package requesttest

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// Context returns parent holding a new record whose route holds only the
// method. Its client address is httptest's, 192.0.2.1.
func Context(parent context.Context) context.Context {
	var ctx context.Context
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent)
	Root(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() })).
		ServeHTTP(httptest.NewRecorder(), req)
	return ctx
}

// Root is h behind request.Begin: each request gets a record whose route
// holds only the method, and its own TCP peer as the client address.
func Root(h http.Handler) http.Handler {
	return request.Begin(func(r *http.Request) request.Route { return request.Route{Method: r.Method} })(h)
}
