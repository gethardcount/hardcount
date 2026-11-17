package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// RequireMethod checks write for POST, PUT, PATCH and DELETE and read for
// every other method. An ingest key holds usage:ingest and not
// customer:write, so with those as read and write the read methods pass and
// the write methods get 403.
func TestRequireMethod_PicksThePermissionByMethod(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := RequireMethod(PermUsageIngest, PermCustomerWrite)(ok)
	for method, want := range map[string]int{
		http.MethodGet: http.StatusOK, http.MethodHead: http.StatusOK, http.MethodOptions: http.StatusOK,
		http.MethodPost: http.StatusForbidden, http.MethodPut: http.StatusForbidden,
		http.MethodPatch: http.StatusForbidden, http.MethodDelete: http.StatusForbidden,
	} {
		req := httptest.NewRequest(method, "/", nil)
		req = req.WithContext(WithCaller(req.Context(), Caller{KeyID: "hc_key_1", KeyType: KeyTypeIngest}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: got %d, want %d", method, rec.Code, want)
		}
	}
}
