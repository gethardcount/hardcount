package billing_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

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

// A tax-inclusive Stripe Tax account bills a subscription whose collection
// is paused, so the cycle close leaves the June invoice a draft. Its plan
// has a $120.00 base fee billed in advance, API calls at 1 cent each, and
// included calls at no charge. June had 6000 calls and 50 included calls, so
// the draft charges $180.00 in three lines: the July base fee ($100.00 net
// + $20.00 VAT), the calls ($50.00 + $10.00) and the included calls ($0).
// The lines are stored at their net.
//
// A day later the Stripe Tax calculation has expired, finalize refuses it,
// and the operator clicks Retry tax to refresh it. The refresh must tax the
// $120.00 and $60.00 the customer is charged. It taxed the stored nets
// instead, carved the VAT out a second time, and the total fell to $150.00.
//
// The second refresh follows a rate change to 25%: each line amount is
// rewritten to its new net, so amount, tax and total still add up.
//
// Mutation: tax the stored lines again (storedTaxBasis returning the lines
// as stored) and the first refresh asks Stripe to tax 10000 and 5000, and
// the total falls to 15000. Drop the amount_cents write from UpdateTaxAtomic
// and the second refresh leaves the base fee at 10000 + 2400 tax ≠ 12000.
func TestRetryTax_TaxInclusiveCycleDraft_RefreshKeepsTheTotal(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newInclusiveRefreshRig(t, db, "Inclusive Tax Refresh")

	// 1. The cycle close: a draft, taxed, its lines stored at the net.
	draft := r.closeCycle(t)
	if draft.Status != domain.InvoiceDraft || draft.TaxStatus != domain.InvoiceTaxOK || draft.TaxCalculationID == "" {
		t.Fatalf("after the close: status/tax_status/calc = %s/%s/%q, want draft/ok/a calculation", draft.Status, draft.TaxStatus, draft.TaxCalculationID)
	}
	at20 := map[string]lineWant{
		"base":     {10000, 2000, 12000},
		"calls":    {5000, 1000, 6000},
		"included": {0, 0, 0},
	}
	r.assertDraft(t, draft.ID, 15000, 3000, at20)

	// 2. A day later: the calculation has expired and Retry tax refreshes it.
	r.refresh(t, draft.ID)
	if got := r.fake.Requests(); len(got) != 2 || !slices.Equal(sorted(got[1].Amounts), []int64{0, 6000, 12000}) || !got[1].Inclusive {
		t.Fatalf("calculation requests = %+v, want the refresh to tax the 12000, 6000 and 0 the customer is charged, inclusive", got)
	}
	refreshed := r.assertDraft(t, draft.ID, 15000, 3000, at20)
	if refreshed.TaxCalculationID == draft.TaxCalculationID {
		t.Errorf("tax_calculation_id = %q after the refresh, want the new calculation", refreshed.TaxCalculationID)
	}

	// 3. The rate rises to 25% and the new calculation expires too.
	r.fake.SetRate(2500)
	r.refresh(t, draft.ID)
	if got := r.fake.Requests(); len(got) != 3 || !slices.Equal(sorted(got[2].Amounts), []int64{0, 6000, 12000}) {
		t.Fatalf("calculation requests = %+v, want the second refresh to tax 12000, 6000 and 0 too", got)
	}
	r.assertDraft(t, draft.ID, 14400, 3600, map[string]lineWant{
		"base":     {9600, 2400, 12000},
		"calls":    {4800, 1200, 6000},
		"included": {0, 0, 0},
	})
}

// lineWant is one stored line: amount, tax and total.
type lineWant struct{ amount, tax, total int64 }

// inclusiveRefreshRig is one tax-inclusive Stripe Tax account with the
// paused subscription of TestRetryTax_TaxInclusiveCycleDraft_RefreshKeepsTheTotal,
// before its June close.
type inclusiveRefreshRig struct {
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	subID    string
	meters   map[string]string // meter id → line key ("calls", "included")
	engine   *billing.Engine
	svc      *invoice.Service
	invoices *invoice.PostgresStore
	fake     interface {
		SetRate(int64)
		Requests() []billing.StripeTaxFakeRequest
	}
}

func newInclusiveRefreshRig(t *testing.T, db *postgres.DB, name string) *inclusiveRefreshRig {
	t.Helper()
	ctx := postgres.WithLivemode(t.Context(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)

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
	pricingSvc := pricing.NewService(pricingStore)
	calls, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{EventName: "calls", Name: "API calls", Unit: "calls", Aggregation: "sum"})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	included, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{EventName: "included_calls", Name: "Included calls", Unit: "calls", Aggregation: "sum"})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	plan, _, err := pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "pro-incl", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 12000, BaseBillTiming: domain.BillInAdvance,
		Prices: []pricing.PriceInput{
			{MeterID: calls.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}},
			{MeterID: included.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.Zero}},
		},
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

	usageStore := usage.NewPostgresStore(db)
	usedAt := periodStart.AddDate(0, 0, 2)
	for meterID, qty := range map[string]int64{calls.ID: 6000, included.ID: 50} {
		if _, err := usage.NewService(usageStore).Ingest(ctx, tenantID, usage.IngestInput{
			CustomerID: cust.ID, MeterID: meterID, Quantity: decimal.NewFromInt(qty), Timestamp: &usedAt,
		}); err != nil {
			t.Fatalf("ingest usage: %v", err)
		}
	}

	fake := billing.NewStripeTaxFake(t, 2000) // 20% VAT
	invoiceStore := invoice.NewPostgresStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	engine := billing.NewEngine(
		&subStoreAdapter{subs}, &usageStoreAdapter{usageStore},
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

	return &inclusiveRefreshRig{
		ctx: ctx, db: db, tenantID: tenantID, subID: sub.ID,
		meters: map[string]string{calls.ID: "calls", included.ID: "included"},
		engine: engine, svc: svc, invoices: invoiceStore, fake: fake,
	}
}

// closeCycle runs the June close and returns its one invoice.
func (r *inclusiveRefreshRig) closeCycle(t *testing.T) domain.Invoice {
	t.Helper()
	if n, failures := r.engine.RunCycleForTenant(r.ctx, r.tenantID, 50); n != 1 || len(failures) != 0 {
		t.Fatalf("cycle close = %d invoice(s), failures %v; want 1, none", n, failures)
	}
	invs, _, err := r.invoices.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID, SubscriptionID: r.subID})
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices = %d, %v; want 1", len(invs), err)
	}
	return invs[0]
}

// refresh ages the draft's calculation past Stripe's 24 hours and clicks
// Retry tax.
func (r *inclusiveRefreshRig) refresh(t *testing.T, invoiceID string) {
	t.Helper()
	expireTaxCalculations(t, r.ctx, r.db, r.tenantID)
	if _, err := r.svc.RetryTax(r.ctx, r.tenantID, invoiceID); err != nil {
		t.Fatalf("retry tax: %v", err)
	}
}

// assertDraft checks the stored draft charges 18000 in all: the header's
// subtotal and tax, and each line's amount, tax and total, by line key.
func (r *inclusiveRefreshRig) assertDraft(t *testing.T, invoiceID string, wantSubtotal, wantTax int64, wantLines map[string]lineWant) domain.Invoice {
	t.Helper()
	inv, err := r.invoices.Get(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if inv.Status != domain.InvoiceDraft || inv.TaxStatus != domain.InvoiceTaxOK {
		t.Errorf("status/tax_status = %s/%s, want draft/ok (collection is paused)", inv.Status, inv.TaxStatus)
	}
	if inv.SubtotalCents != wantSubtotal || inv.DiscountCents != 0 || inv.TaxAmountCents != wantTax {
		t.Errorf("subtotal/discount/tax = %d/%d/%d, want %d/0/%d", inv.SubtotalCents, inv.DiscountCents, inv.TaxAmountCents, wantSubtotal, wantTax)
	}
	if inv.TotalAmountCents != 18000 || inv.AmountDueCents != 18000 {
		t.Errorf("total/due = %d/%d, want 18000/18000 (the prices, tax included)", inv.TotalAmountCents, inv.AmountDueCents)
	}
	lines, err := r.invoices.ListLineItems(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	got := map[string]lineWant{}
	for _, li := range lines {
		key := "base"
		if li.LineType == domain.LineTypeUsage {
			key = r.meters[li.MeterID]
		}
		got[key] = lineWant{li.AmountCents, li.TaxAmountCents, li.TotalAmountCents}
	}
	if len(lines) != len(wantLines) {
		t.Errorf("lines = %d (%v), want %d", len(lines), got, len(wantLines))
	}
	for key, want := range wantLines {
		if g, ok := got[key]; !ok || g != want {
			t.Errorf("%s line amount/tax/total = %d/%d/%d (found %v), want %d/%d/%d", key, g.amount, g.tax, g.total, ok, want.amount, want.tax, want.total)
		}
	}
	return inv
}

func sorted(s []int64) []int64 {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
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
