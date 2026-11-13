package respond

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fruit string

// TestParseFilter: an absent or empty filter is no filter, a value in the set
// comes back typed, and anything else, a change of case included, is a 422 on
// the param that lists the set.
func TestParseFilter(t *testing.T) {
	cases := []struct {
		query    string
		want     fruit
		accepted bool
	}{
		{"", "", true},
		{"fruit=", "", true},
		{"fruit=apple", "apple", true},
		{"fruit=pear", "pear", true},
		{"fruit=plum", "", false},
		{"fruit=Apple", "", false},
		{"fruit=%20apple", "", false},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		got, ok := ParseFilter(rec, httptest.NewRequest(http.MethodGet, "/?"+c.query, nil), "fruit", fruit("apple"), fruit("pear"))
		if c.accepted {
			if !ok || got != c.want || rec.Body.Len() != 0 {
				t.Errorf("?%s = %q, %v (%s); want %q accepted with nothing written", c.query, got, ok, rec.Body.String(), c.want)
			}
			continue
		}
		var body ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if ok || rec.Code != http.StatusUnprocessableEntity || body.Error.Code != "validation_error" || body.Error.Param != "fruit" ||
			body.Error.Message != "fruit must be one of apple, pear" {
			t.Errorf("?%s = %d %s, want 422 validation_error on fruit listing apple, pear", c.query, rec.Code, rec.Body.String())
		}
	}
}
