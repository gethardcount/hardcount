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
// The fake is Stripe Tax over HTTP (stripeTaxFake), so the real provider
// reads the inclusive response. Every invoice here has one line priced at
// 12000. Tax-inclusive at 20%, that is 10000 net and 2000 tax; at 25%, 9600
// and 2400.

const linePrice = 12000

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

// engineDraft stores a draft the way the cycle close does: tax applied to
// the gross lines, then the lines and the header saved as they come out.
// Collection paused is the common reason such a draft is still open a day
// later.
func engineDraft(t *testing.T, e *Engine, fake *stripeTaxFake) {
	t.Helper()
	ctx := postgres.WithLivemode(t.Context(), false)
	lines := []domain.InvoiceLineItem{grossLine("li_base", linePrice)}
	app, err := e.ApplyTaxToLineItems(ctx, "t1", "cus_1", "USD", linePrice, 0, lines)
	if err != nil {
		t.Fatalf("build-time tax: %v", err)
	}
	total := app.SubtotalCents - app.DiscountCents + app.TaxAmountCents
	e.invoices = &mockInvoices{
		invoices: []domain.Invoice{{
			ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Currency: "USD",
			Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonSubscriptionCycle,
			SubtotalCents: app.SubtotalCents, DiscountCents: app.DiscountCents, TaxFacts: app.TaxFacts,
			TotalAmountCents: total, AmountDueCents: total,
		}},
		lineItems: lines,
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("build-time calculations = %d, want 1", len(fake.Requests()))
	}
}

// pendingEngineDraft is engineDraft with the provider down at the build, so
// the draft waits for tax with its lines at the gross.
func pendingEngineDraft(t *testing.T, e *Engine, fake *stripeTaxFake) {
	t.Helper()
	fake.FailNext()
	engineDraft(t, e, fake)
}

// operatorDraft is a draft an operator composed, taxed once by a finalize
// that then did not complete. Its lines keep what was entered.
func operatorDraft(t *testing.T, e *Engine, fake *stripeTaxFake) {
	t.Helper()
	e.invoices = &mockInvoices{
		invoices: []domain.Invoice{{
			ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Currency: "USD",
			Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonManual,
			SubtotalCents: linePrice, TotalAmountCents: linePrice, AmountDueCents: linePrice,
			TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		}},
		lineItems: []domain.InvoiceLineItem{grossLine("li_fee", linePrice)},
	}
	if _, err := e.ComputeTaxForInvoice(postgres.WithLivemode(t.Context(), false), "t1", "inv_1"); err != nil {
		t.Fatalf("finalize-time tax: %v", err)
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("finalize-time calculations = %d, want 1", len(fake.Requests()))
	}
}

func TestRetryTax_TaxInclusiveDraft_TaxesTheGross(t *testing.T) {
	expired := time.Now().UTC().Add(-30 * time.Hour)
	for _, tc := range []struct {
		name      string
		inclusive bool
		build     func(*testing.T, *Engine, *stripeTaxFake)
		// What the retry changes before it runs.
		rateBP      int64
		providerOut bool

		wantRequest    int64 // the line amount the retry asks Stripe to tax
		wantStatus     domain.InvoiceTaxStatus
		wantTax        int64
		wantSubtotal   int64
		wantLineAmount int64
		wantTotal      int64
	}{
		{
			name: "engine draft, calculation expired, same rate", inclusive: true, build: engineDraft,
			rateBP: 2000, wantRequest: linePrice,
			wantStatus: domain.InvoiceTaxOK, wantTax: 2000, wantSubtotal: 10000, wantLineAmount: 10000, wantTotal: linePrice,
		},
		{
			name: "engine draft, calculation expired, rate now 25%", inclusive: true, build: engineDraft,
			rateBP: 2500, wantRequest: linePrice,
			wantStatus: domain.InvoiceTaxOK, wantTax: 2400, wantSubtotal: 9600, wantLineAmount: 9600, wantTotal: linePrice,
		},
		{
			name: "engine draft, calculation expired, provider down on the retry", inclusive: true, build: engineDraft,
			rateBP: 2000, providerOut: true, wantRequest: linePrice,
			wantStatus: domain.InvoiceTaxPending, wantTax: 0, wantSubtotal: linePrice, wantLineAmount: linePrice, wantTotal: linePrice,
		},
		{
			name: "operator draft taxed at a failed finalize, calculation expired, rate now 25%", inclusive: true, build: operatorDraft,
			rateBP: 2500, wantRequest: linePrice,
			wantStatus: domain.InvoiceTaxOK, wantTax: 2400, wantSubtotal: 9600, wantLineAmount: linePrice, wantTotal: linePrice,
		},
		// Unchanged behavior: a draft whose tax never succeeded has its lines
		// at the gross, and a tax-exclusive line amount is before tax.
		{
			name: "engine draft pending since the build", inclusive: true, build: pendingEngineDraft,
			rateBP: 2000, wantRequest: linePrice,
			wantStatus: domain.InvoiceTaxOK, wantTax: 2000, wantSubtotal: 10000, wantLineAmount: linePrice, wantTotal: linePrice,
		},
		{
			name: "tax-exclusive engine draft, calculation expired", inclusive: false, build: engineDraft,
			rateBP: 2000, wantRequest: linePrice,
			wantStatus: domain.InvoiceTaxOK, wantTax: 2400, wantSubtotal: linePrice, wantLineAmount: linePrice, wantTotal: 14400,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newStripeTaxFake(t, 2000)
			e := newStripeTaxEngine(fake, tc.inclusive, expired)
			tc.build(t, e, fake)
			fake.SetRate(tc.rateBP)
			if tc.providerOut {
				fake.FailNext()
			}

			inv, err := e.RetryTaxForInvoice(postgres.WithLivemode(t.Context(), false), "t1", "inv_1")
			if err != nil {
				t.Fatalf("retry tax: %v", err)
			}

			reqs := fake.Requests()
			if len(reqs) != 2 {
				t.Fatalf("calculations = %d, want 2 (the first one and the retry)", len(reqs))
			}
			if got := reqs[1]; !slices.Equal(got.Amounts, []int64{tc.wantRequest}) || got.Inclusive != tc.inclusive {
				t.Errorf("retry asked Stripe to tax %v (inclusive=%v), want [%d] (inclusive=%v): the line's price", got.Amounts, got.Inclusive, tc.wantRequest, tc.inclusive)
			}
			if inv.TaxStatus != tc.wantStatus {
				t.Errorf("tax_status = %s, want %s", inv.TaxStatus, tc.wantStatus)
			}
			if inv.TotalAmountCents != tc.wantTotal || inv.AmountDueCents != tc.wantTotal {
				t.Errorf("total/due = %d/%d, want %d/%d", inv.TotalAmountCents, inv.AmountDueCents, tc.wantTotal, tc.wantTotal)
			}
			if inv.SubtotalCents != tc.wantSubtotal || inv.DiscountCents != 0 || inv.TaxAmountCents != tc.wantTax {
				t.Errorf("subtotal/discount/tax = %d/%d/%d, want %d/0/%d", inv.SubtotalCents, inv.DiscountCents, inv.TaxAmountCents, tc.wantSubtotal, tc.wantTax)
			}

			stored, err := e.invoices.ListLineItems(t.Context(), "t1", "inv_1")
			if err != nil || len(stored) != 1 {
				t.Fatalf("stored lines = %+v, %v; want one", stored, err)
			}
			li := stored[0]
			if li.AmountCents != tc.wantLineAmount || li.TaxAmountCents != tc.wantTax || li.TotalAmountCents != tc.wantTotal {
				t.Errorf("line amount/tax/total = %d/%d/%d, want %d/%d/%d", li.AmountCents, li.TaxAmountCents, li.TotalAmountCents, tc.wantLineAmount, tc.wantTax, tc.wantTotal)
			}
		})
	}
}
