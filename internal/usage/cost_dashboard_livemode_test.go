package usage

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// fakeTokenLookup resolves the cost-dashboard token to a fixed customer.
// It runs under TxBypass in production, so it intentionally does NOT read
// livemode from ctx — it returns the customer's stored mode instead.
type fakeTokenLookup struct {
	cust domain.Customer
}

func (f fakeTokenLookup) GetByCostDashboardToken(_ context.Context, _ string) (domain.Customer, error) {
	return f.cust, nil
}

// recordingLister captures the livemode it was scoped to via ctx, mirroring
// how a real TxTenant read derives its RLS mode from postgres.Livemode(ctx).
type recordingLister struct {
	gotLivemode bool
	called      bool
}

func (r *recordingLister) List(ctx context.Context, _ subscription.ListFilter) ([]domain.Subscription, int, error) {
	r.called = true
	r.gotLivemode = postgres.Livemode(ctx)
	return nil, 0, nil
}

// TestCostDashboard_PinsCustomerLivemode is the regression test for the
// public cost-dashboard route 500ing for test-mode customers. The route
// arrives with no livemode on ctx; the assembler must pin the resolved
// customer's mode before any TxTenant read. Without the pin,
// postgres.Livemode(ctx) defaults to true (live) and every read for a
// test-mode customer mismatches the RLS livemode predicate.
func TestCostDashboard_PinsCustomerLivemode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		custLivemode bool
	}{
		{name: "test-mode customer pins livemode=false", custLivemode: false},
		{name: "live-mode customer pins livemode=true", custLivemode: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lookup := fakeTokenLookup{cust: domain.Customer{
				ID:       "vlx_cus_test",
				TenantID: "vlx_ten_test",
				Livemode: tt.custLivemode,
			}}
			lister := &recordingLister{}

			// usageService is never reached: with no active subscriptions
			// the assembler short-circuits on the no_subscription branch
			// after pinning livemode, which is exactly the path we assert.
			a := NewCostDashboardAssembler(lookup, nil, lister, &fakeCredits{})

			// ctx deliberately carries NO livemode — the public route is
			// unauthenticated and never set one. The fix must pin it from
			// the resolved customer.
			_, err := a.GetByToken(context.Background(), "vlx_pcd_whatever")
			if err != nil {
				t.Fatalf("GetByToken returned error: %v", err)
			}

			if !lister.called {
				t.Fatal("subscription lister was never called; assembler short-circuited before the pinned-ctx read")
			}
			if lister.gotLivemode != tt.custLivemode {
				t.Fatalf("downstream read scoped to livemode=%v, want %v (livemode not pinned from customer)",
					lister.gotLivemode, tt.custLivemode)
			}
		})
	}
}
