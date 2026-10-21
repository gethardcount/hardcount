package respond

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// DecodeStrict decodes the request body into v and refuses any key v does
// not declare, at any depth (`items[].price` too). It writes the refusal and
// returns false; the handler returns on false.
//
// The money-path create routes use it because encoding/json drops unknown keys
// silently: `trial_period_days` instead of `trial_days` created an active,
// charged subscription with a 201, and `expiry` instead of `expires_at` a
// grant that never expires. An unknown key is a 422 with param set to the key,
// not a 400, because the idempotency middleware caches a 400: a corrected
// retry under the same Idempotency-Key would get the refusal replayed.
// Malformed JSON stays a 400 "invalid JSON body".
func DecodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if key, ok := unknownField(err); ok {
			ValidationField(w, r, key, fmt.Sprintf("unknown field %q", key))
			return false
		}
		BadRequest(w, r, "invalid JSON body")
		return false
	}
	return true
}

// unknownField pulls the key out of encoding/json's `json: unknown field "x"`,
// the only form DisallowUnknownFields reports.
func unknownField(err error) (string, bool) {
	quoted, ok := strings.CutPrefix(err.Error(), "json: unknown field ")
	if !ok {
		return "", false
	}
	key, uerr := strconv.Unquote(quoted)
	if uerr != nil {
		return "", false
	}
	return key, true
}
