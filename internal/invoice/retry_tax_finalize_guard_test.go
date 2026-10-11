package invoice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// unreadableGuardCommitter is a TaxCommitter whose expiry guard cannot read
// its inputs (the engine's guard now fails closed on a read error).
type unreadableGuardCommitter struct{}

func (unreadableGuardCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return errors.New("tax expiry guard: look up calculation age: connection reset")
}
func (unreadableGuardCommitter) CommitTax(context.Context, string, string, string) error { return nil }

// The auto-finalize after a successful tax retry finalizes a calculation made
// moments earlier in the same call, so it skips the expiry guard: run there,
// a guard read error left the freshly-taxed invoice a draft that no sweep
// retries (the tax retry only revisits pending/failed tax). An operator
// Finalize still runs the guard and still fails closed.
func TestRetryTax_AutoFinalizeSkipsTheExpiryGuard(t *testing.T) {
	store := newMemStore()
	store.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_a",
		Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage",
			TaxCalculationID: "taxcalc_fresh",
		},
		BillingReason: "subscription_cycle", Currency: "USD",
		TotalAmountCents: 5000, AmountDueCents: 5000, CreatedAt: time.Now().UTC(),
	}
	svc := NewService(store, nil, nil)
	svc.SetTaxRetrier(&stubTaxRetrier{store: store})
	svc.SetTaxCommitter(unreadableGuardCommitter{})

	out, err := svc.RetryTax(context.Background(), "t1", "inv_1")
	if err != nil {
		t.Fatalf("RetryTax: %v", err)
	}
	if out.Status != domain.InvoiceFinalized {
		t.Fatalf("status = %s, want finalized (the guard must not run on the auto-finalize)", out.Status)
	}

	// The operator's Finalize on a different draft still runs the guard.
	store.invoices["inv_2"] = domain.Invoice{
		ID: "inv_2", TenantID: "t1", Status: domain.InvoiceDraft,
		BillingReason: "subscription_cycle",
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK, TaxCalculationID: "taxcalc_old"},
	}
	if _, err := svc.Finalize(context.Background(), "t1", "inv_2"); err == nil {
		t.Fatal("an operator Finalize with an unreadable guard must fail closed")
	}
}
