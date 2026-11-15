package credit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// customerAnswer answers every customer with sim, or fails with err, and
// counts the reads.
type customerAnswer struct {
	sim   clock.Sim
	err   error
	reads int
}

func (a *customerAnswer) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	a.reads++
	return a.sim, a.err
}
func (a *customerAnswer) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (a *customerAnswer) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// A grant and an adjustment of either sign decide their time from the
// customer: an unreadable clock refuses with nothing written, a not-found
// writes at real time, and live mode reads nothing.
func TestGrantAndAdjust_DecideFromTheCustomer(t *testing.T) {
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name string
		seed int64 // a grant made before the resolver is set
		do   func(*Service, context.Context) (time.Time, error)
	}{
		{"grant", 0, func(s *Service, ctx context.Context) (time.Time, error) {
			e, err := s.Grant(ctx, "t1", GrantInput{Currency: "USD", CustomerID: "cus_1", AmountCents: 500, Description: "Goodwill"})
			return e.CreatedAt, err
		}},
		{"positive adjust", 0, func(s *Service, ctx context.Context) (time.Time, error) {
			e, err := s.Adjust(ctx, "t1", AdjustInput{Currency: "USD", CustomerID: "cus_1", AmountCents: 500, Description: "Credit"})
			return e.CreatedAt, err
		}},
		{"negative adjust", 1000, func(s *Service, ctx context.Context) (time.Time, error) {
			e, err := s.Adjust(ctx, "t1", AdjustInput{Currency: "USD", CustomerID: "cus_1", AmountCents: -300, Description: "Deduct"})
			return e.CreatedAt, err
		}},
	} {
		setup := func(t *testing.T, res clock.Resolver) (*Service, *memStore) {
			t.Helper()
			store := newMemStore()
			svc := NewService(store)
			if c.seed > 0 {
				if _, err := svc.Grant(testMode, "t1", GrantInput{Currency: "USD", CustomerID: "cus_1", AmountCents: c.seed, Description: "seed"}); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			svc.SetResolver(res)
			return svc, store
		}
		t.Run(c.name+": an unreadable clock refuses", func(t *testing.T) {
			svc, store := setup(t, &customerAnswer{err: errors.New("connection reset by peer")})
			before := len(store.entries)
			_, err := c.do(svc, testMode)
			if !errors.Is(err, errs.ErrUnavailable) || !errors.Is(err, clock.ErrUndecided) {
				t.Fatalf("err %v; want unavailable over clock.ErrUndecided", err)
			}
			if len(store.entries) != before {
				t.Errorf("%d ledger entries; want %d (nothing written)", len(store.entries), before)
			}
		})
		t.Run(c.name+": a not-found writes at real time", func(t *testing.T) {
			svc, _ := setup(t, &customerAnswer{err: fmt.Errorf("read test clock clk_gone: %w", errs.ErrNotFound)})
			from := time.Now().UTC()
			at, err := c.do(svc, testMode)
			if err != nil || at.Before(from) || at.After(time.Now().UTC()) {
				t.Errorf("err %v, created_at %v; want written at real time", err, at)
			}
		})
		t.Run(c.name+": the customer's clock", func(t *testing.T) {
			frozen := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
			svc, _ := setup(t, &customerAnswer{sim: clock.Sim{At: frozen, TestClockID: "clk_1"}})
			if at, err := c.do(svc, testMode); err != nil || !at.Equal(frozen) {
				t.Errorf("err %v, created_at %v; want the clock's %v", err, at, frozen)
			}
		})
		t.Run(c.name+": live mode reads nothing", func(t *testing.T) {
			res := &customerAnswer{err: errors.New("must not be read")}
			svc, _ := setup(t, res)
			if _, err := c.do(svc, postgres.WithLivemode(context.Background(), true)); err != nil || res.reads != 0 {
				t.Errorf("err %v, reads %d; want written with no read", err, res.reads)
			}
		})
	}
}

// steppingClock moves a second each time it answers a read itself (no
// instant bound on ctx), and counts those reads.
type steppingClock struct {
	at    time.Time
	reads int
}

func (c *steppingClock) Now(ctx context.Context) time.Time {
	if t, ok := clock.EffectiveNow(ctx); ok {
		return t
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// A grant reads one instant, from the service's clock: for a customer on no
// clock and in live mode, the server's clock is read once, and the expiry
// check and the ledger entry both read that instant. A grant expiring an hour
// after the server's time, long before the machine's, is accepted.
func TestGrant_ReadsOneInstantFromTheServersClock(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	for name, ctx := range map[string]context.Context{
		"on no clock": postgres.WithLivemode(context.Background(), false),
		"live mode":   postgres.WithLivemode(context.Background(), true),
	} {
		t.Run(name, func(t *testing.T) {
			server := &steppingClock{at: past}
			svc := NewService(newMemStore())
			svc.SetClock(server)
			svc.SetResolver(&customerAnswer{sim: clock.Sim{At: time.Now().UTC()}})
			expires := past.Add(time.Hour)
			e, err := svc.Grant(ctx, "t1", GrantInput{Currency: "USD", CustomerID: "cus_1", AmountCents: 500, Description: "Goodwill", ExpiresAt: &expires})
			if err != nil {
				t.Fatalf("grant: %v", err)
			}
			if want := past.Add(time.Second); server.reads != 1 || !e.CreatedAt.Equal(want) {
				t.Errorf("server clock read %d times, created_at %v; want one read, the entry at %v", server.reads, e.CreatedAt, want)
			}
		})
	}
}
