package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
)

// Issue #1271: with a tax provider on, a prepaid commit used to be taxed
// twice: once on its purchase invoice, and again on the usage invoices the
// credit paid (credit pays amount_due, which includes tax). Hardcount follows
// Lago (ADR-078 amendment 2026-10-19): the purchase is an advance payment
// and is not taxed; the usage is taxed when it is invoiced.

// setManualTax turns on the manual provider at a flat rate for the tenant.
func setManualTax(t *testing.T, ctx context.Context, settings *tenant.SettingsStore, tenantID string, rate float64) {
	t.Helper()
	if _, err := settings.Update(ctx, tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxProvider = "manual"
		ts.TaxRate = rate
		ts.TaxName = "VAT"
		return nil
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
}

// buyTopup creates a top-up selling grantCents of credit for priceCents and
// records its payment, which issues it and grants the credit. svc's store
// needs a commit funder and an invoice numberer.
func buyTopup(t *testing.T, ctx context.Context, svc *invoice.Service, tenantID, customerID string, priceCents, grantCents int64) domain.Invoice {
	t.Helper()
	draft, err := svc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: customerID,
		Currency:   "USD",
		LineItems: []invoice.AddLineItemInput{{
			Description: "Prepaid credit", Quantity: 1,
			UnitAmountCents: priceCents, CommitGrantedCents: &grantCents,
		}},
	})
	if err != nil {
		t.Fatalf("create top-up: %v", err)
	}
	paid, err := svc.RecordOfflinePayment(ctx, tenantID, draft.ID, "Wire")
	if err != nil {
		t.Fatalf("record the top-up's payment: %v", err)
	}
	return paid
}

func newReliefService(db *postgres.DB, invStore *invoice.PostgresStore, creditSvc *credit.Service, settings *tenant.SettingsStore) *creditnote.Service {
	svc := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil, &cnGrantAdapter{creditSvc})
	svc.SetNumberGenerator(settings)
	return svc
}

// TestCommitTax_PurchaseUntaxed_UsageTaxedOnce_E2E: manual tax at 10%. The
// customer buys $100 of credit for $100 on a top-up, then uses $20 of metered
// calls (billed by the real cycle close). Tax across the two invoices is $2:
// 10% of the usage, once.
func TestCommitTax_PurchaseUntaxed_UsageTaxedOnce_E2E(t *testing.T) {
	lf := newLiveFixture(t, "Commit Tax Once", 0)
	ctx := lf.ctx
	setManualTax(t, ctx, lf.settings, lf.tenantID, 10)

	// Wired as the router wires it: the top-up's payment numbers it and
	// grants the credit in its transaction.
	lf.invStore.SetCommitFunder(lf.creditSvc)
	lf.invStore.SetInvoiceNumberer(lf.settings)
	invSvc := invoice.NewService(lf.invStore, clock.Real(), lf.settings)

	// 1. The purchase: a top-up carries no tax.
	purchase := buyTopup(t, ctx, invSvc, lf.tenantID, lf.customerID, 10000, 10000)
	if purchase.TaxStatus != domain.InvoiceTaxOK || purchase.TaxAmountCents != 0 || purchase.TotalAmountCents != 10000 {
		t.Fatalf("top-up tax_status/tax/total = %s/%d/%d, want ok/0/10000", purchase.TaxStatus, purchase.TaxAmountCents, purchase.TotalAmountCents)
	}
	lines, err := lf.invStore.ListLineItems(ctx, lf.tenantID, purchase.ID)
	if err != nil || len(lines) != 1 {
		t.Fatalf("commit lines: %v, %+v", err, lines)
	}
	if lines[0].TaxAmountCents != 0 || lines[0].TaxRate != 0 || lines[0].TaxabilityReason != "prepaid_credit_purchase" {
		t.Errorf("commit line tax/rate/reason = %d/%g/%q, want 0/0/prepaid_credit_purchase", lines[0].TaxAmountCents, lines[0].TaxRate, lines[0].TaxabilityReason)
	}
	if bal := lf.balance(t); bal != 10000 {
		t.Fatalf("credit after the purchase = %d, want 10000", bal)
	}

	// 2. The usage: taxed when invoiced, and the credit pays it in full.
	lf.insertUsage(t, lf.meterID, 2000, lf.cycleStart.Add(time.Hour)) // 2000 calls × 1¢
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close made %d invoices, want 1", n)
	}
	var usageInv domain.Invoice
	for _, inv := range lf.listInvoices(t, ctx) {
		if inv.SubscriptionID == lf.subID {
			usageInv = inv
		}
	}
	if usageInv.ID == "" {
		t.Fatal("no usage invoice after the cycle close")
	}
	if usageInv.SubtotalCents != 2000 || usageInv.TaxAmountCents != 200 || usageInv.TotalAmountCents != 2200 {
		t.Fatalf("usage invoice subtotal/tax/total = %d/%d/%d, want 2000/200/2200", usageInv.SubtotalCents, usageInv.TaxAmountCents, usageInv.TotalAmountCents)
	}
	if usageInv.CreditsAppliedCents != 2200 || usageInv.AmountDueCents != 0 {
		t.Errorf("usage invoice credits applied/due = %d/%d, want 2200/0 (the commit credit pays it, tax included)", usageInv.CreditsAppliedCents, usageInv.AmountDueCents)
	}
	if bal := lf.balance(t); bal != 7800 {
		t.Errorf("credit after the usage invoice = %d, want 7800", bal)
	}

	// 3. Taxed once: the only tax is 10% of the usage.
	if got := purchase.TaxAmountCents + usageInv.TaxAmountCents; got != 200 {
		t.Errorf("tax across the purchase and the usage = %d, want 200 (10%% of $20 of usage, once)", got)
	}

	// 4. Refunding the unused credit reverses no tax: the purchase carried none.
	cn, err := newReliefService(lf.db, lf.invStore, lf.creditSvc, lf.settings).CreateAndIssueCommitRelief(ctx, lf.tenantID, creditnote.CommitReliefInput{
		InvoiceID: purchase.ID, RetireAll: true, Reason: "unused credit",
	})
	if err != nil {
		t.Fatalf("commit relief: %v", err)
	}
	if cn.TotalCents != 7800 || cn.TaxAmountCents != 0 || cn.SubtotalCents != 7800 {
		t.Errorf("relief total/tax/subtotal = %d/%d/%d, want 7800/0/7800", cn.TotalCents, cn.TaxAmountCents, cn.SubtotalCents)
	}
}
