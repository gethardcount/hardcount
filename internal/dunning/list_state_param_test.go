package dunning

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
)

// TestListRuns_StateIsAClosedSet: ?state= takes the three states a run can
// be in. "paused" was listed in the spec, but nothing ever wrote it, so the
// filter always came back empty and read as "no paused runs". An unknown
// value is a 422 naming the filter, never an empty list that reads as an
// answer.
func TestListRuns_StateIsAClosedSet(t *testing.T) {
	h := NewHandler(NewService(newMemStore(), nil, nil))
	for query, want := range map[string]int{
		"":                 http.StatusOK,
		"?state=active":    http.StatusOK,
		"?state=escalated": http.StatusOK,
		"?state=resolved":  http.StatusOK,
		"?state=paused":    http.StatusUnprocessableEntity,
		"?state=Active":    http.StatusUnprocessableEntity,
		"?state=open":      http.StatusUnprocessableEntity,
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

// TestListRuns_UnknownStateAnswers422WithParam: the 422 carries
// code=validation_error, `param: state` and the allowed values, the same
// shape a bad limit or offset gets (#1519).
func TestListRuns_UnknownStateAnswers422WithParam(t *testing.T) {
	h := NewHandler(NewService(newMemStore(), nil, nil))
	req := httptest.NewRequest(http.MethodGet, "/runs?state=paused", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), "t1"))
	rr := httptest.NewRecorder()
	h.listRuns(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("state=paused: code %d, want 422 (%s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not an error envelope (%v)", rr.Body.String(), err)
	}
	if body.Error.Code != "validation_error" {
		t.Errorf("code %q, want validation_error", body.Error.Code)
	}
	if body.Error.Param != "state" {
		t.Errorf("param %q, want state", body.Error.Param)
	}
	if !strings.Contains(rr.Body.String(), "active, escalated, resolved") {
		t.Errorf("body %q does not list the allowed states", rr.Body.String())
	}
}
