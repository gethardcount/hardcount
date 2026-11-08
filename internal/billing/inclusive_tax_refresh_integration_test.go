package billing_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// A tax-inclusive Stripe Tax account bills a $120.00 plan to a subscription
// whose collection is paused, so the cycle close leaves the invoice a draft:
// $100.00 net plus $20.00 VAT, the lines stored at their net. A day later
// the Stripe Tax calculation has expired, finalize refuses it, and the
// operator clicks Retry tax to refresh it. The refresh must tax the $120.00
// the customer is charged. It taxed the stored $100.00 net instead, carved
// the VAT out a second time, and the invoice total fell to $100.00.
//
// The second refresh follows a rate change to 25%: the line amount is
// rewritten to the new net, so amount, tax and total still add up.
//
// Mutation: tax the stored lines again (storedTaxBasis returning the lines
// as stored) and the first refresh asks Stripe to tax 10000 and the total
// falls to 10000. Drop the amount_cents write from UpdateTaxAtomic and the
// second refresh leaves the line at 10000 + 2400 tax ≠ 12000.
func TestRetryTax_TaxInclusiveCycleDraft_RefreshKeepsTheTotal(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(t.Context(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Inclusive Tax Refresh")

	settings := tenant.NewSettingsStore(db)
	ts, err := settings.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.TaxProvider = "stripe_tax"
	ts.TaxInclusive = true
	ts.TaxName = "VAT"
	if _, err := settings.Upsert(ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}

	customers := customer.NewPostgresStore(db)
	cust, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_incl_refresh", DisplayName: "Inclusive Refresh"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	if _, err := customers.UpsertBillingProfile(ctx, tenantID, domain.CustomerBillingProfile{
		CustomerID: cust.ID, Country: "GB", PostalCode: "SW1A 1AA", ProfileStatus: domain.BillingProfileIncomplete,
	}); err != nil {
		t.Fatalf("billing profile: %v", err)
	}

	pricingStore := pricing.NewPostgresStore(db)
	plan, err := pricingStore.CreatePlan(ctx, tenantID, domain.Plan{
		Code: "pro-incl", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Status: domain.PlanActive, BaseAmountCents: 12000, BaseBillTiming: domain.BillInArrears,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	subs := subscription.NewPostgresStore(db)
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	sub, err := subs.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-incl-refresh", DisplayName: "Pro", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: bivPE(periodStart),
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	if _, err := subs.SetPauseCollection(ctx, tenantID, sub.ID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
		t.Fatalf("pause collection: %v", err)
	}

	fake := billing.NewStripeTaxFake(t, 2000) // 20% VAT
	invoiceStore := invoice.NewPostgresStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	engine := billing.NewEngine(
		&subStoreAdapter{subs}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invoiceStore},
		creditSvc, settings, testPaymentSetupsNoPM{}, testChargerSentinel{},
		clock.NewFake(periodEnd.Add(time.Hour)), customers,
	)
	engine.SetIntervalReader(subs)
	engine.SetTaxProviderResolver(tax.NewResolver(fake))
	engine.SetTaxCalculationStore(tax.NewPostgresStore(db))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningStarter(testDunningStarter{})
	engine.SetStripeReadiness(testStripeConnected{})
	engine.SetInvoiceEmailer(&testInvoiceEmailer{})
	engine.SetDunningResolver(&testDunningResolver{})

	// The operator's Retry tax, as the HTTP handler calls it.
	svc := invoice.NewService(invoiceStore, clock.Real(), settings)
	svc.SetTaxRetrier(engine)
	svc.SetSubscriptionPauseReader(subs)

	// 1. The cycle close: a draft, taxed, its line stored at the net.
	if n, failures := engine.RunCycleForTenant(ctx, tenantID, 50); n != 1 || len(failures) != 0 {
		t.Fatalf("cycle close = %d invoice(s), failures %v; want 1, none", n, failures)
	}
	invs, _, err := invoiceStore.List(ctx, invoice.ListFilter{TenantID: tenantID, SubscriptionID: sub.ID})
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices = %d, %v; want 1", len(invs), err)
	}
	draft := invs[0]
	if draft.Status != domain.InvoiceDraft || draft.TaxStatus != domain.InvoiceTaxOK || draft.TaxCalculationID == "" {
		t.Fatalf("after the close: status/tax_status/calc = %s/%s/%q, want draft/ok/a calculation", draft.Status, draft.TaxStatus, draft.TaxCalculationID)
	}
	assertInclusiveDraft(t, ctx, invoiceStore, tenantID, draft.ID, 10000, 2000, 10000)

	// 2. A day later: the calculation has expired and Retry tax refreshes it.
	expireTaxCalculations(t, ctx, db, tenantID)
	if _, err := svc.RetryTax(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("retry tax: %v", err)
	}
	if got := fake.Requests(); len(got) != 2 || !slices.Equal(got[1].Amounts, []int64{12000}) || !got[1].Inclusive {
		t.Fatalf("calculation requests = %+v, want the refresh to tax the 12000 the customer is charged, inclusive", got)
	}
	refreshed := assertInclusiveDraft(t, ctx, invoiceStore, tenantID, draft.ID, 10000, 2000, 10000)
	if refreshed.TaxCalculationID == draft.TaxCalculationID {
		t.Errorf("tax_calculation_id = %q after the refresh, want the new calculation", refreshed.TaxCalculationID)
	}

	// 3. The rate rises to 25% and the new calculation expires too.
	fake.SetRate(2500)
	expireTaxCalculations(t, ctx, db, tenantID)
	if _, err := svc.RetryTax(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("second retry tax: %v", err)
	}
	if got := fake.Requests(); len(got) != 3 || !slices.Equal(got[2].Amounts, []int64{12000}) {
		t.Fatalf("calculation requests = %+v, want the second refresh to tax 12000 too", got)
	}
	assertInclusiveDraft(t, ctx, invoiceStore, tenantID, draft.ID, 9600, 2400, 9600)
}

// assertInclusiveDraft checks the stored draft charges 12000 in all: the
// header's subtotal and tax, and its one line's amount, tax and total.
func assertInclusiveDraft(t *testing.T, ctx context.Context, store *invoice.PostgresStore, tenantID, invoiceID string, wantSubtotal, wantTax, wantLineAmount int64) domain.Invoice {
	t.Helper()
	inv, err := store.Get(ctx, tenantID, invoiceID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if inv.Status != domain.InvoiceDraft || inv.TaxStatus != domain.InvoiceTaxOK {
		t.Errorf("status/tax_status = %s/%s, want draft/ok (collection is paused)", inv.Status, inv.TaxStatus)
	}
	if inv.SubtotalCents != wantSubtotal || inv.DiscountCents != 0 || inv.TaxAmountCents != wantTax {
		t.Errorf("subtotal/discount/tax = %d/%d/%d, want %d/0/%d", inv.SubtotalCents, inv.DiscountCents, inv.TaxAmountCents, wantSubtotal, wantTax)
	}
	if inv.TotalAmountCents != 12000 || inv.AmountDueCents != 12000 {
		t.Errorf("total/due = %d/%d, want 12000/12000 (the price, tax included)", inv.TotalAmountCents, inv.AmountDueCents)
	}
	lines, err := store.ListLineItems(ctx, tenantID, invoiceID)
	if err != nil || len(lines) != 1 {
		t.Fatalf("lines = %+v, %v; want one", lines, err)
	}
	if li := lines[0]; li.AmountCents != wantLineAmount || li.TaxAmountCents != wantTax || li.TotalAmountCents != 12000 {
		t.Errorf("line amount/tax/total = %d/%d/%d, want %d/%d/12000", li.AmountCents, li.TaxAmountCents, li.TotalAmountCents, wantLineAmount, wantTax)
	}
	return inv
}

// expireTaxCalculations ages the tenant's calculation rows past Stripe's 24h
// limit. Raw SQL because no production path makes a day-old row in a test.
func expireTaxCalculations(t *testing.T, ctx context.Context, db *postgres.DB, tenantID string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE tax_calculations SET created_at = created_at - interval '25 hours' WHERE tenant_id = $1`, tenantID); err != nil {
		t.Fatalf("age tax calculations: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
