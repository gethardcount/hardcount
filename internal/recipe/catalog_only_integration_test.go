package recipe

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestService_Instantiate_TouchesOnlyTheCatalog: an install creates meters,
// a plan and its prices, and nothing else (ADR-130 §15, approved 2026-10-23).
// It creates no dunning policy, so the policy the account was created with
// stays the default (ADR-123), and no webhook endpoint. The install record
// and its audit row name only catalog objects.
func TestService_Instantiate_TouchesOnlyTheCatalog(t *testing.T) {
	f := newRecipeFixture(t)
	tenantID := testutil.CreateTestTenant(t, f.db, "catalog only")
	ctx := postgres.WithLivemode(context.Background(), false)
	dunningSvc := dunning.NewService(dunning.NewPostgresStore(f.db), nil, nil)

	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := dunningSvc.SeedDefaultPoliciesTx(ctx, tx, tenantID, nil); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	for _, key := range []string{"anthropic_style", "openai_style", "replicate_style"} {
		inst, err := f.svc.Instantiate(ctx, tenantID, key, nil, InstantiateOptions{})
		if err != nil {
			t.Fatalf("Instantiate %s: %v", key, err)
		}
		b, err := json.Marshal(inst.CreatedObjects)
		if err != nil {
			t.Fatal(err)
		}
		var objs map[string]any
		if err := json.Unmarshal(b, &objs); err != nil {
			t.Fatal(err)
		}
		for k := range objs {
			if k != "meter_ids" && k != "plan_ids" && k != "price_ids" {
				t.Errorf("%s: created_objects has %q", key, k)
			}
		}
	}

	if n := countRows(t, f.db, tenantID, "dunning_policies"); n != 1 {
		t.Errorf("dunning_policies in test mode: %d, want 1 (the seeded one)", n)
	}
	def, err := dunningSvc.GetDefaultPolicy(ctx, tenantID)
	if err != nil {
		t.Fatalf("default policy: %v", err)
	}
	if !def.Seeded {
		t.Errorf("default policy = %q, want the seeded one", def.Name)
	}
	if n := countRows(t, f.db, tenantID, "webhook_endpoints"); n != 0 {
		t.Errorf("webhook_endpoints: %d, want 0", n)
	}
	if n := countRows(t, f.db, tenantID, "plans"); n != 3 {
		t.Errorf("plans: %d, want 3", n)
	}
}
