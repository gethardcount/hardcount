package customer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListSort_RefusesOutsideTheSet: the customer list answers 422 for a
// ?sort or ?dir outside its closed set, with the param named and the allowed
// values listed. It used to fall back to created_at and desc without a word.
// Every key the dashboard sends is in the set, so the dashboard is unchanged.
// Mutation-verify: drop the customerSort.Validate guard in the handler → the list
// reaches the service and these requests no longer answer 422.
func TestListSort_RefusesOutsideTheSet(t *testing.T) {
	h := &Handler{}
	for _, c := range []struct{ query, param, names string }{
		{"sort=nope", "sort", "created_at, display_name, name"},
		{"dir=up", "dir", "asc or desc"},
		{"sort=created_at&dir=ASC", "dir", "asc or desc"},
	} {
		rec := httptest.NewRecorder()
		h.list(rec, httptest.NewRequest(http.MethodGet, "/?"+c.query, nil))
		var env struct {
			Error struct {
				Param   string `json:"param"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if rec.Code != http.StatusUnprocessableEntity || env.Error.Param != c.param || !strings.Contains(env.Error.Message, c.names) {
			t.Errorf("GET ?%s = %d param %q %q, want 422 on %s naming %s", c.query, rec.Code, env.Error.Param, env.Error.Message, c.param, c.names)
		}
	}
	for _, key := range []string{"created_at", "display_name", "email", "status"} {
		if err := customerSort.Validate(key, "asc"); err != nil {
			t.Errorf("the dashboard's sort key %q is refused: %v", key, err)
		}
	}
}
