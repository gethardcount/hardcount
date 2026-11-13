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
// The lines are stored at their net. The fake serves no tax transactions,
// so the close cannot record the calculation in Stripe's tax reports, as
// when Stripe fails at that moment. (A draft whose calculation is recorded
// finalizes without a refresh.)
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

// The same draft as v0.9.2 left it after two refreshes, the second at 25%
// (docs/ops/tax-calculation.md, "Invoices an earlier version under-billed").
// Each refresh taxed the stored line amounts as if they were the prices, and
// that version wrote no line amount, so the lines charge 100.00 and 50.00,
// not 120.00 and 60.00. The second refresh also stored a discount of 25.00,
// about the old tax, so the draft charges 125.00.
//
// A refresh now drops the discount. The draft charges the sum of its line
// totals, 150.00: its total rises by the discount, and the lowered lines stay.
//
// Mutation: carry the stored discount into the write
// (update.DiscountCents = max(taxApp.DiscountCents, inv.DiscountCents)) and
// the refresh keeps the discount of 2500.
func TestRetryTax_TaxInclusiveCycleDraft_RefreshDropsAStoredDiscount(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newInclusiveRefreshRig(t, db, "Inclusive Tax Stored Discount")
	draft := r.closeCycle(t)
	r.storeAsV092(t, draft.ID, 12000, 2500, 3000, map[string]lineWant{
		"base":     {10000, 2000, 10000},
		"calls":    {5000, 1000, 5000},
		"included": {0, 0, 0},
	})

	r.fake.SetRate(2500)
	r.refresh(t, draft.ID)
	r.assertDraft(t, draft.ID, 12000, 3000, map[string]lineWant{
		"base":     {8000, 2000, 10000},
		"calls":    {4000, 1000, 5000},
		"included": {0, 0, 0},
	})
}

// storeAsV092 writes a draft's tax as v0.9.2 stored it: each line's amount,
// tax and total by line key, and the header's subtotal, discount and tax,
// with the total and amount due that follow. Raw SQL, because this version
// never stores these amounts.
func (r *inclusiveRefreshRig) storeAsV092(t *testing.T, invoiceID string, subtotal, discount, taxCents int64, lines map[string]lineWant) {
	t.Helper()
	stored, err := r.invoices.ListLineItems(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	for _, li := range stored {
		key := "base"
		if li.LineType == domain.LineTypeUsage {
			key = r.meters[li.MeterID]
		}
		want, ok := lines[key]
		if !ok {
			t.Fatalf("no amounts given for the %s line", key)
		}
		if _, err := tx.ExecContext(r.ctx, `UPDATE invoice_line_items SET amount_cents = $2, tax_amount_cents = $3, total_amount_cents = $4 WHERE id = $1`,
			li.ID, want.amount, want.tax, want.total); err != nil {
			t.Fatalf("store the %s line: %v", key, err)
		}
	}
	total := subtotal - discount + taxCents
	if _, err := tx.ExecContext(r.ctx, `UPDATE invoices SET subtotal_cents = $2, discount_cents = $3, tax_amount_cents = $4,
		total_amount_cents = $5, amount_due_cents = $5 WHERE id = $1`, invoiceID, subtotal, discount, taxCents, total); err != nil {
		t.Fatalf("store the header: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
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
	if _, err := settings.Update(ctx, tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxProvider = "stripe_tax"
		ts.TaxInclusive = true
		ts.TaxName = "VAT"
		return nil
	}); err != nil {
		t.Fatalf("update settings: %v", err)
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

// refresh ages the draft's calculation past its expiry and clicks
// Retry tax.
func (r *inclusiveRefreshRig) refresh(t *testing.T, invoiceID string) {
	t.Helper()
	expireTaxCalculations(t, r.ctx, r.db, r.tenantID)
	if _, err := r.svc.RetryTax(r.ctx, r.tenantID, invoiceID); err != nil {
		t.Fatalf("retry tax: %v", err)
	}
}

// assertDraft checks the stored draft charges the sum of wantLines' totals,
// with no discount: the header's subtotal and tax, and each line's amount,
// tax and total, by line key.
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
	var wantTotal int64
	for _, l := range wantLines {
		wantTotal += l.total
	}
	if inv.TotalAmountCents != wantTotal || inv.AmountDueCents != wantTotal {
		t.Errorf("total/due = %d/%d, want %d/%d (the line totals, tax included)", inv.TotalAmountCents, inv.AmountDueCents, wantTotal, wantTotal)
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
