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

// needsOf is a route's Need for RequirePermission's test, and whether a route
// serves the request.
type needsOf struct {
	need   Need
	routed bool
}

func (n needsOf) Need(*http.Request) (Need, bool) { return n.need, n.routed }

// RequirePermission checks the route's Need: a permission the caller holds
// (an ingest key holds only usage:ingest), a dashboard session, or any
// caller. A request no route serves passes on to the router's 404 or 405. An
// entry that names nobody is a fault in the table, refused with 500.
func TestRequirePermission_ChecksTheRoutesNeed(t *testing.T) {
	ingest := Caller{TenantID: "t1", KeyID: "hc_key_1", KeyType: KeyTypeIngest}
	session := Caller{TenantID: "t1", UserID: "hc_usr_1"}
	members := Need{Session: "manage team membership"}
	for _, c := range []struct {
		name   string
		needs  needsOf
		caller Caller
		want   int
	}{
		{"no route", needsOf{}, ingest, http.StatusOK},
		{"any caller", needsOf{Need{Any: true}, true}, ingest, http.StatusOK},
		{"a permission held", needsOf{Need{Perm: PermUsageIngest}, true}, ingest, http.StatusOK},
		{"a permission not held", needsOf{Need{Perm: PermCustomerWrite}, true}, ingest, http.StatusForbidden},
		{"session only, a session", needsOf{members, true}, session, http.StatusOK},
		{"session only, a key", needsOf{members, true}, ingest, http.StatusForbidden},
		{"names nobody", needsOf{Need{}, true}, session, http.StatusInternalServerError},
	} {
		ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req = req.WithContext(WithCaller(req.Context(), c.caller))
		rec := httptest.NewRecorder()
		RequirePermission(c.needs)(ok).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}
