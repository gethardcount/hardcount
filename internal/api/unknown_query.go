package api

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// refuseUnknownQuery refuses a request whose query string holds a parameter
// its route does not take (routeDecl.query), as respond.DecodeStrict refuses
// an unknown body field: a misspelled filter would otherwise answer as if no
// filter was sent. The answer is 422 validation_error with param naming the
// first such parameter in sorted order, so the same request always gets the
// same answer. A parameter sent with an empty value is still one the route
// takes or not. A query string that does not parse is refused whole: Go's
// lenient reading drops the pairs it cannot read, and a dropped filter is the
// same silent answer. A request no route serves passes, for the router's 404
// or 405.
//
// It runs in every group. Where the group has a caller check and a rate
// limit, it runs after them, so a caller who is not signed in to a /v1 route
// gets 401 and never learns from a 422 that the route exists. It runs before
// Idempotency, so a refused request holds no key.
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
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			respond.Validation(w, r, "the query string could not be read: check its escapes, and separate parameters with &")
			return
		}
		if name, unknown := firstUnknownQuery(q, d.query); unknown {
			respond.ValidationField(w, r, name, fmt.Sprintf("unknown query parameter %q", name))
			return
		}
	}
	h.next.ServeHTTP(w, r)
}

// firstUnknownQuery is the first name in q, in sorted order, that takes does
// not list.
func firstUnknownQuery(q url.Values, takes []string) (string, bool) {
	for _, name := range slices.Sorted(maps.Keys(q)) {
		if !slices.Contains(takes, name) {
			return name, true
		}
	}
	return "", false
}
