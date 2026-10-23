package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
)

// TestListHandler_StatusAndCollectionParams: ?status= and ?collection= are
// closed sets. An unknown value is a 400, never an empty or unfiltered list
// that reads as an answer. "paused" is not a status: the dashboard's Paused
// tab sent ?status=paused and always showed nothing (sweep E6), because a
// paused subscription stays active and only its collection pauses.
// "archived" is not a status either: nothing ever wrote it, so it was
// removed before the first release.
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
		"?status=archived":                 http.StatusBadRequest,
		"?status=paused":                   http.StatusBadRequest,
		"?status=past_due":                 http.StatusBadRequest,
		"?status=Active":                   http.StatusBadRequest,
		"?status=cancelled":                http.StatusBadRequest,
		"?collection=paused":               http.StatusOK,
		"?collection=paused&status=active": http.StatusOK,
		"?collection=running":              http.StatusBadRequest,
		"?collection=PAUSED":               http.StatusBadRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/subscriptions"+query, nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.TestTenantIDKey(), tenantID))
		rr := httptest.NewRecorder()
		h.list(rr, req)
		if rr.Code != want {
			t.Errorf("GET /subscriptions%s: got %d, want %d (%s)", query, rr.Code, want, rr.Body.String())
		}
	}
}
