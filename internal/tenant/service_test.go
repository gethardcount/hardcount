package tenant

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// memoryStore is an in-memory Store implementation for unit tests.
type memoryStore struct {
	tenants map[string]domain.Tenant
}

func newMemoryStore() *memoryStore {
	return &memoryStore{tenants: make(map[string]domain.Tenant)}
}

// CreateAudited mirrors the real store's shared-fate contract: emission
// error rolls the in-memory create back.
func (m *memoryStore) CreateAudited(ctx context.Context, t domain.Tenant, emit func(tx *sql.Tx, out domain.Tenant) error) (domain.Tenant, error) {
	out, err := m.Create(ctx, t)
	if err != nil {
		return out, err
	}
	if emit != nil {
		if err := emit(nil, out); err != nil {
			delete(m.tenants, out.ID)
			return domain.Tenant{}, err
		}
	}
	return out, nil
}

func (m *memoryStore) Create(_ context.Context, t domain.Tenant) (domain.Tenant, error) {
	t.ID = "vlx_ten_test"
	t.Status = domain.TenantStatusActive
	m.tenants[t.ID] = t
	return t, nil
}

func (m *memoryStore) Get(_ context.Context, id string) (domain.Tenant, error) {
	t, ok := m.tenants[id]
	if !ok {
		return domain.Tenant{}, errs.ErrNotFound
	}
	return t, nil
}

func (m *memoryStore) List(_ context.Context, _ ListFilter) ([]domain.Tenant, error) {
	var result []domain.Tenant
	for _, t := range m.tenants {
		result = append(result, t)
	}
	return result, nil
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

// newTestService wires a no-op dunning seeder: Create refuses to run
// without one. The real seed is pinned against Postgres in
// create_audit_integration_test.go.
func newTestService(store Store) *Service {
	svc := NewService(store)
	svc.SetDunningSeeder(func(context.Context, *sql.Tx, string, func(domain.DunningPolicy, bool) error) error { return nil })
	return svc
}

// Create must not make an account without its dunning policies (ADR-123):
// an unwired seeder is refused, and a failing seed creates nothing.
func TestService_Create_DunningSeedRequired(t *testing.T) {
	ctx := context.Background()

	t.Run("unwired seeder refuses", func(t *testing.T) {
		store := newMemoryStore()
		svc := NewService(store)
		if _, err := svc.Create(ctx, CreateInput{Name: "Acme"}); err == nil {
			t.Fatal("Create must fail when the dunning seeder is not wired")
		}
		if len(store.tenants) != 0 {
			t.Fatalf("no tenant may be created without the seed; got %d", len(store.tenants))
		}
	})

	t.Run("seed failure creates nothing", func(t *testing.T) {
		store := newMemoryStore()
		svc := NewService(store)
		svc.SetDunningSeeder(func(context.Context, *sql.Tx, string, func(domain.DunningPolicy, bool) error) error {
			return errors.New("injected seed failure")
		})
		if _, err := svc.Create(ctx, CreateInput{Name: "Acme"}); err == nil {
			t.Fatal("Create must fail when the seed fails")
		}
		if len(store.tenants) != 0 {
			t.Fatalf("tenant leaked from a failed seed: %d", len(store.tenants))
		}
	})

	t.Run("seed runs for the new tenant", func(t *testing.T) {
		svc := NewService(newMemoryStore())
		var seededFor string
		svc.SetDunningSeeder(func(_ context.Context, _ *sql.Tx, tenantID string, _ func(domain.DunningPolicy, bool) error) error {
			seededFor = tenantID
			return nil
		})
		created, err := svc.Create(ctx, CreateInput{Name: "Acme"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if seededFor != created.ID {
			t.Fatalf("seed ran for %q, want the new tenant %q", seededFor, created.ID)
		}
	})
}

func TestService_Create(t *testing.T) {
	svc := newTestService(newMemoryStore())
	ctx := context.Background()

	t.Run("valid name", func(t *testing.T) {
		tenant, err := svc.Create(ctx, CreateInput{Name: "Acme Corp"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tenant.Name != "Acme Corp" {
			t.Errorf("got name %q, want %q", tenant.Name, "Acme Corp")
		}
		if tenant.Status != domain.TenantStatusActive {
			t.Errorf("got status %q, want %q", tenant.Status, domain.TenantStatusActive)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		_, err := svc.Create(ctx, CreateInput{Name: ""})
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})

	t.Run("whitespace name", func(t *testing.T) {
		_, err := svc.Create(ctx, CreateInput{Name: "   "})
		if err == nil {
			t.Fatal("expected error for whitespace name")
		}
	})
}

func TestService_Get(t *testing.T) {
	store := newMemoryStore()
	svc := newTestService(store)
	ctx := context.Background()

	created, _ := svc.Create(ctx, CreateInput{Name: "Test"})

	t.Run("found", func(t *testing.T) {
		got, err := svc.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ID != created.ID {
			t.Errorf("got id %q, want %q", got.ID, created.ID)
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
	store := newMemoryStore()
	svc := newTestService(store)
	ctx := context.Background()

	created, _ := svc.Create(ctx, CreateInput{Name: "Test"})

	updated, err := svc.UpdateStatus(ctx, created.ID, domain.TenantStatusSuspended)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.Status != domain.TenantStatusSuspended {
		t.Errorf("got status %q, want %q", updated.Status, domain.TenantStatusSuspended)
	}
}
