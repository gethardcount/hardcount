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
			ctx := postgres.WithLivemode(t.Context(), false)
			tenantID := testutil.CreateTestTenant(t, db, "Inclusive Composed "+tc.name)
			settings := tenant.NewSettingsStore(db)
			setInclusiveManualTax(t, ctx, settings, tenantID, tc.rate)

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
			svc.SetTaxRetrier(engine)

			var lines []invoice.AddLineItemInput
			for i, p := range tc.prices {
				lines = append(lines, invoice.AddLineItemInput{Description: fmt.Sprintf("Service %d", i), Quantity: 1, UnitAmountCents: p})
			}
			inv, err := svc.Create(ctx, tenantID, invoice.CreateInput{CustomerID: cust.ID, Currency: "USD", LineItems: lines})
			if err != nil {
				t.Fatalf("create invoice: %v", err)
			}

			// The tax a first finalize saves before it fails.
			if _, err := engine.ComputeTaxForInvoice(ctx, tenantID, inv.ID); err != nil {
				t.Fatalf("compute tax: %v", err)
			}
			first := assertChargesThePrices(t, ctx, invoices, tenantID, inv.ID, tc.prices, false)

			finalized, err := svc.Finalize(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("finalize: %v", err)
			}
			if finalized.Status != domain.InvoiceFinalized {
				t.Fatalf("status = %s, want finalized", finalized.Status)
			}
			if again := assertChargesThePrices(t, ctx, invoices, tenantID, inv.ID, tc.prices, false); !slices.Equal(again, first) {
				t.Errorf("after the finalize, lines (amount, tax, total) = %v, want %v as first taxed", again, first)
			}
		})
	}
}

func setInclusiveManualTax(t *testing.T, ctx context.Context, settings *tenant.SettingsStore, tenantID string, rate float64) {
	t.Helper()
	setManualTax(t, ctx, settings, tenantID, rate)
	ts, err := settings.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.TaxInclusive = true
	if _, err := settings.Upsert(ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
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
