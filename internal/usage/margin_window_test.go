package usage

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A bare GET /v1/customers/{id}/margin defaults to the last 30 days ending at
// the report's instant, the one its context carries: for a customer on a test
// clock that is the clock's frozen time (the API's door decided it), so the
// window covers the simulated last 30 days, the timeline customer usage
// follows. An explicit window is used as given.
func TestMarginDefaultWindow_FollowsTheDecidedTime(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	h := NewProviderCostHandler(nil, NewMarginAssembler(nil, NewCustomerUsageService(svc, nil, nil, nil)))
	onClock := clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: frozen, TestClockID: "clk_1"})
	window := func(ctx context.Context, url string) (time.Time, time.Time, error) {
		return h.marginWindow(ctx, httptest.NewRequest("GET", url, nil).WithContext(ctx))
	}

	t.Run("a bare request on a test clock covers the simulated last 30 days", func(t *testing.T) {
		from, to, err := window(onClock, "/v1/customers/cus_pinned/margin")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !to.Equal(frozen) || !from.Equal(frozen.AddDate(0, 0, -30)) {
			t.Errorf("window %s..%s; want the 30 days ending at the clock's frozen time %s", from.Format(time.RFC3339), to.Format(time.RFC3339), frozen.Format(time.RFC3339))
		}
	})

	t.Run("at real time it ends at the instant the report bound", func(t *testing.T) {
		at := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
		from, to, err := window(clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), at), "/v1/customers/cus_free/margin")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !to.Equal(at) || to.Sub(from) != 30*24*time.Hour {
			t.Errorf("window %s..%s; want the 30 days ending at %s", from.Format(time.RFC3339), to.Format(time.RFC3339), at.Format(time.RFC3339))
		}
	})

	t.Run("an explicit window is used as given", func(t *testing.T) {
		from, to, err := window(onClock, "/v1/customers/cus_pinned/margin?from=2027-01-01T00:00:00Z&to=2027-02-01T00:00:00Z")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !from.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("explicit window changed: %s..%s", from, to)
		}
	})

	t.Run("only from given: to defaults to the decided time", func(t *testing.T) {
		_, to, err := window(onClock, "/v1/customers/cus_pinned/margin?from=2027-02-01T00:00:00Z")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !to.Equal(frozen) {
			t.Errorf("to: got %s, want frozen_time %s", to.Format(time.RFC3339), frozen.Format(time.RFC3339))
		}
	})
}

// The margin handler takes the report's instant from the usage service's
// clock, through the margin assembler and customer usage. Each constructor on
// that chain refuses a missing link, so no wiring can build a handler that
// would fail on its first margin request.
func TestMarginChain_EachConstructorRequiresItsLink(t *testing.T) {
	for name, build := range map[string]func(){
		"customer usage without the usage service":    func() { NewCustomerUsageService(nil, nil, nil, nil) },
		"the margin assembler without customer usage": func() { NewMarginAssembler(nil, nil) },
		"the handler without the margin assembler":    func() { NewProviderCostHandler(nil, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("built with a missing link; want a panic naming it")
				}
			}()
			build()
		})
	}
}
