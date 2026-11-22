package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// accessOf is a route's Access for RequirePermission's test, and whether a
// route serves the request.
type accessOf struct {
	access Access
	routed bool
}

func (a accessOf) Lookup(*http.Request) (Access, bool) { return a.access, a.routed }

// RequirePermission checks the route's Access: a permission the caller holds
// (an ingest key holds only usage:ingest), a dashboard session, or any
// caller. A request no route serves gets the unrouted handler's answer (the
// router's 404 or 405), never the next handler. An entry that names nobody is
// a fault in the table, refused with 500.
func TestRequirePermission_ChecksTheRoutesAccess(t *testing.T) {
	ingest := Caller{TenantID: "t1", KeyID: "hc_key_1", KeyType: KeyTypeIngest}
	session := Caller{TenantID: "t1", UserID: "hc_usr_1"}
	members := Access{Session: "manage team membership"}
	for _, c := range []struct {
		name   string
		table  accessOf
		caller Caller
		want   int
	}{
		{"no route", accessOf{}, session, http.StatusNotFound},
		{"any caller", accessOf{Access{Any: true}, true}, ingest, http.StatusOK},
		{"a permission held", accessOf{Access{Perm: PermUsageIngest}, true}, ingest, http.StatusOK},
		{"a permission not held", accessOf{Access{Perm: PermCustomerWrite}, true}, ingest, http.StatusForbidden},
		{"session only, a session", accessOf{members, true}, session, http.StatusOK},
		{"session only, a key", accessOf{members, true}, ingest, http.StatusForbidden},
		{"names nobody", accessOf{Access{}, true}, session, http.StatusInternalServerError},
	} {
		ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req = req.WithContext(WithCaller(req.Context(), c.caller))
		rec := httptest.NewRecorder()
		RequirePermission(c.table, http.NotFoundHandler())(ok).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}
