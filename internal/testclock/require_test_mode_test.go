package testclock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A live-mode caller never reaches a test-clock route: requireTestMode
// answers 403. The table's CHECK (livemode = false) is the second refusal.
func TestRequireTestMode_RefusesALiveCaller(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	for _, c := range []struct {
		live bool
		code int
	}{{true, http.StatusForbidden}, {false, http.StatusOK}} {
		reached = false
		req := httptest.NewRequest(http.MethodPost, "/v1/test-clocks", nil).WithContext(postgres.WithLivemode(context.Background(), c.live))
		rec := httptest.NewRecorder()
		(&Handler{}).requireTestMode(next).ServeHTTP(rec, req)
		if rec.Code != c.code || reached == c.live {
			t.Errorf("live=%v: code %d, reached the route %v; want %d and %v", c.live, rec.Code, reached, c.code, !c.live)
		}
	}
}
