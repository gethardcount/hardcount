package request

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The outer layer reads what an inner layer wrote, although the inner layer
// handed its own handler a new request with a new context.
func TestCallerOf_ReadsWhatALayerInsideWrote(t *testing.T) {
	want := Caller{ActorType: "api_key", ActorID: "hc_key_1", TenantID: "hc_ten_1"}
	type innerKey struct{}

	inner := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), innerKey{}, "added by the inner layer")
			SetCaller(ctx, want)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	var got Caller
	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithRecord(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
			got = CallerOf(ctx)
		})
	}

	outer(inner(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got != want {
		t.Errorf("caller = %+v, want %+v", got, want)
	}
}

func TestCallerOf_NobodySignedIn(t *testing.T) {
	if got := CallerOf(WithRecord(t.Context())); got != (Caller{}) {
		t.Errorf("caller on a new record = %+v, want the zero Caller", got)
	}
}

// A context that never passed an outer layer has no record: a write does
// nothing and a read answers the zero Caller.
func TestSetCaller_NoRecord(t *testing.T) {
	ctx := t.Context()
	SetCaller(ctx, Caller{ActorType: "user", ActorID: "hc_usr_1", TenantID: "hc_ten_1"})
	if got := CallerOf(ctx); got != (Caller{}) {
		t.Errorf("caller with no record = %+v, want the zero Caller", got)
	}
}

// Each request gets its own record: a second WithRecord does not share the
// first one's caller.
func TestWithRecord_OnePerRequest(t *testing.T) {
	first := WithRecord(t.Context())
	SetCaller(first, Caller{ActorType: "user", ActorID: "hc_usr_1", TenantID: "hc_ten_1"})
	if got := CallerOf(WithRecord(t.Context())); got != (Caller{}) {
		t.Errorf("a second request's caller = %+v, want the zero Caller", got)
	}
}
