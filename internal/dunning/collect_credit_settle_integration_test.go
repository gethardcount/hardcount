package dunning_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// failIfChargedCharger fails the test if the collect handler reaches the card:
// a credit-covered invoice must settle without one.
type failIfChargedCharger struct{ t *testing.T }

func (c failIfChargedCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	c.t.Errorf("ChargeInvoice called on a credit-covered invoice (due %d)", inv.AmountDueCents)
	return inv, nil
}

// noPaymentSetup is a customer with no card on file.
type noPaymentSetup struct{}

func (noPaymentSetup) GetPaymentSetup(context.Context, string, string) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{}, errs.ErrNotFound
}

// TestCollect_CreditCoveredInvoice_ResolvesActiveDunningRun reproduces the
// screenshot-pass finding: an invoice with no card sits in no-payment-method
// dunning, the customer is granted enough credit to cover it, and the operator
// clicks "Charge now". POST /v1/invoices/{id}/collect drains the credit and
// marks the invoice paid without a card — and before the fix left the run
// 'active' at $0. The dunning tick does not catch it either: a no-card run's
// next retry is a grace period away, and the tick only touches due runs, so the
// run kept counting in "active dunning runs" for days. The card branch of the
// same handler resolves the run; the credit branch must too, with the same
// resolution.
func TestCollect_CreditCoveredInvoice_ResolvesActiveDunningRun(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Collect Credit Settle")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_collect_credit", DisplayName: "No Card",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	dunStore := dunning.NewPostgresStore(db)
	dunSvc := dunning.NewService(dunStore, nil, clock.Real())
	policy, err := dunSvc.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetrySchedule: []string{"72h", "72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	if err := dunSvc.SetDefaultPolicy(ctx, tenantID, policy.ID); err != nil {
		t.Fatalf("set default policy: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	invSvc := invoice.NewService(invStore, clock.Real(), tenant.NewSettingsStore(db))
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	invSvc.SetCreditApplier(creditSvc)

	draft, err := invSvc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := invSvc.AddLineItem(ctx, tenantID, draft.ID, invoice.AddLineItemInput{
		Description: "Onboarding", Quantity: 1, UnitAmountCents: 5000,
	}); err != nil {
		t.Fatalf("add line: %v", err)
	}
	if _, err := invSvc.Finalize(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// The no-card enrollment the auto-charge sweep performs.
	run, err := dunSvc.StartDunning(ctx, tenantID, draft.ID, cust.ID, time.Time{}, domain.DunningCauseNoPaymentMethod)
	if err != nil {
		t.Fatalf("start dunning: %v", err)
	}
	if run.State != domain.DunningActive {
		t.Fatalf("seed: run state %q, want active", run.State)
	}

	// Credit arrives that covers the whole invoice.
	if _, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, AmountCents: 5000, Description: "goodwill", At: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	h := invoice.NewHandler(invSvc, nil, nil, invoice.HandlerDeps{
		Charger:       failIfChargedCharger{t: t},
		PaymentSetups: noPaymentSetup{},
		Dunning:       dunSvc,
	})
	router := chi.NewRouter()
	router.Mount("/v1/invoices", h.Routes())
	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+draft.ID+"/collect", nil)
	reqCtx := context.WithValue(ctx, auth.TestTenantIDKey(), tenantID)
	req = req.WithContext(reqCtx)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("collect: status %d, body %s", rr.Code, rr.Body.String())
	}

	inv, err := invStore.Get(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("re-read invoice: %v", err)
	}
	if inv.Status != domain.InvoicePaid || inv.AmountDueCents != 0 || inv.CreditsAppliedCents != 5000 {
		t.Fatalf("invoice = %s due=%d credits=%d, want paid / 0 / 5000 (settled from credit)",
			inv.Status, inv.AmountDueCents, inv.CreditsAppliedCents)
	}

	// A dunning tick runs, as it would every few minutes in production.
	tickCtx := leadertest.Token(t, testutil.AdminPool(t), ctx, leader.RoleDunning)
	if _, errsTick := dunSvc.ProcessDueRuns(tickCtx, tenantID, 50); len(errsTick) > 0 {
		t.Fatalf("dunning tick: %v", errsTick)
	}

	got, err := dunStore.GetRun(ctx, tenantID, run.ID)
	if err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if got.State != domain.DunningResolved {
		t.Fatalf("run state = %q after the invoice was settled from credit, want resolved (it keeps counting as an active dunning run)", got.State)
	}
	if got.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("run resolution = %q, want %q (same as the card branch)", got.Resolution, domain.ResolutionPaymentRecovered)
	}
	if got.ResolvedAt == nil {
		t.Error("resolved_at must be stamped")
	}
}
