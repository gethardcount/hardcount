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
// the report's instant, the one its context carries. An explicit window is
// used as given.
func TestMarginDefaultWindow_FollowsTheDecidedTime(t *testing.T) {
	decided := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	h := NewProviderCostHandler(nil, NewMarginAssembler(nil, NewCustomerUsageService(svc, nil, nil, nil)))
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), decided)
	window := func(ctx context.Context, url string) (time.Time, time.Time, error) {
		return h.marginWindow(ctx, httptest.NewRequest("GET", url, nil).WithContext(ctx))
	}

	t.Run("it ends at the instant the report bound", func(t *testing.T) {
		from, to, err := window(ctx, "/v1/customers/cus_free/margin")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !to.Equal(decided) || to.Sub(from) != 30*24*time.Hour {
			t.Errorf("window %s..%s; want the 30 days ending at %s", from.Format(time.RFC3339), to.Format(time.RFC3339), decided.Format(time.RFC3339))
		}
	})

	t.Run("an explicit window is used as given", func(t *testing.T) {
		from, to, err := window(ctx, "/v1/customers/cus_free/margin?from=2024-11-01T00:00:00Z&to=2024-12-01T00:00:00Z")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !from.Equal(time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("explicit window changed: %s..%s", from, to)
		}
	})

	t.Run("only from given: to defaults to the decided time", func(t *testing.T) {
		_, to, err := window(ctx, "/v1/customers/cus_free/margin?from=2024-12-01T00:00:00Z")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !to.Equal(decided) {
			t.Errorf("to: got %s, want %s", to.Format(time.RFC3339), decided.Format(time.RFC3339))
		}
	})
}

// The margin report and the cost dashboard take their instant from the usage
// service.s clock, through customer usage (and, for the margin, the margin
// assembler). Each constructor on those chains refuses a missing link, so no
// wiring can build a read that would fail on its first request.
func TestUsageReads_EachConstructorRequiresItsLink(t *testing.T) {
	for name, build := range map[string]func(){
		"customer usage without the usage service":    func() { NewCustomerUsageService(nil, nil, nil, nil) },
		"the margin assembler without customer usage": func() { NewMarginAssembler(nil, nil) },
		"the handler without the margin assembler":    func() { NewProviderCostHandler(nil, nil) },
		"the cost dashboard without customer usage":   func() { NewCostDashboardAssembler(nil, nil, nil, nil) },
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
