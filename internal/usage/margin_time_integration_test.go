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

// The margin report runs on the instant the API's door bound: the default
// window ends at it.
func TestMarginHandler_OneInstantForTheReport(t *testing.T) {
	f := newCustomerUsageFixture(t, "Margin One Instant")
	base := postgres.WithLivemode(context.Background(), false)
	now := time.Now().UTC().Truncate(time.Second)
	cycleStart := now.Add(-24 * time.Hour)
	meter := f.meter(t, base, "tokens")
	custID, _, _ := f.seedCustomerWithSub(t, base, "cus_margin_instant", "pln_margin_instant", []pricing.PriceInput{
		price(meter.ID, 1, nil),
	}, cycleStart, cycleStart.Add(30*24*time.Hour))
	store := usage.NewPostgresStore(f.db)
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

	bound := now.Add(-6 * time.Hour)
	if rep := margin(t, clock.WithEffectiveNow(base, bound)); !rep.To.Equal(bound) {
		t.Errorf("window ends %v; want %v", rep.To, bound)
	}
}
