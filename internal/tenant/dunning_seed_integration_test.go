package tenant_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/user"
)

func bootstrapDeps(db *postgres.DB, seed tenant.DunningSeedFunc) tenant.BootstrapDeps {
	return tenant.BootstrapDeps{
		HashPassword:  user.HashPassword,
		CreateUserTx:  user.NewPostgresStore(db).CreateInTx,
		SeedDunningTx: seed,
		Audit:         audit.NewLogger(db),
	}
}

func modePolicies(t *testing.T, svc *dunning.Service, tenantID string, livemode bool) []domain.DunningPolicy {
	t.Helper()
	ps, err := svc.ListPolicies(postgres.WithLivemode(context.Background(), livemode), tenantID)
	if err != nil {
		t.Fatalf("list policies (livemode=%v): %v", livemode, err)
	}
	return ps
}

// assertSeeded checks that each mode has exactly one policy, the enabled
// seeded default, with final actions none/none (not the pause an empty
// value would normalize to).
func assertSeeded(t *testing.T, svc *dunning.Service, tenantID string) {
	t.Helper()
	for _, live := range []bool{false, true} {
		ps := modePolicies(t, svc, tenantID, live)
		if len(ps) != 1 {
			t.Fatalf("livemode=%v: %d policies, want exactly 1", live, len(ps))
		}
		p := ps[0]
		if !p.IsDefault || !p.Enabled || !p.Seeded || p.Name != "Default retries" {
			t.Errorf("livemode=%v: %+v, want the enabled seeded default \"Default retries\"", live, p)
		}
		if p.FinalSubscriptionAction != domain.SubActionNone || p.FinalInvoiceAction != domain.InvActionNone {
			t.Errorf("livemode=%v: final actions %q/%q, want none/none", live, p.FinalSubscriptionAction, p.FinalInvoiceAction)
		}
	}
}

// TestBootstrap_SeedsDefaultDunningPolicy is #1152: a new account retries a
// failed payment without anyone first creating a policy.
//
// Mutation checks: drop SeedDunningTx from RunBootstrap (no policies);
// make its error non-fatal (the failure subtest leaks rows).
func TestBootstrap_SeedsDefaultDunningPolicy(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dunSvc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)

	t.Run("one enabled default per mode, and a failed payment starts a run on it", func(t *testing.T) {
		res, err := tenant.RunBootstrap(ctx, db, bootstrapDeps(db, dunSvc.SeedDefaultPoliciesTx), tenant.BootstrapOpts{
			TenantName: "Seeded Co", OwnerEmail: "owner@seeded.test",
		})
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		assertSeeded(t, dunSvc, res.Tenant.ID)

		// A payment fails on the fresh account (test mode, where a new
		// operator tries things first): a run starts on the seed, first
		// retry three days out.
		testCtx := postgres.WithLivemode(ctx, false)
		cust, err := customer.NewPostgresStore(db).Create(testCtx, res.Tenant.ID, domain.Customer{ExternalID: "cus_seed", DisplayName: "Seed"})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		now := time.Now().UTC()
		inv, err := invoice.NewPostgresStore(db).Create(testCtx, res.Tenant.ID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: "INV-SEED-1", Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentFailed, Currency: "USD", SubtotalCents: 5000,
			TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
			BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		run, err := dunSvc.StartDunning(testCtx, res.Tenant.ID, inv.ID, cust.ID, time.Time{}, domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("a failed payment on a fresh account must start dunning: %v", err)
		}
		seed := modePolicies(t, dunSvc, res.Tenant.ID, false)[0]
		if run.PolicyID != seed.ID {
			t.Fatalf("run policy = %s, want the test-mode seed %s", run.PolicyID, seed.ID)
		}
		if run.NextActionAt == nil {
			t.Fatal("run has no first retry")
		}
		if d := run.NextActionAt.Sub(run.CreatedAt); d != 72*time.Hour {
			t.Fatalf("first retry %v after the failure, want 72h", d)
		}
	})

	t.Run("a failed seed creates no account at all", func(t *testing.T) {
		failing := func(context.Context, *sql.Tx, string, func(domain.DunningPolicy, bool) error) error {
			return errors.New("injected seed failure")
		}
		if _, err := tenant.RunBootstrap(ctx, db, bootstrapDeps(db, failing), tenant.BootstrapOpts{
			TenantName: "Never Seeded Co", OwnerEmail: "owner@never-seeded.test",
		}); err == nil {
			t.Fatal("bootstrap must fail when the seed fails")
		}
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin bypass: %v", err)
		}
		defer postgres.Rollback(tx)
		var tenants, users int
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM tenants WHERE name = 'Never Seeded Co'),
			(SELECT count(*) FROM users WHERE email = 'owner@never-seeded.test')`).Scan(&tenants, &users); err != nil {
			t.Fatalf("count: %v", err)
		}
		if tenants != 0 || users != 0 {
			t.Fatalf("tenants=%d users=%d after a failed seed, want 0/0", tenants, users)
		}
	})

	t.Run("an unwired seed is refused before any write", func(t *testing.T) {
		if _, err := tenant.RunBootstrap(ctx, db, bootstrapDeps(db, nil), tenant.BootstrapOpts{
			TenantName: "Unwired Co", OwnerEmail: "owner@unwired.test",
		}); err == nil {
			t.Fatal("bootstrap must refuse to run without the dunning seed")
		}
	})
}

// TestTenantCreate_SeedsDefaultDunningPolicy: POST /v1/tenants is the second
// way an account is created, and it gets the same policies, with an audit
// row per mode. Mutation check: drop the seed from Service.Create.
func TestTenantCreate_SeedsDefaultDunningPolicy(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dunSvc := dunning.NewService(dunning.NewPostgresStore(db), nil, nil)
	logger := audit.NewLogger(db)

	svc := tenant.NewService(tenant.NewPostgresStore(db))
	svc.SetAuditLogger(logger)
	svc.SetDunningSeeder(dunSvc.SeedDefaultPoliciesTx)

	created, err := svc.Create(ctx, tenant.CreateInput{Name: "Platform Created Co"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	assertSeeded(t, dunSvc, created.ID)

	for _, live := range []bool{false, true} {
		rows, _, err := logger.Query(postgres.WithLivemode(ctx, live), created.ID, audit.QueryFilter{ResourceType: "dunning_policy"})
		if err != nil {
			t.Fatalf("query audit (livemode=%v): %v", live, err)
		}
		if len(rows) != 1 || rows[0].Metadata["action"] != "tenant_provisioned" {
			t.Errorf("livemode=%v: dunning_policy audit rows %+v, want one tenant_provisioned row", live, rows)
		}
	}
}
