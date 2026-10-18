package tenant_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/user"
)

// Bootstrap provisions a tenant, an owner account and THREE api keys — one of
// them a LIVE secret that can move money — with no prior actor to attribute
// them to. Until ADR-090's uninstall it left no audit trail at all, behind a
// registry note that claimed otherwise. "Who created this live key, and when?"
// must have an answer, so the provisioning writes its rows into the new
// tenant's own log, on bootstrap's own transaction.
func TestBootstrap_AuditsItsProvisioning(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := audit.NewLogger(db)
	userStore := user.NewPostgresStore(db)

	res, err := tenant.RunBootstrap(ctx, db, tenant.BootstrapDeps{
		HashPassword:  user.HashPassword,
		CreateUserTx:  userStore.CreateInTx,
		SeedDunningTx: dunning.NewService(dunning.NewPostgresStore(db), nil, nil).SeedDefaultPoliciesTx,
		Audit:         logger,
	}, tenant.BootstrapOpts{
		TenantName: "Bootstrap Audit Co",
		OwnerEmail: "owner@bootstrap-audit.test",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Account-plane facts are recorded live, matching POST /v1/tenants.
	liveCtx := postgres.WithLivemode(ctx, true)

	rows, _, err := logger.Query(liveCtx, res.Tenant.ID, audit.QueryFilter{})
	if err != nil {
		t.Fatalf("query the new tenant's audit log: %v", err)
	}

	byResource := map[string][]string{} // resource_type -> resource_ids
	for _, r := range rows {
		if r.Metadata["action"] != "bootstrap_provisioned" {
			continue
		}
		byResource[r.ResourceType] = append(byResource[r.ResourceType], r.ResourceID)
	}

	if got := byResource["tenant"]; len(got) != 1 || got[0] != res.Tenant.ID {
		t.Errorf("tenant row: got %v, want exactly [%s]", got, res.Tenant.ID)
	}
	if got := byResource["user"]; len(got) != 1 || got[0] != res.OwnerUser.ID {
		t.Errorf("owner row: got %v, want exactly [%s]", got, res.OwnerUser.ID)
	}
	// One row per minted credential — this is the whole point: a LIVE secret
	// key must not appear in a running system with no record of its birth.
	if got := byResource["api_key"]; len(got) != 3 {
		t.Errorf("api_key rows: got %d, want 3 (test secret, LIVE secret, test publishable)", len(got))
	}
	// The seeded dunning policies (ADR-123): one row per mode, each in its
	// own mode's log. The live one is here; the test-mode one below.
	if got := byResource["dunning_policy"]; len(got) != 1 {
		t.Errorf("live dunning_policy rows: got %d, want 1", len(got))
	}
	testRows, _, err := logger.Query(postgres.WithLivemode(ctx, false), res.Tenant.ID, audit.QueryFilter{ResourceType: "dunning_policy"})
	if err != nil {
		t.Fatalf("query test-mode audit log: %v", err)
	}
	if len(testRows) != 1 || testRows[0].Metadata["action"] != "bootstrap_provisioned" {
		t.Errorf("test-mode dunning_policy rows: got %+v, want exactly one bootstrap_provisioned row", testRows)
	}

	// The key MATERIAL must never reach the append-only log.
	for _, r := range rows {
		for k, v := range r.Metadata {
			str, ok := v.(string)
			if !ok {
				continue
			}
			for _, secret := range []string{res.LiveSecretKey, res.TestSecretKey} {
				if secret != "" && str == secret {
					t.Fatalf("audit metadata %q leaked raw key material", k)
				}
			}
		}
	}
}
