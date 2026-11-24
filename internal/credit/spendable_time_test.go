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

// A spendable balance judges expiry at the time its caller bound (the API's
// door, or the engine's work that reads it), not at a read of its own.
func TestSpendableBalance_JudgesExpiryAtTheDecidedTime(t *testing.T) {
	decidedAt := time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)
	store := &atStore{memStore: newMemStore()}
	svc := NewService(store)
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), decidedAt)
	if _, err := svc.SpendableBalance(ctx, "t1", "cus_1"); err != nil {
		t.Fatalf("SpendableBalance: %v", err)
	}
	if !store.at.Equal(decidedAt) {
		t.Errorf("expiry judged at %v; want %v", store.at, decidedAt)
	}
}
