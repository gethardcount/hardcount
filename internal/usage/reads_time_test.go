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

// readsFixture is the customer usage read with a rater spy and a server
// clock that steps on each read it answers itself.
func readsFixture(server clock.Clock) (*CustomerUsageService, *raterSpy, *fakeSubLister) {
	cu, rater, subs := usageFixture(domain.RatedSubscription{})
	spy := &raterSpy{fakeRater: rater}
	cu.SetRater(spy)
	cu.usage.SetClock(server)
	return cu, spy, subs
}

// The customer usage read runs on the time bound before it (the API's door)
// and binds nothing itself when one is bound: the bound instant, with no read
// of the server's clock; with no instant bound, one read of the server's
// clock for the whole read.
func TestCustomerUsage_ReadsTheDecidedTime(t *testing.T) {
	t.Run("on a bound instant", func(t *testing.T) {
		decided := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
		server := &usageSteppingClock{at: decided.Add(30 * 24 * time.Hour)}
		cu, spy, _ := readsFixture(server)
		ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), decided)
		if _, err := cu.Get(ctx, "t1", "cus_1", CustomerUsagePeriod{}); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(spy.at) != 1 || !spy.at[0].Equal(decided) || server.reads != 0 {
			t.Errorf("rated at %v; server reads %d; want once at %v, none", spy.at, server.reads, decided)
		}
	})
	t.Run("at real time", func(t *testing.T) {
		past := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
		server := &usageSteppingClock{at: past}
		cu, spy, _ := readsFixture(server)
		if _, err := cu.Get(clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), "t1", "cus_1", CustomerUsagePeriod{}); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if want := past.Add(time.Second); len(spy.at) != 1 || !spy.at[0].Equal(want) || server.reads != 1 {
			t.Errorf("rated at %v; server reads %d; want once at %v, one", spy.at, server.reads, want)
		}
	})
}

// The default window is picked at the read's instant: of the customer's
// subscriptions, the one whose current period started last, among those
// that have started by then. Here a second subscription's period starts on
// 2026-05-01: after the decided instant (2026-04-12), and before the
// server's time on the bound-instant row and before machine time on either.
// So the window is the first subscription's period only when the pick reads
// the decided instant.
func TestCustomerUsage_TheWindowIsPickedAtTheDecidedTime(t *testing.T) {
	decided := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name        string
		ctx         context.Context
		serverAhead bool
	}{
		{"on a bound instant (the server is a month ahead)", clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), decided), true},
		{"at real time (the server's clock is read once)", clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := &usageSteppingClock{at: decided}
			if c.serverAhead {
				server.at = decided.Add(30 * 24 * time.Hour)
			}
			cu, _, subs := readsFixture(server)
			first := subs.subs[0]
			laterStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
			laterEnd := laterStart.AddDate(0, 1, 0)
			subs.subs = append(subs.subs, domain.Subscription{
				ID: "sub_2", TenantID: "t1", CustomerID: "cus_1", Status: domain.SubscriptionActive,
				CurrentBillingPeriodStart: &laterStart, CurrentBillingPeriodEnd: &laterEnd,
				Items: []domain.SubscriptionItem{{ID: "si_2", PlanID: "pln_1"}},
			})
			res, err := cu.Get(c.ctx, "t1", "cus_1", CustomerUsagePeriod{})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !res.Period.From.Equal(*first.CurrentBillingPeriodStart) || !res.Period.To.Equal(*first.CurrentBillingPeriodEnd) {
				t.Errorf("window [%v, %v); want sub_1's period [%v, %v), the one started at the decided instant",
					res.Period.From, res.Period.To, *first.CurrentBillingPeriodStart, *first.CurrentBillingPeriodEnd)
			}
		})
	}
}

// The public cost dashboard binds its time once, in GetByToken, and the
// whole page reads it: the live credit figure (which reads the clock, as the
// engine's does for ComputedAt) and the usage view, on one read of the
// server's clock.
func TestCostDashboard_ReadsItsOwnDecision(t *testing.T) {
	past := time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC)
	server := &usageSteppingClock{at: past}
	cu, spy, subs := readsFixture(server)
	a := NewCostDashboardAssembler(fakeTokenLookup{cust: domain.Customer{ID: "cus_1", TenantID: "t1"}}, cu, subs, &fakeCredits{})
	live := &fakeLive{clock: server}
	a.SetLiveBalanceReader(live)
	if _, err := a.GetByToken(context.Background(), "hc_pcd_x"); err != nil {
		t.Fatalf("GetByToken: %v", err)
	}
	want := past.Add(time.Second)
	if len(spy.at) != 1 || !spy.at[0].Equal(want) || !live.at.Equal(want) || server.reads != 1 {
		t.Errorf("rated at %v, live figure at %v; server reads %d; want both at %v, one for the whole page",
			spy.at, live.at, server.reads, want)
	}
}
