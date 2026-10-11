package invoice

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

var errBlip = errors.New("connection reset")

type failingCreditNoteTotaler struct{}

func (failingCreditNoteTotaler) IssuedCreditedCents(context.Context, string, string) (int64, error) {
	return 0, errBlip
}

func (failingCreditNoteTotaler) UnreliefedClawbackCents(context.Context, string, string) (int64, error) {
	return 0, nil
}

// An unreadable credit-noted total fails the tax reversal (marker stays NULL,
// the sweep re-drives it). It used to fall back to amount_paid + amount_due,
// which under-reverses when customer credit was applied, and the short
// reversal was filed for good under the one inv_taxrev_<id> reference.
func TestTaxReversal_CreditedTotalReadErrorFails(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	rev := &fakeTaxReverser{}
	svc.SetTaxReverser(rev)
	svc.SetCreditNoteTotaler(failingCreditNoteTotaler{})
	// $10 total, $4 covered by customer credit: the proxy would reverse only $6.
	store.invoices["inv_rev"] = domain.Invoice{
		ID: "inv_rev", TenantID: "t1", CustomerID: "c",
		Status:           domain.InvoiceVoided,
		TaxFacts:         domain.TaxFacts{TaxProvider: "stripe_tax", TaxStatus: domain.InvoiceTaxOK},
		TaxTransactionID: "tx_1",
		TotalAmountCents: 1000, AmountDueCents: 600,
	}

	recovered, errs := svc.RetryPendingTaxReversal(context.Background(), 50)
	if recovered != 0 || len(errs) != 1 {
		t.Fatalf("recovered=%d errs=%d, want 0/1", recovered, len(errs))
	}
	if len(rev.calls) != 0 {
		t.Fatalf("ReverseTax called %d time(s) on a guessed amount (%+v), want 0", len(rev.calls), rev.calls)
	}
	if store.invoices["inv_rev"].TaxReversedAt != nil {
		t.Fatal("tax_reversed_at stamped; the sweep would never retry")
	}
}

// A customer read error fails a manual Create: is_simulated is durable, and a
// test-clock invoice stored as real would count as revenue.
func TestCreate_CustomerReadErrorFails(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	svc.SetCustomerReader(&spyCustomerReader{err: errBlip})
	_, err := svc.Create(context.Background(), "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems:  []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
	})
	if !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the customer read error", err)
	}
	if len(store.invoices) != 0 {
		t.Fatalf("%d invoice(s) stored on an unread test-clock pin, want 0", len(store.invoices))
	}
}
