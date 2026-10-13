package billing_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// prorationInvoices bridges the invoice store and the invoice numberer to the
// handler's proration writer, as prorationInvoiceCreatorAdapter does in
// internal/api/adapters.go.
type prorationInvoices struct {
	*invoice.PostgresStore
	numberer invoice.InvoiceNumberer
}

func (a prorationInvoices) CreateInvoiceWithLineItems(ctx context.Context, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.CreateWithLineItems(ctx, tenantID, inv, items)
}

func (a prorationInvoices) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	return a.CreateWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

func (a prorationInvoices) NextInvoiceNumber(ctx context.Context, tenantID string) (string, error) {
	return a.numberer.NextInvoiceNumber(ctx, tenantID)
}

func (a prorationInvoices) NextInvoiceNumberTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, error) {
	return a.numberer.NextInvoiceNumberTx(ctx, tx, tenantID)
}

// prorationTax bridges the engine's tax step to the handler, as
// prorationTaxApplierAdapter does in internal/api/adapters.go.
type prorationTax struct{ engine *billing.Engine }

func (a prorationTax) ApplyTaxToLineItems(ctx context.Context, tenantID, customerID, currency string, subtotal, discount int64, lineItems []domain.InvoiceLineItem) (subscription.ProrationTaxResult, error) {
	r, err := a.engine.ApplyTaxToLineItems(ctx, tenantID, customerID, currency, subtotal, discount, lineItems)
	if err != nil {
		return subscription.ProrationTaxResult{}, err
	}
	return subscription.ProrationTaxResult{TaxFacts: r.TaxFacts, SubtotalCents: r.SubtotalCents, DiscountCents: r.DiscountCents}, nil
}

func (a prorationTax) CommitTax(ctx context.Context, tenantID, invoiceID, calculationID string) error {
	return a.engine.CommitTax(ctx, tenantID, invoiceID, calculationID)
}

// TestImmediateCancel_SameDayAsUnpaidUpgrade_VoidsUpgradeInvoice: a quantity
// increase ten days into a prepaid period writes a proration invoice through
// the real handler; the customer has not paid it when the subscription is
// canceled an hour later. Nothing of the upgrade was used, so its invoice is
// voided outright, and the paid day-1 invoice is credited its own unused
// share and no more.
//
// The proration writer leaves its base-fee lines with no period of their own
// (only the header is stamped), which is the shape the relief's
// "only prepaid base" check has to recognize. With that check reading line
// periods alone the upgrade invoice was never voided: it stayed finalized
// with a credit note against it.
//
// Mutation seam: make onlyPrepaidBase reject a line with no period → the
// upgrade invoice is still finalized after the cancel.
func TestImmediateCancel_SameDayAsUnpaidUpgrade_VoidsUpgradeInvoice(t *testing.T) {
	r := newCancelRig(t, "991 same-day upgrade")
	p0 := r.f.cycleStart
	changeAt := p0.AddDate(0, 0, 10)
	cancelAt := changeAt.Add(time.Hour)
	r.exec(t, `UPDATE plans SET base_bill_timing = 'in_advance' WHERE id = $1`, r.f.planID)
	day1, err := r.f.engine.BillOnCreate(r.at(p0), r.sub(t))
	if err != nil || day1.SubtotalCents != cancelFixtureBaseCents {
		t.Fatalf("day-1 invoice: %v subtotal=%dc, want %dc", err, day1.SubtotalCents, cancelFixtureBaseCents)
	}
	if _, err := r.f.invStore.MarkPaid(r.base, r.f.tenantID, day1.ID, "pi_day1", p0); err != nil {
		t.Fatalf("pay day-1 invoice: %v", err)
	}

	// Quantity 1 → 2 through the real handler, wired as internal/api/router.go
	// wires it. No credit granter: an increase grants nothing.
	h := subscription.NewHandler(r.f.subSvc)
	h.SetDB(r.f.db)
	h.SetProrationDeps(r.f.pricingSvc, prorationInvoices{PostgresStore: r.f.invStore, numberer: r.f.settings}, nil)
	h.SetProrationTaxApplier(prorationTax{engine: r.f.engine})
	h.SetTenantLocator(r.f.engine)
	h.SetNetTermsReader(r.f.engine)
	req := httptest.NewRequest(http.MethodPatch, "/"+r.f.subID+"/items/"+r.f.itemID, strings.NewReader(`{"quantity":2,"immediate":true}`))
	req = req.WithContext(auth.WithKeyID(auth.WithTenantID(r.at(changeAt), r.f.tenantID), "vlx_key_cancel_991"))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("quantity increase: HTTP %d %s, want 200", rec.Code, rec.Body.String())
	}

	var upgrade domain.Invoice
	for _, inv := range r.f.listInvoices(t, r.base) {
		if inv.BillingReason == domain.BillingReasonSubscriptionUpdate {
			upgrade = inv
		}
	}
	if upgrade.ID == "" || upgrade.Status != domain.InvoiceFinalized || upgrade.AmountPaidCents != 0 || upgrade.SubtotalCents <= 0 {
		t.Fatalf("upgrade invoice: id=%q status=%s amount_paid=%dc subtotal=%dc, want one finalized, unpaid, above zero",
			upgrade.ID, upgrade.Status, upgrade.AmountPaidCents, upgrade.SubtotalCents)
	}
	// The shape this test exists for. If the writer starts stamping line
	// periods, this test no longer covers the rows already stored without one.
	upLines, err := r.f.invStore.ListLineItems(r.base, r.f.tenantID, upgrade.ID)
	if err != nil || len(upLines) != 2 {
		t.Fatalf("upgrade invoice lines: %v, %d lines, want the credit and charge pair", err, len(upLines))
	}
	for _, li := range upLines {
		if li.LineType != domain.LineTypeBaseFee || li.BillingPeriodStart != nil {
			t.Fatalf("upgrade line: type=%s period_start=%v, want base_fee with no period", li.LineType, li.BillingPeriodStart)
		}
	}

	canceled, credited, err := r.f.subSvc.Cancel(r.at(cancelAt), r.f.tenantID, r.f.subID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != domain.SubscriptionCanceled {
		t.Fatalf("cancel: status=%s, want canceled", canceled.Status)
	}

	after, err := r.f.invStore.Get(r.base, r.f.tenantID, upgrade.ID)
	if err != nil {
		t.Fatalf("get upgrade invoice: %v", err)
	}
	if after.Status != domain.InvoiceVoided {
		t.Errorf("upgrade invoice status = %s, want voided: none of the upgrade was used and none of it was paid", after.Status)
	}
	// One seat's unused share comes back on the paid day-1 invoice. The voided
	// upgrade invoice was that same share for the second seat, so the two agree
	// to the rounding cent; a double relief would credit twice as much.
	bal, err := r.credit.GetBalance(r.base, r.f.tenantID, r.f.customerID)
	if err != nil {
		t.Fatalf("credit balance: %v", err)
	}
	if diff := credited - upgrade.SubtotalCents; credited <= 0 || diff < -1 || diff > 1 || bal.BalanceCents != credited {
		t.Errorf("credited=%dc balance=%dc, want both within a cent of one seat's unused share (%dc)",
			credited, bal.BalanceCents, upgrade.SubtotalCents)
	}
	if d, err := r.f.invStore.Get(r.base, r.f.tenantID, day1.ID); err != nil || d.Status == domain.InvoiceVoided || d.AmountPaidCents != cancelFixtureBaseCents {
		t.Errorf("day-1 invoice: %v status=%s amount_paid=%dc, want it live and paid %dc", err, d.Status, d.AmountPaidCents, cancelFixtureBaseCents)
	}
}
