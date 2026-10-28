package recipe

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestPostgresStore_CreateAndGet exercises the round-trip: insert a row
// inside a Tx, read it back via the convenience GetByKey wrapper, verify
// every field hydrates including the nested CreatedObjects JSONB.
func TestPostgresStore_CreateAndGet(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "recipe test")
	store := NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)

	created, err := store.CreateTx(ctx, tx, domain.RecipeInstance{
		TenantID:      tenantID,
		RecipeKey:     "anthropic_style",
		RecipeVersion: "1.0.0",
		Overrides:     map[string]any{"currency": "USD", "plan_name": "AI API"},
		CreatedObjects: domain.CreatedObjects{
			MeterIDs: []string{"hc_mtr_aaa"},
			PlanIDs:  []string{"hc_pln_bbb"},
			PriceIDs: []string{"hc_prc_ccc"},
		},
		CreatedBy: "operator@example.com",
	})
	if err != nil {
		t.Fatalf("CreateTx: %v", err)
	}
	if created.ID == "" {
		t.Error("expected ID to be populated")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := store.GetByKey(ctx, tenantID, "anthropic_style")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("ID mismatch: got %q want %q", got.ID, created.ID)
	}
	if got.RecipeVersion != "1.0.0" {
		t.Errorf("Version: got %q", got.RecipeVersion)
	}
	if got.Overrides["currency"] != "USD" {
		t.Errorf("Overrides: got %v", got.Overrides)
	}
	if len(got.CreatedObjects.PriceIDs) != 1 || got.CreatedObjects.PriceIDs[0] != "hc_prc_ccc" {
		t.Errorf("CreatedObjects.PriceIDs: got %v", got.CreatedObjects.PriceIDs)
	}
	if got.CreatedBy != "operator@example.com" {
		t.Errorf("CreatedBy: got %q", got.CreatedBy)
	}
}

// TestPostgresStore_GetByKeyNotFound asserts the unambiguous miss signal so
// Service.Instantiate can branch on errs.ErrNotFound without sniffing
// sql.ErrNoRows directly.
func TestPostgresStore_GetByKeyNotFound(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "recipe miss")
	store := NewPostgresStore(db)

	_, err := store.GetByKey(postgres.WithLivemode(context.Background(), false), tenantID, "openai_style")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestPostgresStore_UniqueByTenantKey enforces the idempotency guarantee
// at the schema layer: two CreateTx calls with the same (tenant, key)
// must fail at INSERT (UNIQUE constraint), independent of any Service-
// level pre-check that might race.
func TestPostgresStore_UniqueByTenantKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "recipe unique")
	store := NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)

	tx1, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.CreateTx(ctx, tx1, domain.RecipeInstance{
		TenantID: tenantID, RecipeKey: "anthropic_style", RecipeVersion: "1.0.0",
	}); err != nil {
		t.Fatalf("first CreateTx: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx2, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	defer postgres.Rollback(tx2)
	if _, err := store.CreateTx(ctx, tx2, domain.RecipeInstance{
		TenantID: tenantID, RecipeKey: "anthropic_style", RecipeVersion: "1.0.0",
	}); err == nil {
		t.Fatal("expected UNIQUE-violation error on duplicate (tenant, key) insert")
	}
}
