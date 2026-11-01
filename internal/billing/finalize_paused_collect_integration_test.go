package billing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// TestFinalize_PausedSubscriptionDraft_NotCharged drives the operator's
// POST /invoices/{id}/finalize on the draft a paused subscription's cycle
// left, with the real billing engine as the handler's collector. The finalize
// dialog tells the operator "Collection is paused on this subscription, so
// nothing is charged now". The collect that runs after the finalize claimed
// with no pause check, so it charged the card. Now the invoice is issued and
// stays queued, and it is charged once collection resumes.
//
// Mutation: drop the pause predicate from ClaimAutoCharge, and the finalize
// charges 5000 while collection is paused.
func TestFinalize_PausedSubscriptionDraft_NotCharged(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Finalize Paused Draft")

	invoiceStore := invoice.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	customers := customer.NewPostgresStore(db)
	subs := subscription.NewPostgresStore(db)
	svc := invoice.NewService(invoiceStore, clock.Real(), settings)
	svc.SetCreditApplier(creditSvc)
	svc.SetSubscriptionPauseReader(subs)

	charger := &amountCharger{}
	engine := billing.NewEngine(
		&subStoreAdapter{subs}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoiceStore},
		creditSvc, settings, testPaymentSetupsReady{}, charger, clock.Real(),
	)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningStarter(testDunningStarter{})
	engine.SetStripeReadiness(testStripeConnected{})
	engine.SetInvoiceEmailer(&testInvoiceEmailer{})
	engine.SetDunningResolver(&testDunningResolver{})

	h := invoice.NewHandler(svc, customers, settings)
	h.SetCollector(engine)
	routes := h.Routes()

	cust, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_paused_fin", DisplayName: "Paused Finalize"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "paused-fin", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Status: domain.PlanActive, BaseAmountCents: 5000, BaseBillTiming: domain.BillInArrears,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	ps := time.Now().UTC().AddDate(0, -1, 0)
	pe := ps.AddDate(0, 1, 0)
	sub, err := subs.Create(ctx, tenantID, domain.Subscription{
		Code: "paused-fin-sub", DisplayName: "Pro", CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &ps, Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if _, err := subs.SetPauseCollection(ctx, tenantID, sub.ID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
		t.Fatalf("pause collection: %v", err)
	}
	// The draft the cycle close leaves while collection is paused.
	draft, err := invoiceStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: "INV-PAUSED-FIN",
		Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		BillingReason: domain.BillingReasonSubscriptionCycle, Currency: "USD",
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: ps, BillingPeriodEnd: pe,
	}, []domain.InvoiceLineItem{{
		LineType: domain.LineTypeBaseFee, Description: "Pro - base fee (qty 1)", Quantity: 1,
		UnitAmountCents: 5000, AmountCents: 5000, TotalAmountCents: 5000, Currency: "USD",
	}})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/"+draft.ID+"/finalize", nil)
	reqCtx := context.WithValue(postgres.WithLivemode(req.Context(), false), auth.TestTenantIDKey(), tenantID)
	rr := httptest.NewRecorder()
	routes.ServeHTTP(rr, req.WithContext(reqCtx))
	if rr.Code != http.StatusOK {
		t.Fatalf("finalize status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out domain.Invoice
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(charger.got) != 0 {
		t.Fatalf("charges = %v while collection is paused, want none", charger.got)
	}
	stored, err := invoiceStore.Get(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status != domain.InvoiceFinalized || stored.PaymentStatus != domain.PaymentPending || !stored.AutoChargePending {
		t.Fatalf("stored = %s/%s queued=%v, want finalized/pending and queued for when collection resumes",
			stored.Status, stored.PaymentStatus, stored.AutoChargePending)
	}
	if out.Status != domain.InvoiceFinalized || out.PaymentStatus != domain.PaymentPending {
		t.Errorf("response = %s/%s, want finalized/pending", out.Status, out.PaymentStatus)
	}

	// Resumed: the queued invoice is charged, once.
	if _, err := subs.ClearPauseCollection(ctx, tenantID, sub.ID, nil); err != nil {
		t.Fatalf("resume collection: %v", err)
	}
	engine.CollectInvoice(ctx, tenantID, draft.ID, nil)
	if len(charger.got) != 1 || charger.got[0] != 5000 {
		t.Fatalf("charges after resume = %v, want one charge of 5000", charger.got)
	}
}
