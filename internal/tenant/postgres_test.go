package tenant_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

func TestPostgresStore_CreateAndGet(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := tenant.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	created, err := store.Create(ctx, domain.Tenant{Name: "Acme Corp"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("ID should be generated")
	}
	if created.Name != "Acme Corp" {
		t.Errorf("name: got %q, want Acme Corp", created.Name)
	}
	if created.Status != domain.TenantStatusActive {
		t.Errorf("status: got %q, want active", created.Status)
	}

	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("get ID mismatch: got %q, want %q", got.ID, created.ID)
	}
	if got.Name != "Acme Corp" {
		t.Errorf("get name: got %q", got.Name)
	}
}

func TestPostgresStore_GetNotFound(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := tenant.NewPostgresStore(db)

	_, err := store.Get(postgres.WithLivemode(context.Background(), false), "nonexistent")
	if err != errs.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestSettingsStore_GetSynthesizesDefaultsOnMiss covers the
// data-layer fix for orphan tenants without settings rows. Get
// must return Velox defaults (no ErrNotFound) so the engine
// path never sees the missing-row case.
func TestSettingsStore_GetSynthesizesDefaultsOnMiss(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := tenant.NewSettingsStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Settings Defaults Tenant")

	// Note: CreateTestTenant doesn't seed tenant_settings — bootstrap
	// path does that, but the test fixture intentionally leaves it
	// empty so we can exercise the synthesize-defaults branch.

	got, err := store.Get(postgres.WithLivemode(context.Background(), false), tenantID)
	if err != nil {
		t.Fatalf("Get on missing settings should synthesize defaults, got error: %v", err)
	}
	want := tenant.DefaultSettings(tenantID)
	if got.TenantID != want.TenantID ||
		got.DefaultCurrency != want.DefaultCurrency ||
		got.Timezone != want.Timezone ||
		got.InvoicePrefix != want.InvoicePrefix ||
		got.NetPaymentTerms != want.NetPaymentTerms ||
		got.TaxProvider != want.TaxProvider ||
		got.TaxOnFailure != want.TaxOnFailure {
		t.Errorf("synthesized defaults mismatch:\n  got  %+v\n  want %+v", got, want)
	}
}

func TestPostgresStore_List(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := tenant.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	_, _ = store.Create(ctx, domain.Tenant{Name: "Tenant A"})
	_, _ = store.Create(ctx, domain.Tenant{Name: "Tenant B"})

	all, total, err := store.List(ctx, tenant.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 || total != 2 {
		t.Errorf("list count: got %d rows, total %d, want 2", len(all), total)
	}

	// Filter by status
	active, _, err := store.List(ctx, tenant.ListFilter{Status: "active"})
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active count: got %d, want 2", len(active))
	}
}

func TestPostgresStore_UpdateStatus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := tenant.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	created, _ := store.Create(ctx, domain.Tenant{Name: "Test"})

	updated, err := store.UpdateStatus(ctx, created.ID, domain.TenantStatusSuspended)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != domain.TenantStatusSuspended {
		t.Errorf("status: got %q, want suspended", updated.Status)
	}

	// Verify persisted
	got, _ := store.Get(ctx, created.ID)
	if got.Status != domain.TenantStatusSuspended {
		t.Errorf("persisted status: got %q, want suspended", got.Status)
	}
}
