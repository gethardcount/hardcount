package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// guarded runs h behind mw for a principal of key type kt.
func guarded(t *testing.T, mw func(http.Handler) http.Handler, kt KeyType, method string) (code int, body string, reached bool) {
	t.Helper()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, "/", nil)
	req = req.WithContext(WithKeyType(req.Context(), kt))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), reached
}

func TestRequire_SecretKeyHasFullAccess(t *testing.T) {
	for _, perm := range allPermissions {
		t.Run(string(perm), func(t *testing.T) {
			if code, _, _ := guarded(t, Require(perm), KeyTypeSecret, "GET"); code != http.StatusOK {
				t.Errorf("secret key should have %s, got status %d", perm, code)
			}
		})
	}
}

// An ingest key passes Require(usage:ingest) and gets the ordinary
// missing-permission 403 on everything else, with the same message a
// secret key would get for a permission it lacked.
func TestRequire_IngestKeyOnlySendsUsage(t *testing.T) {
	for _, perm := range allPermissions {
		t.Run(string(perm), func(t *testing.T) {
			code, body, reached := guarded(t, Require(perm), KeyTypeIngest, "POST")
			if perm == PermUsageIngest {
				if code != http.StatusOK || !reached {
					t.Errorf("ingest key on usage:ingest: %d, want 200", code)
				}
				return
			}
			if code != http.StatusForbidden || reached {
				t.Errorf("ingest key on %s: %d (reached=%v), want 403", perm, code, reached)
			}
			want := "insufficient permissions: this key type does not have " + string(perm) + " access"
			if !strings.Contains(body, want) {
				t.Errorf("ingest key on %s: body %s, want the missing-permission message %q", perm, body, want)
			}
		})
	}
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
			if code, _, _ := guarded(t, mw, KeyTypeIngest, tc.method); code != tc.want {
				t.Errorf("%s: got %d, want %d", tc.why, code, tc.want)
			}
		})
	}
}
