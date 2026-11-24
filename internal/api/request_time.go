package api

import (
	"net/http"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// bindRequestTime binds the time a request runs on: one read of c, the
// server's clock (clock.EnsureEffectiveNow), which clock.Now and every
// Clock's Now(ctx) then return, so all the request's work reads one time. It
// ends every group (docs/architecture.md#the-path-of-a-request says why), so
// every routed request gets one. An unrouted request that reaches a group, a
// path under a group's mount, gets one too, and the router answers it 404
// or 405.
func bindRequestTime(c clock.Clock) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return requestTimeBound{clock: c, next: next}
	}
}

// requestTimeBound runs next on the request's time; bindRequestTime makes it
// and has the rule.
type requestTimeBound struct {
	clock clock.Clock
	next  http.Handler
}

func (h requestTimeBound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.next.ServeHTTP(w, r.WithContext(clock.EnsureEffectiveNow(r.Context(), h.clock)))
}
