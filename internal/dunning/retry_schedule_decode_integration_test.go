package dunning_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A policy whose retry_schedule does not decode is a broken row, not "no
// retries". Read as empty, a run charges and then fails its schedule bounds
// check without advancing next_action_at, so it charges again every tick.
func TestGetPolicy_UndecodableRetryScheduleFails(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := dunning.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Retry Schedule Decode")

	p, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, MaxRetryAttempts: 3, GracePeriodDays: 3,
		RetrySchedule:           []string{"72h", "120h"},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	// Valid JSONB, wrong shape for []string.
	if _, err := tx.ExecContext(ctx, `UPDATE dunning_policies SET retry_schedule = '{"first": "72h"}'::jsonb WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if got, err := store.GetPolicyByID(ctx, tenantID, p.ID); err == nil {
		t.Fatalf("GetPolicyByID = %d retries, nil error; want a decode error", len(got.RetrySchedule))
	}
	if _, err := store.GetDefaultPolicy(ctx, tenantID); err == nil {
		t.Fatal("GetDefaultPolicy: want a decode error")
	}
}
