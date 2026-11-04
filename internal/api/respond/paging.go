package respond

import (
	"fmt"
	"net/http"
	"strconv"
)

// Every list route pages the same way: `limit` and `offset` in, and
// `{data, total}` out (List). docs/api/pagination.md describes it for
// callers.
const (
	// DefaultLimit is the page size when the request sends no limit.
	DefaultLimit = 50
	// MaxLimit is the largest limit a request may send.
	MaxLimit = 100
)

// Page is one validated page request.
type Page struct {
	Limit  int
	Offset int
}

// ParsePage reads `limit` and `offset` from the query. A missing limit is
// DefaultLimit, and a missing offset is 0. A limit outside 1 to MaxLimit, a
// negative offset, or a value that is not a whole number is refused with a
// 422 naming the parameter. It is never clamped. The removed `after` cursor
// is refused too. If it were ignored, a client asking for the page after
// one it holds would get page 1. On false, the 422 is written and the
// handler returns.
func ParsePage(w http.ResponseWriter, r *http.Request) (Page, bool) {
	q := r.URL.Query()
	if _, ok := q["after"]; ok {
		ValidationField(w, r, "after", "after is not supported; page with offset")
		return Page{}, false
	}
	p := Page{Limit: DefaultLimit}
	if raw, ok := q["limit"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > MaxLimit || len(raw) > 1 {
			ValidationField(w, r, "limit", fmt.Sprintf("limit must be a whole number from 1 to %d", MaxLimit))
			return Page{}, false
		}
		p.Limit = n
	}
	if raw, ok := q["offset"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 0 || len(raw) > 1 {
			ValidationField(w, r, "offset", "offset must be a whole number, 0 or more")
			return Page{}, false
		}
		p.Offset = n
	}
	return p, true
}

// Window returns the page's rows of a list read in full. A route that reads
// its whole list (one parent's rows, or a small tenant setting) pages it
// here, so it answers in the same shape as a list paged in SQL. Such a
// route's total is len(all).
func Window[T any](all []T, p Page) []T {
	if p.Offset >= len(all) {
		return []T{}
	}
	end := min(p.Offset+p.Limit, len(all))
	return all[p.Offset:end]
}
