package credit

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// atStore records the instant a spendable-balance read judges expiry at.
type atStore struct {
	*memStore
	at time.Time
}

func (s *atStore) GetSpendableBalance(ctx context.Context, tenantID, customerID string, at time.Time) (map[string]int64, error) {
	s.at = at
	return s.memStore.GetSpendableBalance(ctx, tenantID, customerID, at)
}

// A spendable balance judges expiry at the time its caller decided (the
// API's door from the customer, or the engine's work that reads it), and
// reads no clock of its own.
func TestSpendableBalance_JudgesExpiryAtTheDecidedTime(t *testing.T) {
	decidedAt := time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)
	store := &atStore{memStore: newMemStore()}
	svc := NewService(store)
	res := &customerAnswer{sim: clock.Sim{At: decidedAt.Add(72 * time.Hour), TestClockID: "clk_d"}}
	svc.SetResolver(res)
	ctx := clock.WithSim(postgres.WithLivemode(context.Background(), false), clock.Sim{At: decidedAt, TestClockID: "clk_d"})
	if _, err := svc.SpendableBalance(ctx, "t1", "cus_1"); err != nil {
		t.Fatalf("SpendableBalance: %v", err)
	}
	if !store.at.Equal(decidedAt) || res.reads != 0 {
		t.Errorf("expiry judged at %v, clock reads %d; want %v, none", store.at, res.reads, decidedAt)
	}
}

// At real time a spendable balance reads the server's clock once and judges
// expiry at that instant.
func TestSpendableBalance_AtRealTimeReadsTheServersClockOnce(t *testing.T) {
	past := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	store := &atStore{memStore: newMemStore()}
	svc := NewService(store)
	server := &steppingClock{at: past}
	svc.SetClock(server)
	if _, err := svc.SpendableBalance(clock.WithRealTime(postgres.WithLivemode(context.Background(), false)), "t1", "cus_1"); err != nil {
		t.Fatalf("SpendableBalance: %v", err)
	}
	if want := past.Add(time.Second); server.reads != 1 || !store.at.Equal(want) {
		t.Errorf("server clock read %d times, expiry judged at %v; want one read, %v", server.reads, store.at, want)
	}
}
