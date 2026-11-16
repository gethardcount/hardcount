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

// readsFixture is the customer usage read with a rater spy, a server clock
// that steps on each read it answers itself, and a counting resolver.
func readsFixture(server clock.Clock) (*CustomerUsageService, *raterSpy, *fakeSubLister, *customerResolver) {
	cu, rater, subs := usageFixture(domain.RatedSubscription{})
	spy := &raterSpy{fakeRater: rater}
	cu.SetRater(spy)
	cu.usage.SetClock(server)
	res := &customerResolver{sims: map[string]clock.Sim{}}
	cu.usage.SetResolver(res)
	return cu, spy, subs, res
}

// The customer usage read runs on the time decided before it (the API's
// door, from the customer in the URL) and decides nothing itself: on a test
// clock, its decided instant, with neither the resolver nor the server's
// clock read; at real time (the door binds no instant), one read of the
// server's clock for the whole read.
func TestCustomerUsage_ReadsTheDecidedTime(t *testing.T) {
	t.Run("on a test clock", func(t *testing.T) {
		decided := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
		server := &usageSteppingClock{at: decided.Add(30 * 24 * time.Hour)}
		cu, spy, _, res := readsFixture(server)
		ctx := clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: decided, TestClockID: "clk_1"})
		if _, err := cu.Get(ctx, "t1", "cus_1", CustomerUsagePeriod{}); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(spy.at) != 1 || !spy.at[0].Equal(decided) || res.reads != 0 || server.reads != 0 {
			t.Errorf("rated at %v; resolver reads %d, server reads %d; want once at %v, none, none", spy.at, res.reads, server.reads, decided)
		}
	})
	t.Run("at real time", func(t *testing.T) {
		past := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
		server := &usageSteppingClock{at: past}
		cu, spy, _, res := readsFixture(server)
		if _, err := cu.Get(clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), "t1", "cus_1", CustomerUsagePeriod{}); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if want := past.Add(time.Second); len(spy.at) != 1 || !spy.at[0].Equal(want) || res.reads != 0 || server.reads != 1 {
			t.Errorf("rated at %v; resolver reads %d, server reads %d; want once at %v, none, one", spy.at, res.reads, server.reads, want)
		}
	})
}

// The public cost dashboard decides its time once, in GetByToken, from the
// customer its token names, and its usage view reads that decision: on a
// test clock, the clock's frozen time; for a customer on no clock, one read
// of the server's clock. The resolver is read once either way.
func TestCostDashboard_ReadsItsOwnDecision(t *testing.T) {
	frozen := time.Date(2026, 4, 20, 6, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name        string
		sim         clock.Sim
		want        func(past time.Time) time.Time
		serverReads int
	}{
		{"a customer on a test clock", clock.Sim{At: frozen, TestClockID: "clk_1"}, func(time.Time) time.Time { return frozen }, 0},
		{"a customer on no clock", clock.Sim{At: time.Now().UTC()}, func(past time.Time) time.Time { return past.Add(time.Second) }, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			past := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
			server := &usageSteppingClock{at: past}
			cu, spy, subs, res := readsFixture(server)
			res.sims["cus_1"] = c.sim
			a := NewCostDashboardAssembler(fakeTokenLookup{cust: domain.Customer{ID: "cus_1", TenantID: "t1"}}, cu, subs, &fakeCredits{})
			a.SetLiveBalanceReader(&fakeLive{})
			a.SetResolver(res)
			if _, err := a.GetByToken(context.Background(), "hc_pcd_x"); err != nil {
				t.Fatalf("GetByToken: %v", err)
			}
			if want := c.want(past); len(spy.at) != 1 || !spy.at[0].Equal(want) || res.reads != 1 || server.reads != c.serverReads {
				t.Errorf("rated at %v; resolver reads %d, server reads %d; want once at %v, one, %d", spy.at, res.reads, server.reads, want, c.serverReads)
			}
		})
	}
}
