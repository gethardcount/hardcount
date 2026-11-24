package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// doorServer is the server's clock in the door's tests: a fixed time,
// counting the reads it answers itself (a bound instant is not a read).
type doorServer struct{ reads int }

var doorServerAt = time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)

func (c *doorServer) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	return doorServerAt
}

// The door binds one read of the server's clock for every routed request but
// a decidedIn one, in either mode and before a mode is known. A decidedIn
// route and a request no route serves get no instant, and the clock is not
// read: the named function binds its own.
func TestClockDoor_BindsOneInstantUnlessAFunctionDecides(t *testing.T) {
	modes := []struct {
		name string
		ctx  context.Context
	}{
		{"test mode", auth.WithTenantID(postgres.WithLivemode(context.Background(), false), "t1")},
		{"live", auth.WithTenantID(postgres.WithLivemode(context.Background(), true), "t1")},
		{"no mode yet", context.Background()},
	}
	for _, c := range []struct {
		name, method, path string
		binds              bool
	}{
		{"a route that names no record", "GET", "/v1/whoami", true},
		{"a route that names a record", "POST", "/v1/subscriptions/sub_1/cancel", true},
		{"a route outside authentication", "POST", "/v1/auth/login", true},
		{"a route its function decides", "POST", "/v1/customers", false},
		{"a public page its function decides", "GET", "/v1/public/invoices/tok_1", false},
		{"a route the router does not have", "GET", "/v1/nothing-here", false},
	} {
		for _, m := range modes {
			t.Run(m.name+", "+c.name, func(t *testing.T) {
				srv := &doorServer{}
				var (
					at             time.Time
					bound, reached bool
				)
				next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					reached = true
					at, bound = clock.EffectiveNow(r.Context())
				})
				req := httptest.NewRequest(c.method, c.path, nil).WithContext(m.ctx)
				begin()((&clockDoor{clock: srv}).Middleware(next)).ServeHTTP(httptest.NewRecorder(), req)
				wantReads := 0
				if c.binds {
					wantReads = 1
				}
				if !reached || bound != c.binds || (bound && !at.Equal(doorServerAt)) || srv.reads != wantReads {
					t.Errorf("reached %v, bound %v at %v after %d server reads; want bound %v after %d",
						reached, bound, at, srv.reads, c.binds, wantReads)
				}
			})
		}
	}
}

// Every route passes through the door, so no handler runs at a time nobody
// decided.
func TestClockDoor_EveryRoutePassesThroughIt(t *testing.T) {
	srv := newTestServer()
	var missing []string
	checked := 0
	if err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		checked++
		if chainIndex(mws, doorMW) < 0 {
			missing = append(missing, method+" "+canonicalRoute(route))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d routes do not pass through the clock door:\n  %s", len(missing), checked, strings.Join(missing, "\n  "))
	}
}
