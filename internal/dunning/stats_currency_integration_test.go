package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestGetStats_AtRiskScopedToDefaultCurrency covers the low audit finding: the
// dunning at-risk stat summed amount_due_cents across mixed currencies into one
// integer. It's now scoped to the tenant's default currency so a EUR invoice
// can't corrupt a USD-denominated at-risk figure.
func TestGetStats_AtRiskScopedToDefaultCurrency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Dunning Stats Currency")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_dun", DisplayName: "Dun",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true,
		RetrySchedule: []string{"72h", "120h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	// USD invoice $100 due, EUR invoice $999 due — both with an active run.
	mkInvoice := func(num, currency string, due int64) string {
		now := time.Now().UTC()
		issued := now
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
			Currency: currency, SubtotalCents: due, TotalAmountCents: due, AmountDueCents: due,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
		})
		if err != nil {
			t.Fatalf("create %s invoice: %v", currency, err)
		}
		if _, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: domain.DunningActive, Reason: "payment_failed",
		}); err != nil {
			t.Fatalf("create run for %s: %v", currency, err)
		}
		return inv.ID
	}
	mkInvoice("INV-USD", "USD", 100_00)
	mkInvoice("INV-EUR", "EUR", 999_00)

	stats, err := store.GetStats(ctx, tenantID)
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats.Currency != "USD" {
		t.Errorf("currency: got %q, want USD", stats.Currency)
	}
	if stats.AtRiskCents != 100_00 {
		t.Errorf("at_risk_cents: got %d, want 10000 (USD only — EUR excluded, not summed in)", stats.AtRiskCents)
	}
	if stats.ActiveCount != 2 {
		t.Errorf("active_count: got %d, want 2 (count is currency-agnostic)", stats.ActiveCount)
	}
}

// TestGetStats_AtRiskByCurrency: after the workspace default moved from USD to
// CAD, the card read "At risk (CAD) CA$0.00" while USD runs were active.
// at_risk_by_currency names every currency with active or escalated runs,
// default first, and leaves out resolved runs; at_risk_cents keeps its
// default-only meaning for API callers.
func TestGetStats_AtRiskByCurrency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Dunning Stats By Currency")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_dun_bc", DisplayName: "Dun BC",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true,
		RetrySchedule: []string{"72h", "120h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	mk := func(num, currency string, due int64, state domain.DunningRunState) {
		t.Helper()
		now := time.Now().UTC()
		issued := now
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
			Currency: currency, SubtotalCents: due, TotalAmountCents: due, AmountDueCents: due,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
		})
		if err != nil {
			t.Fatalf("create %s invoice: %v", num, err)
		}
		if _, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: state, Reason: "payment_failed",
		}); err != nil {
			t.Fatalf("create run for %s: %v", num, err)
		}
	}
	mk("INV-USD-1", "USD", 120_00, domain.DunningActive)
	mk("INV-USD-2", "USD", 619_00, domain.DunningEscalated)
	mk("INV-EUR-1", "EUR", 5_00, domain.DunningActive)
	mk("INV-GBP-1", "GBP", 999_00, domain.DunningResolved) // resolved: not at risk

	// The default moves to CAD, a currency with no runs at all.
	{
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_settings (tenant_id, default_currency)
			VALUES ($1, 'CAD')
			ON CONFLICT (tenant_id) DO UPDATE SET default_currency = 'CAD'
		`, tenantID); err != nil {
			t.Fatalf("set default currency: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	stats, err := store.GetStats(ctx, tenantID)
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats.Currency != "CAD" || stats.AtRiskCents != 0 {
		t.Errorf("default-currency figure: got %s %d, want CAD 0", stats.Currency, stats.AtRiskCents)
	}
	want := []dunning.CurrencyAmount{
		{Currency: "USD", AmountCents: 739_00},
		{Currency: "EUR", AmountCents: 5_00},
	}
	if len(stats.AtRiskByCurrency) != len(want) {
		t.Fatalf("at_risk_by_currency: got %+v, want %+v", stats.AtRiskByCurrency, want)
	}
	for i := range want {
		if stats.AtRiskByCurrency[i] != want[i] {
			t.Errorf("at_risk_by_currency[%d]: got %+v, want %+v", i, stats.AtRiskByCurrency[i], want[i])
		}
	}

	// With EUR as the default, EUR comes first even though USD is larger.
	{
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tenant_settings SET default_currency = 'EUR'`); err != nil {
			t.Fatalf("set default currency: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	stats, err = store.GetStats(ctx, tenantID)
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if len(stats.AtRiskByCurrency) != 2 || stats.AtRiskByCurrency[0].Currency != "EUR" || stats.AtRiskCents != 5_00 {
		t.Errorf("default-first ordering: got %+v (at_risk_cents %d), want EUR first and at_risk_cents 500", stats.AtRiskByCurrency, stats.AtRiskCents)
	}
}
