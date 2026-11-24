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

// boundCtx is a test-mode context carrying the instant the API's door bound.
func boundCtx(at time.Time) context.Context {
	return clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), at)
}

// The customer usage read runs on the instant the API's door bound: the
// rater rates at it.
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

// The default window is picked at the read's instant: of the customer's
// subscriptions, the one whose current period started last, among those
// that have started by then. Here a second subscription's period starts on
// 2026-05-01, after the bound instant (2026-04-12) and before machine time,
// so the window is the first subscription's period only when the pick reads
// the bound instant.
func TestCustomerUsage_TheWindowIsPickedAtTheBoundInstant(t *testing.T) {
	bound := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
	cu, _, subs := readsFixture()
	first := subs.subs[0]
	laterStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	laterEnd := laterStart.AddDate(0, 1, 0)
	subs.subs = append(subs.subs, domain.Subscription{
		ID: "sub_2", TenantID: "t1", CustomerID: "cus_1", Status: domain.SubscriptionActive,
		CurrentBillingPeriodStart: &laterStart, CurrentBillingPeriodEnd: &laterEnd,
		Items: []domain.SubscriptionItem{{ID: "si_2", PlanID: "pln_1"}},
	})
	res, err := cu.Get(boundCtx(bound), "t1", "cus_1", CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !res.Period.From.Equal(*first.CurrentBillingPeriodStart) || !res.Period.To.Equal(*first.CurrentBillingPeriodEnd) {
		t.Errorf("window [%v, %v); want sub_1's period [%v, %v), the one started at the bound instant",
			res.Period.From, res.Period.To, *first.CurrentBillingPeriodStart, *first.CurrentBillingPeriodEnd)
	}
}

// The public cost dashboard runs on the instant the API's door bound, and
// the whole page reads it: the live credit figure and the usage view.
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
