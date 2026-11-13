package respond

import (
	"fmt"
	"net/http"
	"strings"
)

// ParseFilter reads a list filter that takes a fixed set of values, such as
// an invoice list's `status`. An absent or empty param is no filter and
// returns "". A value outside allowed is refused with a 422 naming the param
// and listing allowed. Passed on to the store, an unknown value would match
// no row, and the empty list would read as "none of those". On false, the
// 422 is written and the handler returns.
func ParseFilter[T ~string](w http.ResponseWriter, r *http.Request, param string, allowed ...T) (T, bool) {
	v := r.URL.Query().Get(param)
	if v == "" {
		return "", true
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		if v == string(a) {
			return a, true
		}
		names[i] = string(a)
	}
	ValidationField(w, r, param, fmt.Sprintf("%s must be one of %s", param, strings.Join(names, ", ")))
	return "", false
}
