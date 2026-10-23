package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// ccyPaymentSetups reports a card on file for every customer.
type ccyPaymentSetups struct{}

func (ccyPaymentSetups) ResolveForCharge(context.Context, string, string) (string, string, error) {
	return "cus_stripe_ccy", "pm_ccy", nil
}

// ccyCharger records the invoice each charge was sent for.
type ccyCharger struct{ got []domain.Invoice }

func (c *ccyCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	c.got = append(c.got, inv)
	return inv, nil
}

// R2 (ADR-129): a customer billed in USD earns USD credit, then moves to EUR
// (billing profile EUR, a EUR subscription). The EUR invoice is collected
// through the auto-charge path (processAutoCharge): no USD credit is applied,
// the card is charged the full EUR amount, and the USD balance stays.
func TestCollect_CreditInAnotherCurrencyIsNotApplied(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Collect Credit Currency")

	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	customerStore := customer.NewPostgresStore(db)

	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cust, err := customerStore.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_ccy_move", DisplayName: "Moved to EUR"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if _, err := creditSvc.Grant(ctx, tenantID, credit.GrantInput{
		CustomerID: cust.ID, Currency: "USD", AmountCents: 5000, Description: "USD-era goodwill", At: periodStart,
	}); err != nil {
		t.Fatalf("grant USD credit: %v", err)
	}
	if _, err := customerStore.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID: cust.ID, Currency: "EUR", ProfileStatus: domain.BillingProfileIncomplete,
	}); err != nil {
		t.Fatalf("billing profile: %v", err)
	}
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-eur", Name: "Pro EUR", Currency: "EUR", BillingInterval: domain.BillingMonthly,
		Status: domain.PlanActive, BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-ccy-eur", DisplayName: "EUR", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	sub.CurrentBillingPeriodStart, sub.CurrentBillingPeriodEnd = &periodStart, &periodEnd

	charger := &ccyCharger{}
	e := billing.NewEngine(
		&subStoreAdapter{subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invoiceStore},
		creditSvc, tenant.NewSettingsStore(db), ccyPaymentSetups{}, charger,
		clock.NewFake(periodStart.Add(time.Hour)),
	)
	e.SetIntervalReader(subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetDunningResolver(&testDunningResolver{})

	inv, err := e.BillOnCreate(ctx, sub)
	if err != nil {
		t.Fatalf("BillOnCreate: %v", err)
	}
	got, err := invoiceStore.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("re-read invoice: %v", err)
	}
	if got.Currency != "EUR" {
		t.Fatalf("invoice currency %q, want EUR", got.Currency)
	}
	if got.CreditsAppliedCents != 0 || got.AmountDueCents != 4900 {
		t.Errorf("credits_applied %d, amount_due %d; want 0 and 4900 (USD credit never pays EUR)", got.CreditsAppliedCents, got.AmountDueCents)
	}
	if len(charger.got) != 1 || charger.got[0].AmountDueCents != 4900 {
		var amounts []int64
		for _, c := range charger.got {
			amounts = append(amounts, c.AmountDueCents)
		}
		t.Errorf("charges sent for %v, want one for the full 4900", amounts)
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var usd int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount_cents), 0) FROM customer_credit_ledger WHERE customer_id = $1 AND currency = 'USD'`, cust.ID).Scan(&usd); err != nil {
		t.Fatalf("read USD balance: %v", err)
	}
	if usd != 5000 {
		t.Errorf("USD balance %d, want 5000", usd)
	}
}
