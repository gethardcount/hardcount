package dunning_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestDeletePolicy_RunStartedDuringTheDelete: a payment-retry run that is
// being created on a policy while the policy is deleted ends one of two ways,
// never with a database error. Here the run's insert is open (not yet
// committed) when the delete starts. The delete waits for it, then counts it
// and is refused with its reason, so the API answers 409 (#1215).
//
// The run's insert holds a key share lock on the policy row until it commits.
// DeletePolicy locks the row FOR UPDATE before it counts runs, so it waits
// for that commit and then sees the run.
//
// Mutation check: drop FOR UPDATE from DeletePolicy's policy read → the count
// runs before the run commits and sees none, the DELETE then waits on the key
// share lock and fails its ON DELETE RESTRICT check once the run commits, and
// the error is a foreign-key violation (a 500), not the refusal.
func TestDeletePolicy_RunStartedDuringTheDelete(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Policy delete race")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_race", DisplayName: "Race"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := dunning.NewPostgresStore(db)
	newPolicy := func(name string) domain.DunningPolicy {
		t.Helper()
		p, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
			Name: name, Enabled: true, RetryDays: []int{3, 3},
			FinalSubscriptionAction: domain.SubActionPause, FinalInvoiceAction: domain.InvActionNone,
		})
		if err != nil {
			t.Fatalf("upsert policy %s: %v", name, err)
		}
		return p
	}
	newPolicy("Standard")
	used := newPolicy("Used")
	if used.IsDefault {
		t.Fatalf("the second policy is the default; the test needs one that can be deleted")
	}

	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-RACE-1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentFailed, Currency: "USD", SubtotalCents: 5000,
		TotalAmountCents: 5000, AmountDueCents: 5000, BillingPeriodStart: now.Add(-time.Hour),
		BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	// The run's insert, left open.
	runTx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin run tx: %v", err)
	}
	defer postgres.Rollback(runTx)
	next := now.Add(time.Hour).Truncate(time.Microsecond)
	if _, err := store.CreateRunTx(ctx, runTx, tenantID, domain.InvoiceDunningRun{
		InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: used.ID,
		State: domain.DunningActive, Reason: "payment_failed", NextActionAt: &next,
	}); err != nil {
		t.Fatalf("create run on the open tx: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- store.DeletePolicy(ctx, tenantID, used.ID) }()
	testutil.WaitForLockWaiter(t, ctx, admin, "dunning_policies")
	if err := runTx.Commit(); err != nil {
		t.Fatalf("commit run tx: %v", err)
	}

	var delErr error
	select {
	case delErr = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DeletePolicy did not return after the run committed")
	}
	if !errors.Is(delErr, errs.ErrInvalidState) {
		t.Fatalf("DeletePolicy with a run committed while it waited: %v, want the invalid-state refusal (409)", delErr)
	}
	if !strings.Contains(delErr.Error(), "1 invoice") {
		t.Errorf("refusal %q, want it to say the policy has run on 1 invoice", delErr.Error())
	}
	if _, err := store.GetPolicyByID(ctx, tenantID, used.ID); err != nil {
		t.Errorf("the refused policy is gone: %v", err)
	}
}
