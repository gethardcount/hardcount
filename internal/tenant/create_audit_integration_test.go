package tenant_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

type failingEmitter struct{}

func (failingEmitter) LogInTx(_ context.Context, _ *sql.Tx, _ audit.Entry) error {
	return errors.New("injected audit failure")
}

// Platform tenant creation previously left NO audit trail: the deleted catch-all
// was mounted only on the /v1 block, and /v1/tenants sits outside it (ADR-090
// RC1 — coverage anchored to transport). Panel Q6: the create row lands in the
// NEW tenant's own log, in the same tx as the tenants INSERT — pinned in both
// shared-fate directions here. This emission is also what accounts for the
// request to the coverage detector; nothing else would.
func TestTenantCreate_AuditSharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := tenant.NewPostgresStore(db)
	logger := audit.NewLogger(db)
	seeder := dunning.NewService(dunning.NewPostgresStore(db), nil, nil).SeedDefaultPoliciesTx

	t.Run("create lands tenant and audit row together, in the new tenant's log", func(t *testing.T) {
		svc := tenant.NewService(store)
		svc.SetAuditLogger(logger)
		svc.SetDunningSeeder(seeder)

		created, err := svc.Create(ctx, tenant.CreateInput{Name: "Audit Trail Co"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// The row is queryable AS the new tenant, in the live plane (tenant
		// provisioning is an account-plane fact recorded live by design).
		rows, _, err := logger.Query(postgres.WithLivemode(ctx, true), created.ID, audit.QueryFilter{
			ResourceType: "tenant", ResourceID: created.ID,
		})
		if err != nil {
			t.Fatalf("query audit as new tenant: %v", err)
		}
		if len(rows) != 1 || rows[0].Action != "create" {
			t.Fatalf("want one 'create' tenant audit row in the NEW tenant's log; got %+v", rows)
		}
		if rows[0].ResourceLabel != "Audit Trail Co" {
			t.Errorf("resource_label: got %q, want the tenant name", rows[0].ResourceLabel)
		}
	})

	t.Run("audit failure rolls the tenant creation back", func(t *testing.T) {
		svc := tenant.NewService(store)
		svc.SetAuditLogger(failingEmitter{})
		svc.SetDunningSeeder(seeder)

		_, err := svc.Create(ctx, tenant.CreateInput{Name: "Never Provisioned Co"})
		if err == nil {
			t.Fatal("create must fail when its audit emission fails (shared fate)")
		}
		var count int
		if err := db.Pool.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tenants WHERE name = 'Never Provisioned Co'`,
		).Scan(&count); err != nil {
			t.Fatalf("count tenants: %v", err)
		}
		if count != 0 {
			t.Errorf("tenant row leaked from a rolled-back audited create: %d rows", count)
		}
	})
}
