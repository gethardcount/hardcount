package subscription

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
)

// TestListHandler_StatusAndCollectionParams: ?status=, ?billing= and
// ?collection= are closed sets. An unknown value is a 422 naming the filter
// in `param`, never an empty or unfiltered list that reads as an answer.
// "paused" is not a status: the dashboard's Paused tab sent ?status=paused
// and always showed nothing (sweep E6), because a paused subscription stays
// active and only its collection pauses. "archived" is not a status either:
// nothing ever wrote it, so it was removed before the first release.
func TestListHandler_StatusAndCollectionParams(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	h := NewHandler(NewService(store, nil))

	for query, want := range map[string]int{
		"":                                 http.StatusOK,
		"?status=draft":                    http.StatusOK,
		"?status=trialing":                 http.StatusOK,
		"?status=active":                   http.StatusOK,
		"?status=canceled":                 http.StatusOK,
		"?billing=failing":                 http.StatusOK,
		"?status=archived":                 http.StatusUnprocessableEntity,
		"?status=paused":                   http.StatusUnprocessableEntity,
		"?status=past_due":                 http.StatusUnprocessableEntity,
		"?status=Active":                   http.StatusUnprocessableEntity,
		"?status=cancelled":                http.StatusUnprocessableEntity,
		"?collection=paused":               http.StatusOK,
		"?collection=paused&status=active": http.StatusOK,
		"?collection=running":              http.StatusUnprocessableEntity,
		"?collection=PAUSED":               http.StatusUnprocessableEntity,
		"?billing=ok":                      http.StatusUnprocessableEntity,
	} {
		req := httptest.NewRequest(http.MethodGet, "/subscriptions"+query, nil)
		req = req.WithContext(auth.WithTenantID(req.Context(), tenantID))
		rr := httptest.NewRecorder()
		h.list(rr, req)
		if rr.Code != want {
			t.Errorf("GET /subscriptions%s: got %d, want %d (%s)", query, rr.Code, want, rr.Body.String())
		}
	}
}

// TestListHandler_FilterParamsAnswer422WithParam: every closed-set list
// filter answers 422 validation_error with `param` naming the filter, the
// same shape a bad limit or offset gets, so clients handle one error shape
// for one kind of mistake (#1519).
func TestListHandler_FilterParamsAnswer422WithParam(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	h := NewHandler(NewService(store, nil))

	for query, param := range map[string]string{
		"?status=archived":    "status",
		"?collection=running": "collection",
		"?billing=ok":         "billing",
	} {
		req := httptest.NewRequest(http.MethodGet, "/subscriptions"+query, nil)
		req = req.WithContext(auth.WithTenantID(req.Context(), tenantID))
		rr := httptest.NewRecorder()
		h.list(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET /subscriptions%s: code %d, want 422 (%s)", query, rr.Code, rr.Body.String())
			continue
		}
		var body struct {
			Error struct {
				Code  string `json:"code"`
				Param string `json:"param"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Errorf("GET /subscriptions%s: body %q is not an error envelope (%v)", query, rr.Body.String(), err)
			continue
		}
		if body.Error.Code != "validation_error" {
			t.Errorf("GET /subscriptions%s: code %q, want validation_error", query, body.Error.Code)
		}
		if body.Error.Param != param {
			t.Errorf("GET /subscriptions%s: param %q, want %q", query, body.Error.Param, param)
		}
	}
}

// TestListHandler_FilterMessageListsAllowedValues: the 422 message tells the
// caller which values the filter accepts (#1519).
func TestListHandler_FilterMessageListsAllowedValues(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	h := NewHandler(NewService(store, nil))

	for _, tc := range []struct {
		query, want string
	}{
		{"?status=archived", "draft, trialing, active, canceled"},
		{`?collection=running`, "paused"},
		{`?billing=ok`, "failing"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/subscriptions"+tc.query, nil)
		req = req.WithContext(auth.WithTenantID(req.Context(), tenantID))
		rr := httptest.NewRecorder()
		h.list(rr, req)
		if !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("GET /subscriptions%s: body %q does not mention %q", tc.query, rr.Body.String(), tc.want)
		}
	}
}
