package usage_test

import (
	"context"
	"encoding/json"
	"errors"
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

// refusingResolver fails every read and counts them: the margin report must
// not decide the customer's time again.
type refusingResolver struct{ reads int }

func (r *refusingResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	r.reads++
	return clock.Sim{}, errors.New("the margin report must not read the customer's clock")
}
func (r *refusingResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (r *refusingResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// The margin report runs on one instant, decided before it: the API's door
// decides it from the customer in the URL, and the report reads no clock of
// the customer's again. On a test clock the default window ends at the
// clock's frozen time and the server's clock is not read; at real time the
// server's clock is read once, and the default window and the rated usage
// share that instant.
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
	resolver := &refusingResolver{}
	f.usageSvc.SetResolver(resolver)
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

	t.Run("on a test clock", func(t *testing.T) {
		frozen := serverAt.Add(-6 * time.Hour)
		server.reads, resolver.reads = 0, 0
		rep := margin(t, clock.WithSim(base, clock.Sim{At: frozen, TestClockID: "clk_1"}))
		if !rep.To.Equal(frozen) || server.reads != 0 || resolver.reads != 0 {
			t.Errorf("window ends %v, server reads %d, resolver reads %d; want %v, none, none", rep.To, server.reads, resolver.reads, frozen)
		}
	})
	t.Run("at real time", func(t *testing.T) {
		server.reads, resolver.reads = 0, 0
		rep := margin(t, clock.WithRealTime(base))
		if !rep.To.Equal(serverAt) || server.reads != 1 || resolver.reads != 0 {
			t.Errorf("window ends %v, server reads %d, resolver reads %d; want %v, one read for the whole report, none", rep.To, server.reads, resolver.reads, serverAt)
		}
	})
}
