package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The wall-clock TAX sweeps (retry + commit) complete the invoice-sweep family
// gated in #417: they too must exclude SIMULATED invoices by the invoice's own
// durable is_simulated flag, so a customer-pinned one-off (NULL subscription_id)
// can't leak into a wall-clock Stripe-Tax retry/commit. Simulated-invoice tax is
// driven by the catchup counterpart (ListPendingTaxRetryForClock / inline
// CommitTax during Advance). seedSweepInvoice lives in the sibling
// postgres_sim_sweep_gate_integration_test.go.

func TestListPendingTaxRetry_ExcludesSimulatedOneOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)

	sim := seedSweepInvoice(t, ctx, db, "TaxRetry Sim One-Off", `
		UPDATE invoices SET subscription_id = NULL, status = 'draft',
		       tax_status = 'pending', tax_error_code = 'provider_outage',
		       tax_retry_count = 0, tax_next_retry_at = NULL,
		       is_simulated = true, updated_at = now()
		 WHERE id = $1`)
	real := seedSweepInvoice(t, ctx, db, "TaxRetry Real", `
		UPDATE invoices SET status = 'draft', tax_status = 'pending',
		       tax_error_code = 'provider_outage', tax_retry_count = 0,
		       tax_next_retry_at = NULL, is_simulated = false, updated_at = now()
		 WHERE id = $1`)

	pending, err := store.ListPendingTaxRetry(ctx, 200, []string{"provider_outage"}, 3, false)
	if err != nil {
		t.Fatalf("ListPendingTaxRetry: %v", err)
	}
	got := map[string]bool{}
	for _, inv := range pending {
		got[inv.ID] = true
	}
	if got[sim] {
		t.Error("a simulated customer-pinned one-off must be excluded from the wall-clock tax-retry sweep (ADR-029)")
	}
	if !got[real] {
		t.Error("a wall-clock invoice must remain eligible for tax retry")
	}
}

func TestListPendingTaxCommit_ExcludesSimulatedOneOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)

	sim := seedSweepInvoice(t, ctx, db, "TaxCommit Sim One-Off", `
		UPDATE invoices SET subscription_id = NULL, status = 'paid',
		       tax_provider = 'stripe_tax', tax_status = 'ok',
		       tax_calculation_id = 'calc_sim', tax_transaction_id = '',
		       is_simulated = true, updated_at = now()
		 WHERE id = $1`)
	real := seedSweepInvoice(t, ctx, db, "TaxCommit Real", `
		UPDATE invoices SET status = 'paid', tax_provider = 'stripe_tax',
		       tax_status = 'ok', tax_calculation_id = 'calc_real',
		       tax_transaction_id = '', is_simulated = false, updated_at = now()
		 WHERE id = $1`)

	pending, err := store.ListPendingTaxCommit(ctx, 200, false)
	if err != nil {
		t.Fatalf("ListPendingTaxCommit: %v", err)
	}
	got := map[string]bool{}
	for _, inv := range pending {
		got[inv.ID] = true
	}
	if got[sim] {
		t.Error("a simulated customer-pinned one-off must be excluded from the wall-clock tax-commit sweep (ADR-029)")
	}
	if !got[real] {
		t.Error("a wall-clock invoice must remain eligible for tax commit")
	}
}
