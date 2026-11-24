package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// countingClock is the server's clock in these tests: a fixed time, counting
// the reads it answers itself (a bound instant is not a read).
type countingClock struct{ reads int }

var countingClockAt = time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)

func (c *countingClock) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	return countingClockAt
}

// A request runs on one read of the server's clock, bound before its handler
// runs. With the next test, every routed request runs on one instant.
func TestBindRequestTime_BindsOneReadOfTheServersClock(t *testing.T) {
	srv := &countingClock{}
	var (
		at    time.Time
		bound bool
	)
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		at, bound = clock.EffectiveNow(r.Context())
	})
	req := httptest.NewRequest("POST", "/v1/usage-events/batch", nil)
	bindRequestTime(srv)(next).ServeHTTP(httptest.NewRecorder(), req)
	if !bound || !at.Equal(countingClockAt) || srv.reads != 1 {
		t.Errorf("bound %v at %v after %d server reads; want bound at %v after one", bound, at, srv.reads, countingClockAt)
	}
}

// Every route binds the request's time, so no handler runs at a time nobody
// decided.
func TestBindRequestTime_EveryRoutePassesThroughIt(t *testing.T) {
	srv := newTestServer()
	var missing []string
	checked := 0
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		checked++
		if chainIndex(mws, requestTimeMW) < 0 {
			missing = append(missing, method+" "+canonicalRoute(route))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d routes do not bind the request's time:\n  %s", len(missing), checked, strings.Join(missing, "\n  "))
	}
}
