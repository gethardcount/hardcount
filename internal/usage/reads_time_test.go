package usage

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// raterSpy records the instant the customer usage read rates on.
type raterSpy struct {
	*fakeRater
	at []time.Time
}

func (r *raterSpy) RateCurrentPeriod(ctx context.Context, sub domain.Subscription) (domain.RatedSubscription, error) {
	at, _ := clock.EffectiveNow(ctx)
	r.at = append(r.at, at)
	return r.fakeRater.RateCurrentPeriod(ctx, sub)
}

// readsFixture is the customer usage read with a rater spy.
func readsFixture() (*CustomerUsageService, *raterSpy, *fakeSubLister) {
	cu, rater, subs := usageFixture(domain.RatedSubscription{})
	spy := &raterSpy{fakeRater: rater}
	cu.SetRater(spy)
	return cu, spy, subs
}

// boundCtx is a test-mode context carrying the request's instant.
func boundCtx(at time.Time) context.Context {
	return clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), at)
}

// The customer usage read runs on the request's instant: the rater rates at
// it.
func TestCustomerUsage_ReadsTheBoundInstant(t *testing.T) {
	bound := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
	cu, spy, _ := readsFixture()
	if _, err := cu.Get(boundCtx(bound), "t1", "cus_1", CustomerUsagePeriod{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(spy.at) != 1 || !spy.at[0].Equal(bound) {
		t.Errorf("rated at %v; want once at %v", spy.at, bound)
	}
}

// The public cost dashboard runs on the request's instant, and the whole
// page reads it: the live credit figure and the usage view.
func TestCostDashboard_ReadsTheBoundInstant(t *testing.T) {
	bound := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
	cu, spy, subs := readsFixture()
	a := NewCostDashboardAssembler(fakeTokenLookup{cust: domain.Customer{ID: "cus_1", TenantID: "t1"}}, cu, subs, &fakeCredits{})
	live := &fakeLive{}
	a.SetLiveBalanceReader(live)
	if _, err := a.GetByToken(boundCtx(bound), "hc_pcd_x"); err != nil {
		t.Fatalf("GetByToken: %v", err)
	}
	if len(spy.at) != 1 || !spy.at[0].Equal(bound) || !live.at.Equal(bound) {
		t.Errorf("rated at %v, live figure at %v; want both at %v", spy.at, live.at, bound)
	}
}
