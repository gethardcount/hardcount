package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// guarded runs a handler behind mw for a principal of key type kt and
// returns the status.
func guarded(t *testing.T, mw func(http.Handler) http.Handler, kt KeyType, method string) int {
	t.Helper()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, "/", nil)
	req = req.WithContext(WithKeyType(req.Context(), kt))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestRequireMethod_GETUsesReadPOSTUsesWrite(t *testing.T) {
	// RequireMethod(read, write) checks read for GET, HEAD and OPTIONS and
	// write for the rest. An ingest key holds usage:ingest and not
	// customer:write, so with those two the read methods pass and the write
	// methods get 403.
	mw := RequireMethod(PermUsageIngest, PermCustomerWrite)
	cases := []struct {
		method string
		want   int
		why    string
	}{
		{"GET", http.StatusOK, "the read permission (usage:ingest) is held"},
		{"HEAD", http.StatusOK, "the read permission is held"},
		{"OPTIONS", http.StatusOK, "the read permission is held"},
		{"POST", http.StatusForbidden, "the write permission (customer:write) is not held"},
		{"PUT", http.StatusForbidden, "the write permission is not held"},
		{"PATCH", http.StatusForbidden, "the write permission is not held"},
		{"DELETE", http.StatusForbidden, "the write permission is not held"},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.why, func(t *testing.T) {
			if code := guarded(t, mw, KeyTypeIngest, tc.method); code != tc.want {
				t.Errorf("%s: got %d, want %d", tc.why, code, tc.want)
			}
		})
	}
}
