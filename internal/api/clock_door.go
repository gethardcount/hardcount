package api

import (
	"net/http"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// clockDoor is the door: the last step of every group, which binds the
// instant a request runs on before its handler runs. docs/architecture.md,
// "The path of a request", says why it ends every group. The instant is the
// time on the context that clock.Now and every Clock's Now(ctx) return, so
// all the work of one request reads one time.
//
// The door binds one read of the server's clock (clock.EnsureEffectiveNow),
// in either mode, and reads nothing else. It binds none on a route whose
// entry is decidedIn, whose function binds one instant per record it works
// through, nor on a request no route serves, which the router answers 404 or
// 405.
type clockDoor struct {
	// clock is the server's clock.
	clock clock.Clock
}

func (d *clockDoor) Middleware(next http.Handler) http.Handler {
	return doorHandler{door: d, next: next}
}

// doorHandler runs the clock door in front of next; clockDoor has the rule.
// clockDoor.Middleware makes it.
type doorHandler struct {
	door *clockDoor
	next http.Handler
}

func (h doorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// No entry means no route serves the request (mustListEveryRoute).
	if entry, routed := routeTable[routeKeyOf(r.Context())]; routed && entry.decidedIn == "" {
		r = r.WithContext(clock.EnsureEffectiveNow(r.Context(), h.door.clock))
	}
	h.next.ServeHTTP(w, r)
}
