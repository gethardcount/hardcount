package billing_test

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/billing"
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

// With tax-inclusive prices on the manual provider, a line's net is its price
// less its tax. The line taxes add up to the invoice's tax, so the invoice
// charges the sum of the prices, each line's total is its price, and there is
// no discount. In each case a net rounded apart from its tax misses by a
// cent; in the last, one line's whole price is its tax, so its net is 0.
var inclusiveLineCases = []struct {
	name   string
	rate   float64
	prices []int64 // each line's price, tax included
}{
	{"three lines of 10.00 at 20%", 20, []int64{1000, 1000, 1000}},
	{"two lines of 10.04 at 20%", 20, []int64{1004, 1004}},
	{"two lines of 0.01 at 50%", 50, []int64{1, 1}},
}

// A subscription's cycle invoice: a base fee and one usage line per further
// price, each at 1 cent a unit. Collection is paused, so the invoice stays a
// draft and its tax can be worked out again from the stored lines.
func TestCycleClose_TaxInclusiveManual_ChargesThePrices(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, tc := range inclusiveLineCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := postgres.WithLivemode(t.Context(), false)
			tenantID := testutil.CreateTestTenant(t, db, "Inclusive Cycle "+tc.name)
			settings := tenant.NewSettingsStore(db)
			setInclusiveManualTax(t, ctx, settings, tenantID, tc.rate)

			customers := customer.NewPostgresStore(db)
			cust, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_incl_cycle", DisplayName: "Inclusive Cycle"})
			if err != nil {
				t.Fatalf("create customer: %v", err)
			}

			pricingStore := pricing.NewPostgresStore(db)
			pricingSvc := pricing.NewService(pricingStore)
			var prices []pricing.PriceInput
			var meterIDs []string
			for i := range tc.prices[1:] {
				m, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
					EventName: fmt.Sprintf("calls_%d", i), Name: fmt.Sprintf("Calls %d", i), Unit: "calls", Aggregation: "sum",
				})
				if err != nil {
					t.Fatalf("create meter: %v", err)
				}
				meterIDs = append(meterIDs, m.ID)
				prices = append(prices, pricing.PriceInput{MeterID: m.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}})
			}
			plan, _, err := pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
				Code: "incl", Name: "Inclusive", Currency: "USD", BillingInterval: domain.BillingMonthly,
				BaseAmountCents: tc.prices[0], BaseBillTiming: domain.BillInArrears, Prices: prices,
			})
			if err != nil {
				t.Fatalf("create plan: %v", err)
			}

			subs := subscription.NewPostgresStore(db)
			periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
			periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			sub, err := subs.Create(ctx, tenantID, domain.Subscription{
				Code: "sub-incl", DisplayName: "Inclusive", CustomerID: cust.ID,
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
			for i, meterID := range meterIDs {
				if _, err := usage.NewService(usageStore).Ingest(ctx, tenantID, usage.IngestInput{
					CustomerID: cust.ID, MeterID: meterID, Quantity: decimal.NewFromInt(tc.prices[i+1]), Timestamp: &usedAt,
				}); err != nil {
					t.Fatalf("ingest usage: %v", err)
				}
			}

			invoices := invoice.NewPostgresStore(db)
			engine := billing.NewEngine(
				&subStoreAdapter{subs}, &usageStoreAdapter{usageStore},
				&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invoices},
				nil, settings, testPaymentSetupsNoPM{}, testChargerSentinel{},
				clock.NewFake(periodEnd.Add(time.Hour)), customers,
			)
			engine.SetIntervalReader(subs)
			engine.SetTaxProviderResolver(tax.NewResolver(nil))
			engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
			engine.SetDunningStarter(testDunningStarter{})
			engine.SetStripeReadiness(testStripeConnected{})
			engine.SetInvoiceEmailer(&testInvoiceEmailer{})
			engine.SetDunningResolver(&testDunningResolver{})

			if n, failures := engine.RunCycleForTenant(ctx, tenantID, 50); n != 1 || len(failures) != 0 {
				t.Fatalf("cycle close = %d invoice(s), failures %v; want 1, none", n, failures)
			}
			invs, _, err := invoices.List(ctx, invoice.ListFilter{TenantID: tenantID, SubscriptionID: sub.ID})
			if err != nil || len(invs) != 1 {
				t.Fatalf("invoices = %d, %v; want 1", len(invs), err)
			}
			built := assertChargesThePrices(t, ctx, invoices, tenantID, invs[0].ID, tc.prices, true)

			if _, err := engine.ComputeTaxForInvoice(ctx, tenantID, invs[0].ID); err != nil {
				t.Fatalf("compute tax again: %v", err)
			}
			if again := assertChargesThePrices(t, ctx, invoices, tenantID, invs[0].ID, tc.prices, true); !slices.Equal(again, built) {
				t.Errorf("after computing the tax again, lines (amount, tax, total) = %v, want %v as built", again, built)
			}
		})
	}
}

// An invoice an operator composed is taxed when it is finalized. A finalize
// that fails after the tax is saved leaves the draft taxed, and the next
// finalize works the tax out again from the stored lines: it must charge the
// same.
func TestManualInvoice_TaxInclusiveManual_FinalizeAgainChargesThePrices(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, tc := range inclusiveLineCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newComposedRig(t, db, "Inclusive Composed "+tc.name, tc.rate)
			inv := r.create(t, tc.prices)

			// The tax a first finalize saves before it fails.
			if _, err := r.engine.ComputeTaxForInvoice(r.ctx, r.tenantID, inv.ID); err != nil {
				t.Fatalf("compute tax: %v", err)
			}
			first := assertChargesThePrices(t, r.ctx, r.invoices, r.tenantID, inv.ID, tc.prices, false)

			r.finalize(t, inv.ID)
			if again := assertChargesThePrices(t, r.ctx, r.invoices, r.tenantID, inv.ID, tc.prices, false); !slices.Equal(again, first) {
				t.Errorf("after the finalize, lines (amount, tax, total) = %v, want %v as first taxed", again, first)
			}
		})
	}
}

// An earlier version taxed this composed draft at a finalize that then
// failed. It rounded each line's net apart from the line's tax, so a line
// total can be a cent off the price entered, and the header can carry a
// discount of that cent. No version's tax write changes an operator's line
// amount, so the amounts still hold the prices entered. The next finalize
// charges those prices, with no discount.
//
// Mutations: tax each line's stored total (storedTaxBasis reading
// TotalAmountCents for every invoice) and the first case charges 2009 with
// line totals 1005 and 1004, the second 2999. Carry the stored discount
// into the write (update.DiscountCents = max(taxApp.DiscountCents,
// inv.DiscountCents)) and the first case keeps its discount of 1.
func TestManualInvoice_TaxInclusiveManual_DraftAnEarlierVersionTaxedChargesThePrices(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, tc := range []struct {
		name   string
		prices []int64
		// What the earlier version stored: each line's (amount, tax, total),
		// in the order of prices, and the header.
		lines                          []lineWant
		subtotal, discount, tax, total int64
	}{
		{
			"two lines of 10.04 at 20%", []int64{1004, 1004},
			[]lineWant{{1004, 168, 1005}, {1004, 167, 1004}}, 1674, 1, 335, 2008,
		},
		{
			"three lines of 10.00 at 20%", []int64{1000, 1000, 1000},
			[]lineWant{{1000, 167, 1000}, {1000, 167, 1000}, {1000, 166, 999}}, 2499, 0, 500, 2999,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newComposedRig(t, db, "Inclusive Earlier "+tc.name, 20)
			inv := r.create(t, tc.prices)
			storeEarlierTax(t, r.ctx, db, r.tenantID, inv.ID, tc.lines, tc.subtotal, tc.discount, tc.tax, tc.total)

			r.finalize(t, inv.ID)
			assertChargesThePrices(t, r.ctx, r.invoices, r.tenantID, inv.ID, tc.prices, false)
		})
	}
}

// composedRig is one account on tax-inclusive manual tax with one customer,
// and the engine and invoice service a finalize runs on.
type composedRig struct {
	ctx      context.Context
	tenantID string
	custID   string
	invoices *invoice.PostgresStore
	engine   *billing.Engine
	svc      *invoice.Service
}

func newComposedRig(t *testing.T, db *postgres.DB, name string, rate float64) composedRig {
	t.Helper()
	ctx := postgres.WithLivemode(t.Context(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	settings := tenant.NewSettingsStore(db)
	setInclusiveManualTax(t, ctx, settings, tenantID, rate)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_incl_composed", DisplayName: "Inclusive Composed"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	invoices := invoice.NewPostgresStore(db)
	engine := billing.NewEngine(
		&subStoreAdapter{subscription.NewPostgresStore(db)}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(db)}, &invoiceStoreAdapter{invoices},
		nil, settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.Real(),
	)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningStarter(testDunningStarter{})
	engine.SetStripeReadiness(testStripeConnected{})
	engine.SetInvoiceEmailer(&testInvoiceEmailer{})
	svc := invoice.NewService(invoices, clock.Real(), settings)
	svc.SetTenantSettingsReader(settings) // as router.go wires it
	svc.SetTaxRetrier(engine)
	return composedRig{ctx: ctx, tenantID: tenantID, custID: cust.ID, invoices: invoices, engine: engine, svc: svc}
}

// create composes a draft with one line per price, described "Service 0",
// "Service 1" and so on.
func (r composedRig) create(t *testing.T, prices []int64) domain.Invoice {
	t.Helper()
	var lines []invoice.AddLineItemInput
	for i, p := range prices {
		lines = append(lines, invoice.AddLineItemInput{Description: fmt.Sprintf("Service %d", i), Quantity: 1, UnitAmountCents: p})
	}
	inv, err := r.svc.Create(r.ctx, r.tenantID, invoice.CreateInput{CustomerID: r.custID, Currency: "USD", LineItems: lines})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func (r composedRig) finalize(t *testing.T, invoiceID string) {
	t.Helper()
	finalized, err := r.svc.Finalize(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if finalized.Status != domain.InvoiceFinalized {
		t.Fatalf("status = %s, want finalized", finalized.Status)
	}
}

// storeEarlierTax writes the tax an earlier version saved on a composed
// draft: line i ("Service i") gets lines[i], and the header the given
// amounts. Raw SQL, because this version never stores these amounts.
func storeEarlierTax(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, invoiceID string, lines []lineWant, subtotal, discount, taxCents, total int64) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	for i, l := range lines {
		res, err := tx.ExecContext(ctx, `UPDATE invoice_line_items SET amount_cents = $3, tax_amount_cents = $4, total_amount_cents = $5
			WHERE invoice_id = $1 AND description = $2`, invoiceID, fmt.Sprintf("Service %d", i), l.amount, l.tax, l.total)
		if err != nil {
			t.Fatalf("store line %d: %v", i, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("store line %d: %d rows, %v; want 1", i, n, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET subtotal_cents = $2, discount_cents = $3, tax_amount_cents = $4,
		total_amount_cents = $5, amount_due_cents = $5, tax_provider = 'manual' WHERE id = $1`,
		invoiceID, subtotal, discount, taxCents, total); err != nil {
		t.Fatalf("store header: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func setInclusiveManualTax(t *testing.T, ctx context.Context, settings *tenant.SettingsStore, tenantID string, rate float64) {
	t.Helper()
	setManualTax(t, ctx, settings, tenantID, rate)
	if _, err := settings.Update(ctx, tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxInclusive = true
		return nil
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
}

// assertChargesThePrices checks that the stored invoice charges the prices:
// each line's total is one of them, the header's total is their sum with no
// discount, and its subtotal and tax are the lines' amounts and taxes. A line
// billing made stores its net (amount + tax = total); a line an operator
// entered keeps its price as its amount. It returns each line's (amount, tax,
// total), sorted.
func assertChargesThePrices(t *testing.T, ctx context.Context, invoices *invoice.PostgresStore, tenantID, invoiceID string, prices []int64, netAmounts bool) []lineWant {
	t.Helper()
	inv, err := invoices.Get(ctx, tenantID, invoiceID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	lines, err := invoices.ListLineItems(ctx, tenantID, invoiceID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	var sum, nets, taxes int64
	for _, p := range prices {
		sum += p
	}
	got := make([]lineWant, 0, len(lines))
	totals := make([]int64, 0, len(lines))
	for _, li := range lines {
		got = append(got, lineWant{li.AmountCents, li.TaxAmountCents, li.TotalAmountCents})
		totals = append(totals, li.TotalAmountCents)
		taxes += li.TaxAmountCents
		net := li.TotalAmountCents - li.TaxAmountCents
		nets += net
		switch {
		case netAmounts && li.AmountCents != net:
			t.Errorf("line %q: amount %d, want its net %d (total %d less tax %d)", li.Description, li.AmountCents, net, li.TotalAmountCents, li.TaxAmountCents)
		case !netAmounts && li.AmountCents != li.TotalAmountCents:
			t.Errorf("line %q: amount %d, want the price entered (total %d)", li.Description, li.AmountCents, li.TotalAmountCents)
		}
	}
	if !slices.Equal(sorted(totals), sorted(prices)) {
		t.Errorf("line totals = %v, want the prices %v", totals, prices)
	}
	if inv.TotalAmountCents != sum || inv.AmountDueCents != sum || inv.DiscountCents != 0 {
		t.Errorf("total/due/discount = %d/%d/%d, want %d/%d/0 (the prices, tax included)", inv.TotalAmountCents, inv.AmountDueCents, inv.DiscountCents, sum, sum)
	}
	if inv.SubtotalCents != nets || inv.TaxAmountCents != taxes {
		t.Errorf("subtotal/tax = %d/%d, want the lines' nets %d and taxes %d", inv.SubtotalCents, inv.TaxAmountCents, nets, taxes)
	}
	slices.SortFunc(got, func(a, b lineWant) int {
		return cmp.Or(cmp.Compare(a.total, b.total), cmp.Compare(a.tax, b.tax), cmp.Compare(a.amount, b.amount))
	})
	return got
}
