package billing

import (
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// With tax-inclusive prices, the price a customer is charged already holds
// the tax, and tax is carved out of it. The engine's build paths store each
// line at its net: the carve rewrites the line amount to what is left after
// the tax. A recomputation of a draft's tax (Retry tax, the finalize-time
// compute) reads the stored invoice back, so it must tax the price the
// customer is charged, not that stored net. Taxed from the net, the tax was
// carved out a second time and the invoice total fell by the tax.
//
// After the recomputation, a line on a draft billing made stores the new
// net, as its build does. A line on a draft an operator made keeps the
// amount entered.
//
// The fake is Stripe Tax over HTTP (stripeTaxFake), so the real provider
// reads the inclusive response. A line priced 12000, tax-inclusive at 20%,
// is 10000 net and 2000 tax; at 25%, 9600 and 2400.

const linePrice = 12000

// zeroRatedCode is a product tax code a test rates with
// stripeTaxFake.SetCodeRate.
const zeroRatedCode = "txcd_zero_rated_test"

func newStripeTaxEngine(fake *stripeTaxFake, inclusive bool, calcCreatedAt time.Time) *Engine {
	return &Engine{
		settings:     &taxSettings{provider: "stripe_tax", name: "VAT", inclusive: inclusive},
		taxProviders: stubResolver(tax.NewStripeTaxProvider(fake)),
		taxCalcStore: &guardCalcStore{createdAt: calcCreatedAt},
		profiles: &taxProfiles{profiles: map[string]domain.CustomerBillingProfile{
			"cus_1": {CustomerID: "cus_1", Country: "GB", PostalCode: "SW1A 1AA"},
		}},
	}
}

func grossLine(id string, cents int64) domain.InvoiceLineItem {
	return domain.InvoiceLineItem{
		ID: id, InvoiceID: "inv_1", Description: "Pro - base fee", LineType: domain.LineTypeBaseFee,
		Quantity: 1, UnitAmountCents: cents, AmountCents: cents, TotalAmountCents: cents,
	}
}

// codedLine is a usage line priced cents, with product tax code code.
func codedLine(id string, cents int64, code string) domain.InvoiceLineItem {
	return domain.InvoiceLineItem{
		ID: id, InvoiceID: "inv_1", Description: "API calls", LineType: domain.LineTypeUsage,
		Quantity: 1, UnitAmountCents: cents, AmountCents: cents, TotalAmountCents: cents, TaxCode: code,
	}
}

// lineWant is one stored line: amount, tax and total.
type lineWant struct{ amount, tax, total int64 }

// draftBuild stores the draft a test then retries.
type draftBuild func(t *testing.T, e *Engine, fake *stripeTaxFake)

func sumAmounts(lines []domain.InvoiceLineItem) int64 {
	var sum int64
	for _, li := range lines {
		sum += li.AmountCents
	}
	return sum
}

// engineDraft stores a draft the way billing's build paths do: tax applied
// to the lines at their price, then the lines and the header saved as they
// come out. Collection paused is the common reason such a draft is still
// open a day later.
func engineDraft(reason domain.InvoiceBillingReason, lines ...domain.InvoiceLineItem) draftBuild {
	return func(t *testing.T, e *Engine, fake *stripeTaxFake) {
		t.Helper()
		ctx := postgres.WithLivemode(t.Context(), false)
		lines := slices.Clone(lines)
		app, err := e.ApplyTaxToLineItems(ctx, "t1", "cus_1", "USD", sumAmounts(lines), 0, lines)
		if err != nil {
			t.Fatalf("build-time tax: %v", err)
		}
		total := app.SubtotalCents - app.DiscountCents + app.TaxAmountCents
		e.invoices = &mockInvoices{
			invoices: []domain.Invoice{{
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Currency: "USD",
				Status: domain.InvoiceDraft, BillingReason: reason,
				SubtotalCents: app.SubtotalCents, DiscountCents: app.DiscountCents, TaxFacts: app.TaxFacts,
				TotalAmountCents: total, AmountDueCents: total,
			}},
			lineItems: lines,
		}
		if len(fake.Requests()) != 1 {
			t.Fatalf("build-time calculations = %d, want 1", len(fake.Requests()))
		}
	}
}

// pendingDraft is engineDraft with the provider down, so the draft waits for
// tax with its lines at their price. Billing's drafts get there at the
// build, an operator's at a finalize: the stored draft is the same.
func pendingDraft(reason domain.InvoiceBillingReason, lines ...domain.InvoiceLineItem) draftBuild {
	build := engineDraft(reason, lines...)
	return func(t *testing.T, e *Engine, fake *stripeTaxFake) {
		t.Helper()
		fake.FailNext()
		build(t, e, fake)
	}
}

// operatorDraft is a draft an operator composed, taxed once by a finalize
// that then did not complete. Its lines keep what was entered.
func operatorDraft(lines ...domain.InvoiceLineItem) draftBuild {
	return func(t *testing.T, e *Engine, fake *stripeTaxFake) {
		t.Helper()
		lines := slices.Clone(lines)
		subtotal := sumAmounts(lines)
		e.invoices = &mockInvoices{
			invoices: []domain.Invoice{{
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Currency: "USD",
				Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonManual,
				SubtotalCents: subtotal, TotalAmountCents: subtotal, AmountDueCents: subtotal,
				TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
			}},
			lineItems: lines,
		}
		if _, err := e.ComputeTaxForInvoice(postgres.WithLivemode(t.Context(), false), "t1", "inv_1"); err != nil {
			t.Fatalf("finalize-time tax: %v", err)
		}
		if len(fake.Requests()) != 1 {
			t.Fatalf("finalize-time calculations = %d, want 1", len(fake.Requests()))
		}
	}
}

// operatorDraftWithLaterLine is operatorDraft(first) plus a line the
// operator adds after that finalize. The header is then what
// AddLineItemAtomicAudited writes: the subtotal is the sum of the line
// amounts, and the total that sum plus the stored tax.
func operatorDraftWithLaterLine(first, later domain.InvoiceLineItem) draftBuild {
	build := operatorDraft(first)
	return func(t *testing.T, e *Engine, fake *stripeTaxFake) {
		t.Helper()
		build(t, e, fake)
		m := e.invoices.(*mockInvoices)
		m.lineItems = append(m.lineItems, later)
		inv := &m.invoices[0]
		inv.SubtotalCents = sumAmounts(m.lineItems)
		inv.TotalAmountCents = inv.SubtotalCents + inv.TaxAmountCents - inv.DiscountCents
		inv.AmountDueCents = inv.TotalAmountCents
	}
}

func rateTo(bp int64) func(*stripeTaxFake) {
	return func(f *stripeTaxFake) { f.SetRate(bp) }
}

func TestRetryTax_TaxInclusiveDraft_TaxesTheGross(t *testing.T) {
	expired := time.Now().UTC().Add(-30 * time.Hour)
	cycle := domain.BillingReasonSubscriptionCycle
	for _, tc := range []struct {
		name      string
		inclusive bool
		setup     func(*stripeTaxFake) // before the build
		build     draftBuild
		change    func(*stripeTaxFake) // between the build and the retry

		wantRequest  []int64 // the line amounts the retry asks Stripe to tax
		wantStatus   domain.InvoiceTaxStatus
		wantSubtotal int64
		wantTax      int64
		wantTotal    int64
		wantLines    []lineWant
	}{
		{
			name: "engine draft, calculation expired, same rate", inclusive: true,
			build:       engineDraft(cycle, grossLine("li_base", linePrice)),
			wantRequest: []int64{linePrice}, wantStatus: domain.InvoiceTaxOK,
			wantSubtotal: 10000, wantTax: 2000, wantTotal: linePrice,
			wantLines: []lineWant{{10000, 2000, linePrice}},
		},
		{
			name: "engine draft, calculation expired, rate now 25%", inclusive: true,
			build: engineDraft(cycle, grossLine("li_base", linePrice)), change: rateTo(2500),
			wantRequest: []int64{linePrice}, wantStatus: domain.InvoiceTaxOK,
			wantSubtotal: 9600, wantTax: 2400, wantTotal: linePrice,
			wantLines: []lineWant{{9600, 2400, linePrice}},
		},
		{
			name: "engine draft, calculation expired, provider down on the retry", inclusive: true,
			build: engineDraft(cycle, grossLine("li_base", linePrice)), change: (*stripeTaxFake).FailNext,
			wantRequest: []int64{linePrice}, wantStatus: domain.InvoiceTaxPending,
			wantSubtotal: linePrice, wantTax: 0, wantTotal: linePrice,
			wantLines: []lineWant{{linePrice, 0, linePrice}},
		},
		{
			name: "operator draft taxed at a failed finalize, calculation expired, rate now 25%", inclusive: true,
			build: operatorDraft(grossLine("li_fee", linePrice)), change: rateTo(2500),
			wantRequest: []int64{linePrice}, wantStatus: domain.InvoiceTaxOK,
			wantSubtotal: 9600, wantTax: 2400, wantTotal: linePrice,
			wantLines: []lineWant{{linePrice, 2400, linePrice}},
		},
		{
			// The line had no tax at the build. The retry stores it as a
			// successful build does: net, tax, and the price as the total.
			name: "engine draft pending since the build", inclusive: true,
			build:       pendingDraft(cycle, grossLine("li_base", linePrice)),
			wantRequest: []int64{linePrice}, wantStatus: domain.InvoiceTaxOK,
			wantSubtotal: 10000, wantTax: 2000, wantTotal: linePrice,
			wantLines: []lineWant{{10000, 2000, linePrice}},
		},
		{
			// Two lines in two forms: the base fee taxed at the build (stored
			// net), and a zero-rated line (no tax, so stored at its price).
			// The zero rate is 5% by the refresh. Both are taxed at their
			// price, and both store their new net.
			name: "engine draft, a zero-rated line taxed at the refresh", inclusive: true,
			setup: func(f *stripeTaxFake) { f.SetCodeRate(zeroRatedCode, 0) },
			build: engineDraft(cycle, grossLine("li_base", linePrice), codedLine("li_usage", 6000, zeroRatedCode)),
			change: func(f *stripeTaxFake) { f.SetCodeRate(zeroRatedCode, 500) },
			wantRequest: []int64{linePrice, 6000}, wantStatus: domain.InvoiceTaxOK,
			wantSubtotal: 15714, wantTax: 2286, wantTotal: 18000,
			wantLines: []lineWant{{10000, 2000, linePrice}, {5714, 286, 6000}},
		},
		{
			// The second line came after the failed finalize, untaxed. Both
			// keep the amount entered.
			name: "operator draft, a line added after a failed finalize, rate now 25%", inclusive: true,
			build:       operatorDraftWithLaterLine(grossLine("li_fee", linePrice), grossLine("li_extra", 6000)),
			change:      rateTo(2500),
			wantRequest: []int64{linePrice, 6000}, wantStatus: domain.InvoiceTaxOK,
			wantSubtotal: 14400, wantTax: 3600, wantTotal: 18000,
			wantLines: []lineWant{{linePrice, 2400, linePrice}, {6000, 1200, 6000}},
		},
		// Unchanged behavior: a tax-exclusive line amount is before tax.
		{
			name: "tax-exclusive engine draft, calculation expired", inclusive: false,
			build:       engineDraft(cycle, grossLine("li_base", linePrice)),
			wantRequest: []int64{linePrice}, wantStatus: domain.InvoiceTaxOK,
			wantSubtotal: linePrice, wantTax: 2400, wantTotal: 14400,
			wantLines: []lineWant{{linePrice, 2400, 14400}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newStripeTaxFake(t, 2000)
			e := newStripeTaxEngine(fake, tc.inclusive, expired)
			if tc.setup != nil {
				tc.setup(fake)
			}
			tc.build(t, e, fake)
			if tc.change != nil {
				tc.change(fake)
			}

			inv, err := e.RetryTaxForInvoice(postgres.WithLivemode(t.Context(), false), "t1", "inv_1")
			if err != nil {
				t.Fatalf("retry tax: %v", err)
			}

			reqs := fake.Requests()
			if len(reqs) != 2 {
				t.Fatalf("calculations = %d, want 2 (the first one and the retry)", len(reqs))
			}
			if got := reqs[1]; !slices.Equal(got.Amounts, tc.wantRequest) || got.Inclusive != tc.inclusive {
				t.Errorf("retry asked Stripe to tax %v (inclusive=%v), want %v (inclusive=%v): the line prices", got.Amounts, got.Inclusive, tc.wantRequest, tc.inclusive)
			}
			if inv.TaxStatus != tc.wantStatus {
				t.Errorf("tax_status = %s, want %s", inv.TaxStatus, tc.wantStatus)
			}
			assertTaxedDraft(t, e, inv, tc.wantSubtotal, tc.wantTax, tc.wantTotal, tc.wantLines)
		})
	}
}

// A proration draft whose tax failed at the build stores the plan change as
// two lines: a credit for the unused time on the old plan (negative) and a
// charge for the new one. The provider never sees a negative line, so the
// credit is folded into the charge for the calculation, and the result is
// split back over the two lines by their amounts. The first retry taxes
// the pair and stores each at its new net; the refresh after a rate change
// must split the line prices again, not the nets.
func TestRetryTax_TaxInclusiveProrationDraft_CreditLineKeepsTheTotal(t *testing.T) {
	fake := newStripeTaxFake(t, 2000)
	e := newStripeTaxEngine(fake, true, time.Now().UTC().Add(-30*time.Hour))
	credit := grossLine("li_credit", -4000)
	credit.Description = "Unused time on Basic"
	charge := grossLine("li_charge", 16000)
	charge.Description = "Remaining time on Pro"
	pendingDraft(domain.BillingReasonSubscriptionUpdate, credit, charge)(t, e, fake)
	ctx := postgres.WithLivemode(t.Context(), false)

	// 1. The pending tax is retried and succeeds.
	inv, err := e.RetryTaxForInvoice(ctx, "t1", "inv_1")
	if err != nil {
		t.Fatalf("retry tax: %v", err)
	}
	if got := fake.Requests(); len(got) != 2 || !slices.Equal(got[1].Amounts, []int64{12000}) {
		t.Fatalf("calculation requests = %+v, want the retry to tax the pair as one 12000 line", got)
	}
	assertTaxedDraft(t, e, inv, 10000, 2000, 12000, []lineWant{{-3333, -667, -4000}, {13333, 2667, 16000}})

	// 2. The calculation expires and the rate is now 25%: the refresh.
	fake.SetRate(2500)
	inv, err = e.RetryTaxForInvoice(ctx, "t1", "inv_1")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := fake.Requests(); len(got) != 3 || !slices.Equal(got[2].Amounts, []int64{12000}) {
		t.Fatalf("calculation requests = %+v, want the refresh to tax the pair's 12000 too", got)
	}
	assertTaxedDraft(t, e, inv, 9600, 2400, 12000, []lineWant{{-3200, -800, -4000}, {12800, 3200, 16000}})
}

// Who made a draft decides the amount its lines keep after a
// recomputation. Billing builds its drafts with each line at its net, and
// the recomputation stores the new net. An operator's lines keep the
// amount entered: that is the price, and the subtotal of a line added
// later sums those amounts. A draft with no billing reason was written
// before the reason was recorded; it counts as an operator's, as for
// finalize.
func TestRetryTax_TaxInclusive_LineAmountFollowsWhoMadeTheDraft(t *testing.T) {
	for _, tc := range []struct {
		reason     domain.InvoiceBillingReason
		wantAmount int64
	}{
		{domain.BillingReasonSubscriptionCycle, 10000},
		{domain.BillingReasonSubscriptionCreate, 10000},
		{domain.BillingReasonSubscriptionCancel, 10000},
		{domain.BillingReasonSubscriptionUpdate, 10000},
		{domain.BillingReasonThreshold, 10000},
		{domain.BillingReasonManual, linePrice},
		{"", linePrice},
	} {
		name := string(tc.reason)
		if name == "" {
			name = "no reason"
		}
		t.Run(name, func(t *testing.T) {
			fake := newStripeTaxFake(t, 2000)
			e := newStripeTaxEngine(fake, true, time.Now().UTC())
			pendingDraft(tc.reason, grossLine("li_1", linePrice))(t, e, fake)

			inv, err := e.RetryTaxForInvoice(postgres.WithLivemode(t.Context(), false), "t1", "inv_1")
			if err != nil {
				t.Fatalf("retry tax: %v", err)
			}
			assertTaxedDraft(t, e, inv, 10000, 2000, linePrice, []lineWant{{tc.wantAmount, 2000, linePrice}})
		})
	}
}

// assertTaxedDraft checks the invoice the retry returned (header) and the
// lines it stored, in order.
func assertTaxedDraft(t *testing.T, e *Engine, inv domain.Invoice, wantSubtotal, wantTax, wantTotal int64, wantLines []lineWant) {
	t.Helper()
	if inv.TotalAmountCents != wantTotal || inv.AmountDueCents != wantTotal {
		t.Errorf("total/due = %d/%d, want %d/%d", inv.TotalAmountCents, inv.AmountDueCents, wantTotal, wantTotal)
	}
	if inv.SubtotalCents != wantSubtotal || inv.DiscountCents != 0 || inv.TaxAmountCents != wantTax {
		t.Errorf("subtotal/discount/tax = %d/%d/%d, want %d/0/%d", inv.SubtotalCents, inv.DiscountCents, inv.TaxAmountCents, wantSubtotal, wantTax)
	}
	stored, err := e.invoices.ListLineItems(t.Context(), "t1", "inv_1")
	if err != nil || len(stored) != len(wantLines) {
		t.Fatalf("stored lines = %+v, %v; want %d", stored, err, len(wantLines))
	}
	for i, want := range wantLines {
		got := lineWant{stored[i].AmountCents, stored[i].TaxAmountCents, stored[i].TotalAmountCents}
		if got != want {
			t.Errorf("line %d (%s) amount/tax/total = %d/%d/%d, want %d/%d/%d", i, stored[i].ID, got.amount, got.tax, got.total, want.amount, want.tax, want.total)
		}
	}
}
