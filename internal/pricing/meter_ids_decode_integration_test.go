package pricing_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A plan whose meter_ids does not decode is a broken row, not "no meters".
// Read as empty, every GetPlan reader — the billing usage loop included —
// would see a plan with no meters and bill all its usage at $0.
func TestGetPlan_UndecodableMeterIDsFails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "MeterIDs Decode")

	plan, err := store.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro", Name: "Pro", Currency: "USD",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanDraft,
		BaseAmountCents: 4900, MeterIDs: []string{"mtr_1"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	// Valid JSONB, wrong shape for []string.
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET meter_ids = '{"mtr_1": true}'::jsonb WHERE id = $1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if got, err := store.GetPlan(ctx, tenantID, plan.ID); err == nil {
		t.Fatalf("GetPlan = %d meter(s), nil error; want a decode error", len(got.MeterIDs))
	}
	if _, err := store.ListPlans(ctx, tenantID); err == nil {
		t.Fatal("ListPlans: want a decode error")
	}
}
