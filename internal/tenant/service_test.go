package tenant

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// memoryStore is an in-memory Store implementation for unit tests.
type memoryStore struct {
	tenants map[string]domain.Tenant
}

// newMemoryStore holds one active tenant, hc_ten_test.
func newMemoryStore() *memoryStore {
	return &memoryStore{tenants: map[string]domain.Tenant{
		"hc_ten_test": {ID: "hc_ten_test", Name: "Test", Status: domain.TenantStatusActive},
	}}
}

func (m *memoryStore) Get(_ context.Context, id string) (domain.Tenant, error) {
	t, ok := m.tenants[id]
	if !ok {
		return domain.Tenant{}, errs.ErrNotFound
	}
	return t, nil
}

func (m *memoryStore) UpdateStatus(_ context.Context, id string, status domain.TenantStatus) (domain.Tenant, error) {
	t, ok := m.tenants[id]
	if !ok {
		return domain.Tenant{}, errs.ErrNotFound
	}
	t.Status = status
	m.tenants[id] = t
	return t, nil
}

func TestService_Get(t *testing.T) {
	svc := NewService(newMemoryStore())
	ctx := context.Background()

	t.Run("found", func(t *testing.T) {
		got, err := svc.Get(ctx, "hc_ten_test")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != "hc_ten_test" || got.Name != "Test" {
			t.Errorf("got %+v, want hc_ten_test named Test", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := svc.Get(ctx, "nonexistent")
		if err != errs.ErrNotFound {
			t.Errorf("got error %v, want ErrNotFound", err)
		}
	})

	t.Run("empty id", func(t *testing.T) {
		_, err := svc.Get(ctx, "")
		if err == nil {
			t.Fatal("expected error for empty id")
		}
	})
}

func TestService_UpdateStatus(t *testing.T) {
	svc := NewService(newMemoryStore())

	updated, err := svc.UpdateStatus(context.Background(), "hc_ten_test", domain.TenantStatusSuspended)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.Status != domain.TenantStatusSuspended {
		t.Errorf("got status %q, want %q", updated.Status, domain.TenantStatusSuspended)
	}
}
