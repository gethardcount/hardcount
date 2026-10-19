package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// A prepaid-commit purchase is an advance payment, not a supply: its line is
// left out of the tax request and carries tax 0 with a reason. The usage the
// credit later pays is taxed on its own invoice, so tax is charged once
// (ADR-078 amendment 2026-10-19, issue #1271). These tests pin the one
// chokepoint every tax path shares, ApplyTaxToLineItems.

// recordingProvider records each request it is asked to tax and delegates
// the arithmetic to the manual provider, so the tests assert what the engine
// SENDS, not provider math.
type recordingProvider struct {
	inner tax.Provider
	reqs  []tax.Request
}

func (*recordingProvider) Name() string { return "manual" }
func (p *recordingProvider) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	p.reqs = append(p.reqs, req)
	return p.inner.Calculate(ctx, req)
}
func (*recordingProvider) Commit(_ context.Context, _, _ string) (string, error) { return "", nil }
func (*recordingProvider) Reverse(_ context.Context, _ tax.ReversalRequest) (*tax.ReversalResult, error) {
	return &tax.ReversalResult{}, nil
}

func newRecordingEngine(rateBP int64, inclusive bool) (*Engine, *recordingProvider) {
	prov := &recordingProvider{inner: tax.NewManualProvider(float64(rateBP)/100, "VAT")}
	return &Engine{
		settings:     &taxSettings{provider: "manual", rateBP: rateBP, name: "VAT", inclusive: inclusive},
		taxProviders: stubResolver(prov),
	}, prov
}

func commitLine(amountCents, grantCents int64) domain.InvoiceLineItem {
	return domain.InvoiceLineItem{
		Description: "Prepaid commit", LineType: domain.LineTypeAddOn,
		Quantity: 1, AmountCents: amountCents, CommitGrantedCents: &grantCents,
	}
}

// assertPrepaidLine checks a commit line came out untaxed, with the reason.
func assertPrepaidLine(t *testing.T, li domain.InvoiceLineItem, wantAmount int64) {
	t.Helper()
	if li.TaxAmountCents != 0 || li.TaxRate != 0 {
		t.Errorf("commit line taxed: tax=%d rate=%g, want 0/0", li.TaxAmountCents, li.TaxRate)
	}
	if li.AmountCents != wantAmount || li.TotalAmountCents != wantAmount {
		t.Errorf("commit line amount/total = %d/%d, want %d/%d (no tax added or carved)", li.AmountCents, li.TotalAmountCents, wantAmount, wantAmount)
	}
	if li.TaxabilityReason != "prepaid_credit_purchase" {
		t.Errorf("commit line tax_reason = %q, want prepaid_credit_purchase", li.TaxabilityReason)
	}
}

func TestApplyTax_CommitLineLeftOutOfTheTaxRequest(t *testing.T) {
	e, prov := newRecordingEngine(1000, false) // 10%
	lines := []domain.InvoiceLineItem{
		commitLine(10000, 10000),
		{Description: "Onboarding", LineType: domain.LineTypeAddOn, Quantity: 1, AmountCents: 5000},
	}

	app, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 15000, 0, lines)
	if err != nil {
		t.Fatalf("apply tax: %v", err)
	}
	if len(prov.reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prov.reqs))
	}
	if got := prov.reqs[0].LineItems; len(got) != 1 || got[0].AmountCents != 5000 {
		t.Fatalf("request lines = %+v, want only the 5000 onboarding line (the commit line stays out)", got)
	}
	if app.TaxAmountCents != 500 {
		t.Errorf("invoice tax = %d, want 500 (10%% of the onboarding line only)", app.TaxAmountCents)
	}
	if app.TaxStatus != domain.InvoiceTaxOK || app.SubtotalCents != 15000 {
		t.Errorf("status/subtotal = %s/%d, want ok/15000", app.TaxStatus, app.SubtotalCents)
	}
	assertPrepaidLine(t, lines[0], 10000)
	if lines[1].TaxAmountCents != 500 || lines[1].TotalAmountCents != 5500 {
		t.Errorf("onboarding line tax/total = %d/%d, want 500/5500", lines[1].TaxAmountCents, lines[1].TotalAmountCents)
	}
}

// An invoice that only sells credit has nothing to tax: the provider is not
// called at all, so a provider outage cannot park it either.
func TestApplyTax_CommitOnlyInvoice_NoProviderCall(t *testing.T) {
	prov := &countingProvider{stubProvider: stubProvider{err: errors.New("provider must not be called")}}
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(prov),
	}
	lines := []domain.InvoiceLineItem{commitLine(900000, 1000000)}

	app, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 900000, 0, lines)
	if err != nil {
		t.Fatalf("apply tax: %v", err)
	}
	if prov.calls != 0 {
		t.Fatalf("provider calls = %d, want 0", prov.calls)
	}
	if app.TaxStatus != domain.InvoiceTaxOK || app.TaxAmountCents != 0 || app.TaxCalculationID != "" {
		t.Errorf("status/tax/calc = %s/%d/%q, want ok/0/empty", app.TaxStatus, app.TaxAmountCents, app.TaxCalculationID)
	}
	if app.SubtotalCents != 900000 {
		t.Errorf("subtotal = %d, want 900000", app.SubtotalCents)
	}
	assertPrepaidLine(t, lines[0], 900000)
}

// Tax-inclusive pricing carves tax out of taxable lines only. The commit
// line's price is what the customer pays for the credit; nothing is carved.
func TestApplyTax_CommitLine_TaxInclusive(t *testing.T) {
	e, prov := newRecordingEngine(1800, true) // 18% inclusive
	lines := []domain.InvoiceLineItem{
		commitLine(11800, 11800),
		{Description: "Onboarding", LineType: domain.LineTypeAddOn, Quantity: 1, AmountCents: 11800},
	}

	app, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 23600, 0, lines)
	if err != nil {
		t.Fatalf("apply tax: %v", err)
	}
	if len(prov.reqs) != 1 || len(prov.reqs[0].LineItems) != 1 {
		t.Fatalf("requests = %+v, want one request with one line", prov.reqs)
	}
	if app.TaxAmountCents != 1800 {
		t.Errorf("invoice tax = %d, want 1800 (carved from the onboarding line only)", app.TaxAmountCents)
	}
	if app.SubtotalCents != 21800 || app.DiscountCents != 0 {
		t.Errorf("subtotal/discount = %d/%d, want 21800/0 (11800 commit + 10000 net onboarding)", app.SubtotalCents, app.DiscountCents)
	}
	if got := app.SubtotalCents - app.DiscountCents + app.TaxAmountCents; got != 23600 {
		t.Errorf("subtotal − discount + tax = %d, want the gross 23600", got)
	}
	assertPrepaidLine(t, lines[0], 11800)
}

// A provider outage parks a mixed commit invoice as pending. The commit line
// is stamped untaxed on that branch too, so the stored line says why it has
// no tax while the rest of the invoice waits.
func TestApplyTax_CommitLine_ProviderFailure_StampedOnThePendingInvoice(t *testing.T) {
	prov := &countingProvider{stubProvider: stubProvider{err: errors.New("stripe tax: 503")}}
	e := &Engine{
		settings:     &taxSettings{provider: "stripe_tax"},
		taxProviders: stubResolver(prov),
	}
	lines := []domain.InvoiceLineItem{
		commitLine(10000, 10000),
		{Description: "Onboarding", LineType: domain.LineTypeAddOn, Quantity: 1, AmountCents: 5000},
	}

	app, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 15000, 0, lines)
	if err != nil {
		t.Fatalf("apply tax: %v", err)
	}
	if prov.calls != 1 || app.TaxStatus != domain.InvoiceTaxPending {
		t.Fatalf("provider calls/status = %d/%s, want 1/pending", prov.calls, app.TaxStatus)
	}
	assertPrepaidLine(t, lines[0], 10000)
	if lines[1].TaxabilityReason != "" || lines[1].TotalAmountCents != 5000 {
		t.Errorf("onboarding line reason/total = %q/%d, want empty/5000 (waiting for tax)", lines[1].TaxabilityReason, lines[1].TotalAmountCents)
	}
}

// An account on the none provider collects no tax, so its invoices say
// nothing about tax on a commit line: no reason, so no dashboard label and no
// PDF line about prepaid credit.
func TestApplyTax_CommitLine_NoneProvider_NoReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []domain.InvoiceLineItem
	}{
		{"commit only", []domain.InvoiceLineItem{commitLine(10000, 10000)}},
		{"commit and fee", []domain.InvoiceLineItem{
			commitLine(10000, 10000),
			{Description: "Onboarding", LineType: domain.LineTypeAddOn, Quantity: 1, AmountCents: 5000},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{
				settings:     &taxSettings{provider: "none"},
				taxProviders: stubResolver(tax.NewNoneProvider()),
			}
			var subtotal int64
			for _, li := range tc.lines {
				subtotal += li.AmountCents
			}
			app, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", subtotal, 0, tc.lines)
			if err != nil {
				t.Fatalf("apply tax: %v", err)
			}
			if app.TaxStatus != domain.InvoiceTaxOK || app.TaxAmountCents != 0 {
				t.Errorf("status/tax = %s/%d, want ok/0", app.TaxStatus, app.TaxAmountCents)
			}
			c := tc.lines[0]
			if c.TaxabilityReason != "" {
				t.Errorf("commit line tax_reason = %q, want empty under the none provider", c.TaxabilityReason)
			}
			if c.TaxAmountCents != 0 || c.TotalAmountCents != 10000 {
				t.Errorf("commit line tax/total = %d/%d, want 0/10000", c.TaxAmountCents, c.TotalAmountCents)
			}
		})
	}
}

// A tax-inclusive draft whose tax was computed BEFORE the commit line was
// left out stored a header subtotal with the commit line carved (the stored
// lines keep what was entered: UpdateTaxAtomic writes no line amount). If
// finalize failed after that compute, the draft is finalized again after
// deploy. It must re-price from its lines, not from that carved subtotal, or
// the customer is invoiced the net while the full credit is granted.
func TestComputeTax_InclusiveCommitDraftComputedBeforeTheChange(t *testing.T) {
	grant := int64(11800)
	commit := domain.InvoiceLineItem{
		ID: "li_commit", InvoiceID: "inv_1", Description: "Prepaid commit", LineType: domain.LineTypeAddOn,
		Quantity: 1, AmountCents: 11800, TaxRate: 18, TaxAmountCents: 1800, TotalAmountCents: 11800,
		CommitGrantedCents: &grant,
	}
	fee := domain.InvoiceLineItem{
		ID: "li_fee", InvoiceID: "inv_1", Description: "Onboarding", LineType: domain.LineTypeAddOn,
		Quantity: 1, AmountCents: 11800, TaxRate: 18, TaxAmountCents: 1800, TotalAmountCents: 11800,
	}
	for _, tc := range []struct {
		name                  string
		lines                 []domain.InvoiceLineItem
		oldSubtotal, oldTax   int64
		wantSubtotal, wantTax int64
		wantTotal             int64
		wantProviderCalls     int
	}{
		// $118 commit at 18% inclusive: the old compute stored subtotal $100.
		{"commit only", []domain.InvoiceLineItem{commit}, 10000, 1800, 11800, 0, 11800, 0},
		// $118 commit + $118 fee: the old compute stored subtotal $200.
		{"commit and fee", []domain.InvoiceLineItem{commit, fee}, 20000, 3600, 21800, 1800, 23600, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, prov := newRecordingEngine(1800, true)
			e.invoices = &mockInvoices{
				invoices: []domain.Invoice{{
					ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Currency: "USD", Status: domain.InvoiceDraft,
					SubtotalCents: tc.oldSubtotal, TotalAmountCents: tc.oldSubtotal + tc.oldTax, AmountDueCents: tc.oldSubtotal + tc.oldTax,
					TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK, TaxAmountCents: tc.oldTax, TaxRate: 18},
				}},
				lineItems: append([]domain.InvoiceLineItem(nil), tc.lines...),
			}

			inv, err := e.ComputeTaxForInvoice(context.Background(), "t1", "inv_1")
			if err != nil {
				t.Fatalf("compute tax: %v", err)
			}
			if len(prov.reqs) != tc.wantProviderCalls {
				t.Fatalf("provider calls = %d, want %d", len(prov.reqs), tc.wantProviderCalls)
			}
			if inv.SubtotalCents != tc.wantSubtotal || inv.DiscountCents != 0 || inv.TaxAmountCents != tc.wantTax {
				t.Errorf("subtotal/discount/tax = %d/%d/%d, want %d/0/%d", inv.SubtotalCents, inv.DiscountCents, inv.TaxAmountCents, tc.wantSubtotal, tc.wantTax)
			}
			if inv.TotalAmountCents != tc.wantTotal || inv.AmountDueCents != tc.wantTotal {
				t.Errorf("total/due = %d/%d, want %d/%d (what the lines say the customer owes)", inv.TotalAmountCents, inv.AmountDueCents, tc.wantTotal, tc.wantTotal)
			}
			stored, _ := e.invoices.ListLineItems(context.Background(), "t1", "inv_1")
			for _, li := range stored {
				if li.ID == "li_commit" {
					assertPrepaidLine(t, li, 11800)
				}
			}
		})
	}
}

// The retry family rebuilds the request from STORED lines. A mixed commit
// invoice parked pending (a provider outage at finalize) must keep its
// commit line out on the retry too.
func TestRetryTax_CommitLineStaysOutOfTheRetriedRequest(t *testing.T) {
	e, prov := newRecordingEngine(1000, false)
	grant := int64(10000)
	e.invoices = &mockInvoices{
		invoices: []domain.Invoice{{
			ID: "inv_1", TenantID: "t1", CustomerID: "cus_1", Currency: "USD",
			Status: domain.InvoiceDraft, SubtotalCents: 15000, TotalAmountCents: 15000, AmountDueCents: 15000,
			TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage"},
		}},
		lineItems: []domain.InvoiceLineItem{
			{ID: "li_commit", InvoiceID: "inv_1", Description: "Prepaid commit", LineType: domain.LineTypeAddOn, Quantity: 1, AmountCents: 10000, TotalAmountCents: 10000, CommitGrantedCents: &grant},
			{ID: "li_fee", InvoiceID: "inv_1", Description: "Onboarding", LineType: domain.LineTypeAddOn, Quantity: 1, AmountCents: 5000, TotalAmountCents: 5000},
		},
	}

	inv, err := e.RetryTaxForInvoice(context.Background(), "t1", "inv_1")
	if err != nil {
		t.Fatalf("retry tax: %v", err)
	}
	if len(prov.reqs) != 1 || len(prov.reqs[0].LineItems) != 1 || prov.reqs[0].LineItems[0].AmountCents != 5000 {
		t.Fatalf("retried request = %+v, want only the 5000 onboarding line", prov.reqs)
	}
	if inv.TaxStatus != domain.InvoiceTaxOK || inv.TaxAmountCents != 500 || inv.TotalAmountCents != 15500 {
		t.Errorf("invoice status/tax/total = %s/%d/%d, want ok/500/15500", inv.TaxStatus, inv.TaxAmountCents, inv.TotalAmountCents)
	}
	stored, _ := e.invoices.ListLineItems(context.Background(), "t1", "inv_1")
	for _, li := range stored {
		if li.ID == "li_commit" {
			assertPrepaidLine(t, li, 10000)
		}
	}
}
