package usage

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// A bare GET /v1/customers/{id}/margin defaulted to the WALL-CLOCK last 30
// days. For a customer on a test clock advanced to 2027-03-01 that window
// (2026-09-16..2026-10-16) predates every simulated event, so revenue and
// cost both read 0 while the usage summary and customer usage follow the
// clock. The default must end at the customer's effective now.
func TestMarginDefaultWindow_FollowsCustomerTestClock(t *testing.T) {
	frozen := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	resolver := &fakeResolver{frozen: map[string]time.Time{"cus_pinned": frozen}}
	svc := NewService(newMemStore())
	svc.SetResolver(resolver)
	cu := NewCustomerUsageService(svc, nil, nil, nil)
	h := NewProviderCostHandler(nil, NewMarginAssembler(nil, cu))

	ctx := postgres.WithLivemode(context.Background(), false) // test clocks are test-mode only
	window := func(url, customerID string) (time.Time, time.Time, error) {
		r := httptest.NewRequest("GET", url, nil).WithContext(ctx)
		return h.marginWindow(r, "t1", customerID)
	}

	t.Run("bare request on a clock-pinned customer covers the simulated last 30 days", func(t *testing.T) {
		from, to, err := window("/v1/customers/cus_pinned/margin", "cus_pinned")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !to.Equal(frozen) {
			t.Errorf("to: got %s, want the clock's frozen_time %s", to.Format(time.RFC3339), frozen.Format(time.RFC3339))
		}
		if want := frozen.AddDate(0, 0, -30); !from.Equal(want) {
			t.Errorf("from: got %s, want %s (30 days before frozen_time)", from.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})

	t.Run("unpinned customer keeps the wall-clock last 30 days", func(t *testing.T) {
		before := time.Now().UTC()
		from, to, err := window("/v1/customers/cus_free/margin", "cus_free")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		after := time.Now().UTC()
		if to.Before(before) || to.After(after) {
			t.Errorf("to: got %s, want wall-clock now", to.Format(time.RFC3339Nano))
		}
		if got := to.Sub(from); got != 30*24*time.Hour {
			t.Errorf("window length: got %s, want 30 days", got)
		}
	})

	t.Run("explicit window does not look up the clock", func(t *testing.T) {
		calls := resolver.calls
		from, to, err := window("/v1/customers/cus_pinned/margin?from=2027-01-01T00:00:00Z&to=2027-02-01T00:00:00Z", "cus_pinned")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !from.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("explicit window changed: %s..%s", from, to)
		}
		if resolver.calls != calls {
			t.Errorf("resolver called %d times for a fully explicit window, want 0", resolver.calls-calls)
		}
	})

	t.Run("only from given: to defaults to the customer's now", func(t *testing.T) {
		_, to, err := window("/v1/customers/cus_pinned/margin?from=2027-02-01T00:00:00Z", "cus_pinned")
		if err != nil {
			t.Fatalf("marginWindow: %v", err)
		}
		if !to.Equal(frozen) {
			t.Errorf("to: got %s, want frozen_time %s", to.Format(time.RFC3339), frozen.Format(time.RFC3339))
		}
	})
}
