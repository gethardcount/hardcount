package usage_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/usage"
)

// marginCountingClock is a server clock that answers a fixed instant to each
// read it answers itself (no instant bound on ctx), and counts those reads.
type marginCountingClock struct {
	at    time.Time
	reads int
}

func (c *marginCountingClock) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	return c.at
}

// The margin report runs on one instant, bound before it by the API's door.
// On a bound instant the default window ends at it and the server's clock is
// not read; with none bound the server's clock is read once, and the default
// window and the rated usage share that instant.
func TestMarginHandler_OneInstantForTheReport(t *testing.T) {
	f := newCustomerUsageFixture(t, "Margin One Instant")
	base := postgres.WithLivemode(context.Background(), false)
	serverAt := time.Now().UTC().Truncate(time.Second)
	cycleStart := serverAt.Add(-24 * time.Hour)
	meter := f.meter(t, base, "tokens")
	custID, _, _ := f.seedCustomerWithSub(t, base, "cus_margin_instant", "pln_margin_instant", []pricing.PriceInput{
		price(meter.ID, 1, nil),
	}, cycleStart, cycleStart.Add(30*24*time.Hour))
	store := usage.NewPostgresStore(f.db)
	server := &marginCountingClock{at: serverAt}
	f.usageSvc.SetClock(server)
	h := usage.NewProviderCostHandler(store, usage.NewMarginAssembler(store, f.custUsage))

	margin := func(t *testing.T, ctx context.Context) usage.MarginReport {
		t.Helper()
		route := chi.NewRouteContext()
		route.URLParams.Add("id", custID)
		ctx = context.WithValue(auth.WithTenantID(ctx, f.tenantID), chi.RouteCtxKey, route)
		rec := httptest.NewRecorder()
		h.Margin(rec, httptest.NewRequest("GET", "/v1/customers/"+custID+"/margin", nil).WithContext(ctx))
		if rec.Code != 200 {
			t.Fatalf("margin: %d %s", rec.Code, rec.Body.String())
		}
		var rep usage.MarginReport
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return rep
	}

	t.Run("on a bound instant", func(t *testing.T) {
		bound := serverAt.Add(-6 * time.Hour)
		server.reads = 0
		rep := margin(t, clock.WithEffectiveNow(base, bound))
		if !rep.To.Equal(bound) || server.reads != 0 {
			t.Errorf("window ends %v, server reads %d; want %v, none", rep.To, server.reads, bound)
		}
	})
	t.Run("at real time", func(t *testing.T) {
		server.reads = 0
		rep := margin(t, clock.WithRealTime(base))
		if !rep.To.Equal(serverAt) || server.reads != 1 {
			t.Errorf("window ends %v, server reads %d; want %v, one read for the whole report", rep.To, server.reads, serverAt)
		}
	})
}
