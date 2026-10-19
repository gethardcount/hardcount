package invoice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// finalizeFailingStore fails the draft→finalized write, the one durable step
// of finalize for an engine invoice.
type finalizeFailingStore struct {
	*memStore
}

func (s finalizeFailingStore) UpdateStatus(ctx context.Context, tenantID, id string, status domain.InvoiceStatus) (domain.Invoice, error) {
	if status == domain.InvoiceFinalized {
		return domain.Invoice{}, errors.New("connection reset")
	}
	return s.memStore.UpdateStatus(ctx, tenantID, id, status)
}

func taxPendingCycleDraft(id string) domain.Invoice {
	return domain.Invoice{
		ID: id, TenantID: "t1", CustomerID: "cus_a",
		Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		TaxFacts:      domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: "provider_outage"},
		BillingReason: domain.BillingReasonSubscriptionCycle, Currency: "USD",
		TotalAmountCents: 5000, AmountDueCents: 5000, CreatedAt: time.Now().UTC(),
	}
}

// TestRetryTax_FinalizeFailureIsAnError (G7, T3): when the tax resolves but
// finalize fails, the row is a draft with tax ok, which no sweep lists again.
// RetryTax must say so, so the sweep counts it as an error, not as advanced.
func TestRetryTax_FinalizeFailureIsAnError(t *testing.T) {
	t.Run("RetryTax returns the error; the tax result stays", func(t *testing.T) {
		mem := newMemStore()
		mem.invoices["inv_1"] = taxPendingCycleDraft("inv_1")
		svc := NewService(finalizeFailingStore{mem}, nil, nil)
		svc.SetTaxRetrier(&stubTaxRetrier{store: mem})

		if _, err := svc.RetryTax(context.Background(), "t1", "inv_1"); err == nil {
			t.Fatal("RetryTax = nil after a failed finalize: the sweep counts a draft no sweep revisits as advanced")
		}
		got := mem.invoices["inv_1"]
		if got.Status != domain.InvoiceDraft || got.TaxStatus != domain.InvoiceTaxOK {
			t.Errorf("status = %s, tax_status = %s; want draft with tax ok (the tax write is kept)", got.Status, got.TaxStatus)
		}
	})

	t.Run("RetryPendingTax counts it as an error, not as advanced", func(t *testing.T) {
		mem := newMemStore()
		mem.invoices["inv_1"] = taxPendingCycleDraft("inv_1")
		svc := NewService(finalizeFailingStore{mem}, nil, nil)
		svc.SetTaxRetrier(&stubTaxRetrier{store: mem})

		processed, errs := svc.RetryPendingTax(context.Background(), 50)
		if processed != 0 || len(errs) != 1 {
			t.Errorf("RetryPendingTax = %d processed, %d errors; want 0 and 1", processed, len(errs))
		}
	})
}

// cancellingRetrier resolves tax like stubTaxRetrier and cancels the caller's
// ctx on its first call: a SIGTERM or a lost lease while row 1 is in flight.
// Like the real engine, it fails at once on a done ctx.
type cancellingRetrier struct {
	store  *memStore
	cancel context.CancelFunc
	calls  int
}

func (f *cancellingRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return domain.Invoice{}, err
	}
	defer f.cancel()
	return (&stubTaxRetrier{store: f.store}).RetryTaxForInvoice(ctx, tenantID, invoiceID)
}

func (f *cancellingRetrier) ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	return (&stubTaxRetrier{store: f.store}).ComputeTaxForInvoice(ctx, tenantID, invoiceID)
}

// cancellingCommitter is cancellingRetrier for the tax_commit sweep.
type cancellingCommitter struct {
	cancel context.CancelFunc
	calls  int
}

func (f *cancellingCommitter) CheckTaxCalculationUsable(context.Context, string, string, string) error {
	return nil
}

func (f *cancellingCommitter) CommitTax(ctx context.Context, _, _, _ string) error {
	f.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	f.cancel()
	return nil
}

// cancellingReverser is cancellingRetrier for the tax_reversal sweep.
type cancellingReverser struct {
	cancel context.CancelFunc
	calls  int
}

func (f *cancellingReverser) ReverseTax(ctx context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	f.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.cancel()
	return &tax.ReversalResult{TransactionID: "tax_rev_" + req.Reference}, nil
}

// TestTaxRetryLoops_StopBetweenRowsOnceCtxIsDone (RT-O1): once shutdown or a
// lost lease cancels the tick, each tax loop stops before its next row and
// adds no error for the stop, as processAutoCharge does. Before, every
// remaining row was attempted and logged a "context canceled" error.
func TestTaxRetryLoops_StopBetweenRowsOnceCtxIsDone(t *testing.T) {
	cases := []struct {
		name string
		// run drives one loop over two rows with a fake that cancels ctx
		// during the first; it returns the loop's result and the fake's calls.
		run func(ctx context.Context, cancel context.CancelFunc) (processed int, errs []error, calls int)
	}{
		{"processTaxRetryBatch (tax_retry sweep, clock catch-up)", func(ctx context.Context, cancel context.CancelFunc) (int, []error, int) {
			mem := newMemStore()
			rows := []domain.Invoice{taxPendingCycleDraft("inv_1"), taxPendingCycleDraft("inv_2")}
			for _, inv := range rows {
				mem.invoices[inv.ID] = inv
			}
			svc := NewService(mem, nil, nil)
			f := &cancellingRetrier{store: mem, cancel: cancel}
			svc.SetTaxRetrier(f)
			n, errs := svc.processTaxRetryBatch(ctx, rows)
			return n, errs, f.calls
		}},
		{"retryStuck (connect and billing-profile fan-outs)", func(ctx context.Context, cancel context.CancelFunc) (int, []error, int) {
			mem := newMemStore()
			rows := []domain.Invoice{taxPendingCycleDraft("inv_1"), taxPendingCycleDraft("inv_2")}
			for _, inv := range rows {
				mem.invoices[inv.ID] = inv
			}
			svc := NewService(mem, nil, nil)
			f := &cancellingRetrier{store: mem, cancel: cancel}
			svc.SetTaxRetrier(f)
			n, errs := svc.retryStuck(ctx, "t1", rows)
			return n, errs, f.calls
		}},
		{"RetryPendingTaxCommit (tax_commit sweep)", func(ctx context.Context, cancel context.CancelFunc) (int, []error, int) {
			mem := newMemStore()
			for _, id := range []string{"inv_1", "inv_2"} {
				mem.invoices[id] = domain.Invoice{
					ID: id, TenantID: "t1", Status: domain.InvoiceFinalized,
					TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax", TaxCalculationID: "taxcalc_" + id},
				}
			}
			svc := NewService(mem, nil, nil)
			f := &cancellingCommitter{cancel: cancel}
			svc.SetTaxCommitter(f)
			n, errs := svc.RetryPendingTaxCommit(ctx, 50)
			return n, errs, f.calls
		}},
		{"RetryPendingTaxReversal (tax_reversal sweep)", func(ctx context.Context, cancel context.CancelFunc) (int, []error, int) {
			mem := newMemStore()
			for _, id := range []string{"inv_1", "inv_2"} {
				mem.invoices[id] = domain.Invoice{
					ID: id, TenantID: "t1", Status: domain.InvoiceVoided,
					TaxFacts:         domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK, TaxProvider: "stripe_tax"},
					TaxTransactionID: "tx_" + id, TotalAmountCents: 1000, AmountDueCents: 1000,
				}
			}
			svc := NewService(mem, nil, nil)
			f := &cancellingReverser{cancel: cancel}
			svc.SetTaxReverser(f)
			n, errs := svc.RetryPendingTaxReversal(ctx, 50)
			return n, errs, f.calls
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			processed, errs, calls := c.run(ctx, cancel)
			if calls != 1 {
				t.Errorf("rows attempted = %d, want 1: the loop must stop before the next row once ctx is done", calls)
			}
			if len(errs) != 0 {
				t.Errorf("errors = %v, want none: a stop is not a failed row", errs)
			}
			if processed != 1 {
				t.Errorf("processed = %d, want 1 (row 1 completed before the cancel was seen)", processed)
			}
		})
	}
}
