package customer

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// instantStore records the instant each customer write's ctx carries: the
// database store stamps the row's updated_at from it.
type instantStore struct {
	*memoryStore
	at []time.Time
}

func (s *instantStore) UpdateTx(ctx context.Context, tx *sql.Tx, c domain.Customer, emailChanged bool) (domain.Customer, error) {
	at, _ := clock.EffectiveNow(ctx)
	s.at = append(s.at, at)
	return s.memoryStore.UpdateTx(ctx, tx, c, emailChanged)
}

func (s *instantStore) UpsertBillingProfileTx(ctx context.Context, tx *sql.Tx, tenantID string, bp domain.CustomerBillingProfile) (domain.CustomerBillingProfile, error) {
	at, _ := clock.EffectiveNow(ctx)
	s.at = append(s.at, at)
	return s.memoryStore.UpsertBillingProfileTx(ctx, tx, tenantID, bp)
}

// custEntry is a customer entry point that writes the customer's rows.
type custEntry struct {
	name string
	run  func(t *testing.T, ctx context.Context, svc *Service, customerID string)
}

func custEntries() []custEntry {
	return []custEntry{
		{"Update", func(t *testing.T, ctx context.Context, svc *Service, id string) {
			if _, err := svc.Update(ctx, "t1", id, UpdateInput{DisplayName: "Renamed Co"}); err != nil {
				t.Fatalf("Update: %v", err)
			}
		}},
		{"UpsertBillingProfile", func(t *testing.T, ctx context.Context, svc *Service, id string) {
			if _, err := svc.UpsertBillingProfile(ctx, "t1", domain.CustomerBillingProfile{CustomerID: id, LegalName: "Renamed Co"}); err != nil {
				t.Fatalf("UpsertBillingProfile: %v", err)
			}
		}},
	}
}

func newCustTimeSvc(t *testing.T) (*Service, *instantStore, string) {
	t.Helper()
	store := &instantStore{memoryStore: newMemoryStore()}
	cust, err := store.Create(context.Background(), "t1", domain.Customer{ExternalID: "cus_time", DisplayName: "Time Co"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return NewService(store), store, cust.ID
}

// A change to a customer runs on its request's instant: the row is stamped
// at the bound instant.
func TestCustomerEntryPoints_RunOnTheBoundInstant(t *testing.T) {
	boundAt := time.Date(2027, 3, 4, 10, 0, 0, 0, time.UTC)
	for _, e := range custEntries() {
		t.Run(e.name, func(t *testing.T) {
			svc, store, id := newCustTimeSvc(t)
			ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), boundAt)
			e.run(t, ctx, svc, id)
			if len(store.at) != 1 || !store.at[0].Equal(boundAt) {
				t.Errorf("the write ran at %v; want one write at %v", store.at, boundAt)
			}
		})
	}
}
