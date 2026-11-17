package request

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The outer layer reads what an inner layer wrote, although the inner layer
// handed its own handler a new request with a new context.
func TestActorOf_ReadsWhatALayerInsideWrote(t *testing.T) {
	want := Actor{ActorType: "api_key", ActorID: "hc_key_1", TenantID: "hc_ten_1"}
	type innerKey struct{}

	inner := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), innerKey{}, "added by the inner layer")
			SetActor(ctx, want)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	var got Actor
	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithRecord(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
			got = ActorOf(ctx)
		})
	}

	outer(inner(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if got != want {
		t.Errorf("actor = %+v, want %+v", got, want)
	}
}

func TestActorOf_NobodySignedIn(t *testing.T) {
	if got := ActorOf(WithRecord(t.Context())); got != (Actor{}) {
		t.Errorf("actor on a new record = %+v, want the zero Actor", got)
	}
}

// A context that never passed an outer layer has no record: a write does
// nothing and a read answers the zero Actor.
func TestSetActor_NoRecord(t *testing.T) {
	ctx := t.Context()
	SetActor(ctx, Actor{ActorType: "user", ActorID: "hc_usr_1", TenantID: "hc_ten_1"})
	if got := ActorOf(ctx); got != (Actor{}) {
		t.Errorf("actor with no record = %+v, want the zero Actor", got)
	}
}

// Each request gets its own record: a second WithRecord does not share the
// first one's actor.
func TestWithRecord_OnePerRequest(t *testing.T) {
	first := WithRecord(t.Context())
	SetActor(first, Actor{ActorType: "user", ActorID: "hc_usr_1", TenantID: "hc_ten_1"})
	if got := ActorOf(WithRecord(t.Context())); got != (Actor{}) {
		t.Errorf("a second request's actor = %+v, want the zero Actor", got)
	}
}
