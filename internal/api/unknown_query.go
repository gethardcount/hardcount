package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// refuseUnknownQuery refuses a request whose query string holds a parameter
// its route does not take (routeDecl.query), as respond.DecodeStrict refuses
// an unknown body field: a misspelled filter would otherwise answer as if no
// filter was sent. The answer is 422 validation_error with param naming the
// first such parameter in sorted order, so the same request always gets the
// same answer. A parameter sent with an empty value is still one the route
// takes or not. A request no route serves passes, for the router's 404 or
// 405.
//
// It runs in every group, after authentication, the permission check and the
// rate limit, so a caller who is not signed in gets 401 and never learns from
// a 422 that a route exists; and before Idempotency, so a refused request
// holds no key.
func refuseUnknownQuery(next http.Handler) http.Handler {
	return queryChecked{next: next}
}

// queryChecked runs next once the request's query parameters pass;
// refuseUnknownQuery makes it and has the rule.
type queryChecked struct {
	next http.Handler
}

func (h queryChecked) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d, ok := routeTable[routeKeyOf(r.Context())]
	if ok && !d.anyQuery {
		if name, unknown := firstUnknownQuery(r, d.query); unknown {
			respond.ValidationField(w, r, name, fmt.Sprintf("unknown query parameter %q", name))
			return
		}
	}
	h.next.ServeHTTP(w, r)
}

// firstUnknownQuery is the first name in r's query string, in sorted order,
// that takes does not list.
func firstUnknownQuery(r *http.Request, takes []string) (string, bool) {
	for _, name := range slices.Sorted(maps.Keys(r.URL.Query())) {
		if !slices.Contains(takes, name) {
			return name, true
		}
	}
	return "", false
}
