package api

import (
	"net/http"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/session"
)

// chainOf wraps each middleware around a stub and returns the handlers it
// makes, in order. chi.Walk reports middleware as funcs; the handler a
// middleware returns has the type that says what it does.
func chainOf(mws []func(http.Handler) http.Handler) []http.Handler {
	stub := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	out := make([]http.Handler, len(mws))
	for i, m := range mws {
		out[i] = m(stub)
	}
	return out
}

// is reports whether h is a T.
func is[T any](h http.Handler) bool {
	_, ok := h.(T)
	return ok
}

// chainStep names a middleware by the handler type it makes.
type chainStep struct {
	name string
	is   func(http.Handler) bool
}

var (
	authMW        = chainStep{"authentication", is[session.CallerRequired]}
	timeoutMW     = chainStep{"the timeout", is[mw.TimeLimit]}
	idempotencyMW = chainStep{"Idempotency", is[mw.IdempotencyGuard]}
	doorMW        = chainStep{"the door", is[doorHandler]}
)

// chainIndex is the index of the first middleware in mws that makes step, or
// -1.
func chainIndex(mws []func(http.Handler) http.Handler, step chainStep) int {
	for i, h := range chainOf(mws) {
		if step.is(h) {
			return i
		}
	}
	return -1
}
