package subscription

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestCreate_StartNow_PersistsActivatedAt is the real-Postgres lock on the
// store-INSERT half of the activated_at fix. The CreateWithBill INSERT used to
// omit activated_at from its column list entirely, so an immediately-active
// (started on create) subscription landed with activated_at = NULL even when the
// service set it — the sub then counted in headline MRR (status='active') but
// was INVISIBLE to MRR movement / point-in-time / churn (all key on
// activated_at as the MRR-start event), so the dashboard never reconciled.
//
// The service-layer unit test (TestCreate) can't catch this: its in-memory
// store echoes whatever fields the service passes, so it stays green even with
// the INSERT dropping the column. Only a real round-trip proves persistence.
//
// Mutation-verify: remove `activated_at` from the CreateWithBill INSERT column
// list (postgres.go) — this fails; the unit test does not.
func TestCreate_StartNow_PersistsActivatedAt(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "StartNow ActivatedAt")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_actat", DisplayName: "ActAt",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "actat-monthly", Name: "ActAt Plan", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseAmountCents: 2900,
		Status: domain.PlanActive, MeterIDs: []string{},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	svc := NewService(NewPostgresStore(db), nil)
	sub, err := svc.Create(ctx, tenantID, CreateInput{
		Code: "actat-sub", DisplayName: "ActAt Sub", CustomerID: cust.ID,
		Items: []CreateItemInput{{PlanID: plan.ID}},
	})
	if err != nil {
		t.Fatalf("create a sub that starts now: %v", err)
	}

	// Returned object carries it (RETURNING reads it back from the row).
	if sub.ActivatedAt == nil {
		t.Fatal("returned started sub has nil ActivatedAt — the INSERT dropped the column")
	}

	// And it is actually persisted (read via a fresh bypass tx, not RLS-scoped).
	btx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(btx)
	var hasActivated bool
	var statusStr string
	if err := btx.QueryRow(
		`SELECT status, activated_at IS NOT NULL FROM subscriptions WHERE id = $1`, sub.ID,
	).Scan(&statusStr, &hasActivated); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if statusStr != "active" {
		t.Fatalf("status: got %q, want active", statusStr)
	}
	if !hasActivated {
		t.Fatal("persisted activated_at is NULL for a sub started on create — invisible to MRR movement")
	}

	// A trialing sub, by contrast, must NOT be activated at create time.
	subT, err := svc.Create(ctx, tenantID, CreateInput{
		Code: "actat-trial", DisplayName: "ActAt Trial", CustomerID: cust.ID,
		Items:     []CreateItemInput{{PlanID: plan.ID}},
		TrialDays: 14,
	})
	if err != nil {
		t.Fatalf("create trial sub: %v", err)
	}
	if subT.ActivatedAt != nil {
		t.Error("trialing sub must have nil activated_at until trial-end activation")
	}
}
