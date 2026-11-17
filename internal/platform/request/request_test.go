package request

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A layer outside the one that learned a fact reads it on the record,
// although the inner layer handed its handler a new context: the actor it
// named, and the status the handler wrote.
func TestRecord_OuterLayerReadsWhatAnInnerLayerLearned(t *testing.T) {
	want := Actor{Type: "api_key", ID: "hc_key_1", TenantID: "hc_ten_1"}
	type innerKey struct{}
	var got *Record
	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			got = Of(r.Context())
		})
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), innerKey{}, "added by the inner layer")
		SetActor(ctx, want)
		w.WriteHeader(http.StatusTeapot)
	})
	route := Route{Method: "GET", Pattern: "/v1/things/{id}"}
	Begin(func(*http.Request) Route { return route })(outer(inner)).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/things/thg_1", nil))
	if got.Actor() != want || got.Status() != http.StatusTeapot || got.Route() != route {
		t.Errorf("record: actor %+v, status %d, route %+v; want %+v, 418, %+v", got.Actor(), got.Status(), got.Route(), want, route)
	}
}

// A reader with no record is a mis-wired server: Of panics.
func TestOf_PanicsWithNoRecord(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Of on a context with no record returned; want a panic")
		}
	}()
	Of(t.Context())
}

// A writer with no record does nothing, and does not panic.
func TestSetActor_NoRecord(t *testing.T) {
	SetActor(t.Context(), Actor{Type: "user", ID: "hc_usr_1", TenantID: "hc_ten_1"})
}
