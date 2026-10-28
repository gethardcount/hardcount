package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestRunListReads_CarryThePolicyLimit: the list reads of a run carry its
// bound policy's retry count (the length of its retry_days), read from the policy row as it is now
// (an edit reaches in-flight runs, as it does the dunning tick). The dashboard
// and the invoice notices use it to tell a run with a retry still to come
// from one whose next step is only its final action (RetryScheduled): the
// final action waiting for the invoice grace leaves the run active with every
// attempt used (ADR-120 amendment 2026-10-22).
//
// Mutation check: drop the policy join from ListRunsByInvoices or ListRuns →
// the run's max_retry_attempts reads 0 and the run with attempts left no longer reads
// as retrying.
func TestRunListReads_CarryThePolicyLimit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Run policy limit")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_lim", DisplayName: "Limit"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetryDays: []int{3, 3, 4},
		FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionNone,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	now := time.Now().UTC()
	newRun := func(number string, attempts int) domain.InvoiceDunningRun {
		t.Helper()
		inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: number, Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentFailed, Currency: "USD", SubtotalCents: 5000,
			TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
			BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		next := now.Add(time.Hour).Truncate(time.Microsecond)
		run, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: domain.DunningActive, Reason: "payment_failed", AttemptCount: attempts, NextActionAt: &next,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return run
	}
	retrying := newRun("INV-LIM-1", 1)
	finalStep := newRun("INV-LIM-2", 3)

	check := func(label string, runs []domain.InvoiceDunningRun, wantMax int) {
		t.Helper()
		if len(runs) != 2 {
			t.Fatalf("%s: %d runs, want 2", label, len(runs))
		}
		for _, r := range runs {
			if r.MaxRetryAttempts != wantMax {
				t.Fatalf("%s: run %s max_retry_attempts = %d, want %d", label, r.ID, r.MaxRetryAttempts, wantMax)
			}
			wantRetry := r.ID == retrying.ID && r.AttemptCount < wantMax
			if r.RetryScheduled() != wantRetry {
				t.Fatalf("%s: run %s (attempts %d of %d) RetryScheduled = %v, want %v",
					label, r.ID, r.AttemptCount, wantMax, r.RetryScheduled(), wantRetry)
			}
		}
	}
	read := func(wantMax int) {
		t.Helper()
		byInv, err := store.ListRunsByInvoices(ctx, tenantID, []string{retrying.InvoiceID, finalStep.InvoiceID})
		if err != nil {
			t.Fatalf("ListRunsByInvoices: %v", err)
		}
		check("ListRunsByInvoices", byInv, wantMax)
		listed, _, err := store.ListRuns(ctx, dunning.RunListFilter{TenantID: tenantID})
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		check("ListRuns", listed, wantMax)
	}
	read(3)

	// An edit lowering the limit reaches the in-flight runs: the run with
	// one attempt used now has none left either.
	policy.RetryDays = policy.RetryDays[:1]
	if _, err := store.UpsertPolicy(ctx, tenantID, policy); err != nil {
		t.Fatalf("lower the limit: %v", err)
	}
	read(1)
}
