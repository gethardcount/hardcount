package tenant_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

func TestPostgresStore_Get(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := tenant.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	id := testutil.CreateTestTenant(t, db, "Acme Corp")

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id || got.Name != "Acme Corp" || got.Status != domain.TenantStatusActive {
		t.Errorf("get: %+v, want %s, Acme Corp, active", got, id)
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
// must return Hardcount defaults (no ErrNotFound) so the engine
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

func TestPostgresStore_UpdateStatus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := tenant.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	id := testutil.CreateTestTenant(t, db, "Test")

	updated, err := store.UpdateStatus(ctx, id, domain.TenantStatusSuspended)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Status != domain.TenantStatusSuspended {
		t.Errorf("status: got %q, want suspended", updated.Status)
	}

	// Verify persisted
	got, _ := store.Get(ctx, id)
	if got.Status != domain.TenantStatusSuspended {
		t.Errorf("persisted status: got %q, want suspended", got.Status)
	}
}
