package dunning

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
)

// TestListRuns_StateIsAClosedSet: ?state= takes the three states a run can
// be in. "paused" was listed in the spec, but nothing ever wrote it, so the
// filter always came back empty and read as "no paused runs". An unknown
// value is a 400 now, never an empty list that reads as an answer.
func TestListRuns_StateIsAClosedSet(t *testing.T) {
	h := NewHandler(NewService(newMemStore(), nil, nil))
	for query, want := range map[string]int{
		"":                 http.StatusOK,
		"?state=active":    http.StatusOK,
		"?state=escalated": http.StatusOK,
		"?state=resolved":  http.StatusOK,
		"?state=paused":    http.StatusBadRequest,
		"?state=Active":    http.StatusBadRequest,
		"?state=open":      http.StatusBadRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/runs"+query, nil)
		req = req.WithContext(auth.WithTenantID(req.Context(), "t1"))
		rr := httptest.NewRecorder()
		h.listRuns(rr, req)
		if rr.Code != want {
			t.Errorf("GET /runs%s: got %d, want %d (%s)", query, rr.Code, want, rr.Body.String())
		}
	}
}
