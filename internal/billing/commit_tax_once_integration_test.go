package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Issue #1271: with a tax provider on, a prepaid commit used to be taxed
// twice: once on its purchase invoice, and again on the usage invoices the
// credit paid (credit pays amount_due, which includes tax). Velox follows
// Lago (ADR-078 amendment 2026-10-19): the purchase is an advance payment
// and is not taxed; the usage is taxed when it is invoiced.

// setManualTax turns on the manual provider at a flat rate for the tenant.
func setManualTax(t *testing.T, ctx context.Context, settings *tenant.SettingsStore, tenantID string, rate float64) {
	t.Helper()
	ts, err := settings.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.TaxProvider = "manual"
	ts.TaxRate = rate
	ts.TaxName = "VAT"
	if _, err := settings.Upsert(ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
}

// createCommitInvoice composes a one-line commit invoice through the real
// composer path (Create → buildLineItem → CreateWithLineItems).
func createCommitInvoice(t *testing.T, ctx context.Context, svc *invoice.Service, tenantID, customerID string, priceCents, grantCents int64) domain.Invoice {
	t.Helper()
	inv, err := svc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: customerID,
		Currency:   "USD",
		LineItems: []invoice.AddLineItemInput{{
			Description: "Prepaid commit", LineType: "add_on", Quantity: 1,
			UnitAmountCents: priceCents, CommitGrantedCents: &grantCents,
		}},
	})
	if err != nil {
		t.Fatalf("create commit invoice: %v", err)
	}
	return inv
}

func newReliefService(db *postgres.DB, invStore *invoice.PostgresStore, creditSvc *credit.Service, settings *tenant.SettingsStore) *creditnote.Service {
	svc := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil, &cnGrantAdapter{creditSvc})
	svc.SetNumberGenerator(settings)
	return svc
}

// TestCommitTax_PurchaseUntaxed_UsageTaxedOnce_E2E: manual tax at 10%. The
// customer buys $100 of credit for $100 (finalized through the operator's
// finalize path), then uses $20 of metered calls (billed by the real cycle
// close). Tax across the two invoices is $2: 10% of the usage, once.
func TestCommitTax_PurchaseUntaxed_UsageTaxedOnce_E2E(t *testing.T) {
	lf := newLiveFixture(t, "Commit Tax Once", 0)
	ctx := lf.ctx
	setManualTax(t, ctx, lf.settings, lf.tenantID, 10)

	// The operator's finalize path, wired as the router wires it: the store
	// funds the commit grant in the finalize tx, the engine computes tax.
	lf.invStore.SetCommitFunder(lf.creditSvc)
	invSvc := invoice.NewService(lf.invStore, clock.Real(), lf.settings)
	invSvc.SetTaxRetrier(lf.engine)

	// 1. The purchase: a commit invoice carries no tax.
	draft := createCommitInvoice(t, ctx, invSvc, lf.tenantID, lf.customerID, 10000, 10000)
	purchase, err := invSvc.Finalize(ctx, lf.tenantID, draft.ID)
	if err != nil {
		t.Fatalf("finalize commit invoice: %v", err)
	}
	if purchase.TaxStatus != domain.InvoiceTaxOK || purchase.TaxAmountCents != 0 || purchase.TotalAmountCents != 10000 {
		t.Fatalf("commit invoice tax_status/tax/total = %s/%d/%d, want ok/0/10000", purchase.TaxStatus, purchase.TaxAmountCents, purchase.TotalAmountCents)
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
	if _, err := lf.invStore.MarkPaid(ctx, lf.tenantID, purchase.ID, "out_of_band:test", time.Now().UTC()); err != nil {
		t.Fatalf("pay the commit invoice: %v", err)
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

// TestCommitRelief_TaxedPurchase_StillReversesItsTax: a commit bought before
// the change carried tax on its purchase invoice. Refunding it must still
// reverse that tax. The fixture writes the old invoice's tax directly, since
// no current path produces a taxed commit line.
func TestCommitRelief_TaxedPurchase_StillReversesItsTax(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Commit Taxed Before")
	settings := tenant.NewSettingsStore(db)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_taxed_commit", DisplayName: "Taxed Commit"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	invStore := invoice.NewPostgresStore(db)
	invStore.SetCommitFunder(creditSvc)
	invSvc := invoice.NewService(invStore, clock.Real(), settings)

	draft := createCommitInvoice(t, ctx, invSvc, tenantID, cust.ID, 10000, 10000)
	if _, err := invSvc.Finalize(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices SET tax_amount_cents = 1000, tax_rate = 10, tax_provider = 'manual',
			total_amount_cents = 11000, amount_due_cents = 11000
		WHERE id = $1`, draft.ID); err != nil {
		t.Fatalf("seed the old invoice's tax: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoice_line_items SET tax_amount_cents = 1000, tax_rate = 10, total_amount_cents = 11000
		WHERE invoice_id = $1`, draft.ID); err != nil {
		t.Fatalf("seed the old line's tax: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := invStore.MarkPaid(ctx, tenantID, draft.ID, "out_of_band:test", time.Now().UTC()); err != nil {
		t.Fatalf("pay: %v", err)
	}

	cn, err := newReliefService(db, invStore, creditSvc, settings).CreateAndIssueCommitRelief(ctx, tenantID, creditnote.CommitReliefInput{
		InvoiceID: draft.ID, RetireAll: true,
	})
	if err != nil {
		t.Fatalf("commit relief: %v", err)
	}
	if cn.TotalCents != 11000 || cn.TaxAmountCents != 1000 || cn.SubtotalCents != 10000 {
		t.Errorf("relief total/tax/subtotal = %d/%d/%d, want 11000/1000/10000 (the tax the purchase carried comes back)", cn.TotalCents, cn.TaxAmountCents, cn.SubtotalCents)
	}
}
