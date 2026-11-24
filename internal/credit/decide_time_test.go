package credit

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

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

// A grant reads one instant, from the service's clock: the server's clock is
// read once, and the expiry check and the ledger entry both read that
// instant. A grant expiring an hour after the server's time, long before the
// machine's, is accepted.
func TestGrant_ReadsOneInstantFromTheServersClock(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	for name, ctx := range map[string]context.Context{
		"test mode": postgres.WithLivemode(context.Background(), false),
		"live mode": postgres.WithLivemode(context.Background(), true),
	} {
		t.Run(name, func(t *testing.T) {
			server := &steppingClock{at: past}
			svc := NewService(newMemStore())
			svc.SetClock(server)
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
