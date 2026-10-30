package invoice

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// finalizeFailingStore fails the draft→finalized write, the one durable step
// of finalize for an engine invoice.
type finalizeFailingStore struct {
	*memStore
}

func (s finalizeFailingStore) FinalizeDraft(context.Context, string, string, func(*sql.Tx, domain.Invoice) error) (domain.Invoice, error) {
	return domain.Invoice{}, errors.New("connection reset")
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

		processed, errs := svc.RetryPendingTax(postgres.WithLivemode(context.Background(), true), 50)
		if processed != 0 || len(errs) != 1 {
			t.Errorf("RetryPendingTax = %d processed, %d errors; want 0 and 1", processed, len(errs))
		}
	})
}

// rivalFinalizingRetrier resolves the tax, then a rival finalizes the row
// before RetryTax's own finalize reads it: the connect fan-out and the
// tax_retry sweep retrying the same provider_not_configured row, or an
// operator's Finalize click.
type rivalFinalizingRetrier struct {
	stubTaxRetrier
}

func (f *rivalFinalizingRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	inv, err := f.stubTaxRetrier.RetryTaxForInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		return inv, err
	}
	if _, err := f.store.UpdateStatus(ctx, tenantID, invoiceID, domain.InvoiceFinalized); err != nil {
		return domain.Invoice{}, err
	}
	return inv, nil
}

// TestRetryTax_RivalFinalizeWonIsNotAnError: when a rival finalized the
// invoice first, nothing is owed. RetryTax must not report it as "not
// finalized … finalize it", and the sweep must not log an ERROR for it. The
// draft→finalized CAS half of this race is pinned on real Postgres in
// TestRetryTax_RivalFinalizeWinsTheCAS.
func TestRetryTax_RivalFinalizeWonIsNotAnError(t *testing.T) {
	mem := newMemStore()
	mem.invoices["inv_1"] = taxPendingCycleDraft("inv_1")
	svc := NewService(mem, nil, nil)
	svc.SetTaxRetrier(&rivalFinalizingRetrier{stubTaxRetrier{store: mem}})

	out, err := svc.RetryTax(context.Background(), "t1", "inv_1")
	if err != nil {
		t.Fatalf("RetryTax: %v; want nil, the invoice is finalized and nothing is owed", err)
	}
	if out.Status != domain.InvoiceFinalized {
		t.Errorf("returned status = %s, want finalized (the row as it is now)", out.Status)
	}
}

// TestRetryTaxHandler_FinalizeFailureStillAudited: a click whose tax write
// committed and whose finalize then failed still changed the invoice's tax and
// total, so it writes its retry_tax audit row before answering the error.
func TestRetryTaxHandler_FinalizeFailureStillAudited(t *testing.T) {
	mem := newMemStore()
	draft := taxPendingCycleDraft("inv_1")
	draft.InvoiceNumber = "INV-001"
	mem.invoices["inv_1"] = draft
	svc := NewService(finalizeFailingStore{mem}, nil, nil)
	svc.SetTaxRetrier(&stubTaxRetrier{store: mem})
	aud := &capturingInvoiceAudit{}
	h := &Handler{svc: svc, auditLogger: aud}

	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/inv_1/retry-tax", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "inv_1")
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.retryTax(rr, req.WithContext(reqCtx))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (finalize failed on a database error). body=%s", rr.Code, rr.Body.String())
	}
	entry, ok := aud.firstOf(domain.AuditActionRetryTax)
	if !ok {
		t.Fatalf("no retry_tax audit row; got actions=%v. The click rewrote the invoice's tax and total", aud.actions())
	}
	if entry.label != "INV-001" || entry.meta["tax_status"] != domain.InvoiceTaxOK {
		t.Errorf("audit label = %q, tax_status = %v; want INV-001 and ok", entry.label, entry.meta["tax_status"])
	}
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
// lost lease cancels the tick, each tax loop stops before its next row. Before,
// every remaining row was attempted and logged a "context canceled" error. A
// sweep adds no error for the stop, as processAutoCharge does: its next tick
// lists the same rows. retryStuck serves the one-shot fan-outs (Stripe
// connect, billing-profile save), which no tick repeats, so its stop is one
// error naming the rows it left.
func TestTaxRetryLoops_StopBetweenRowsOnceCtxIsDone(t *testing.T) {
	cases := []struct {
		name string
		// run drives one loop over two rows with a fake that cancels ctx
		// during the first; it returns the loop's result and the fake's calls.
		run func(ctx context.Context, cancel context.CancelFunc) (processed int, errs []error, calls int)
		// oneShot: no next tick lists the rows left, so the stop is an error.
		oneShot bool
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
		}, false},
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
		}, true},
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
		}, false},
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
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(postgres.WithLivemode(context.Background(), true))
			defer cancel()
			processed, errs, calls := c.run(ctx, cancel)
			if calls != 1 {
				t.Errorf("rows attempted = %d, want 1: the loop must stop before the next row once ctx is done", calls)
			}
			switch {
			case c.oneShot && (len(errs) != 1 || !strings.Contains(errs[0].Error(), "1 of 2")):
				t.Errorf("errors = %v, want one naming the 1 of 2 rows left: no tick retries a fan-out", errs)
			case !c.oneShot && len(errs) != 0:
				t.Errorf("errors = %v, want none: the next tick lists the rows left", errs)
			}
			if processed != 1 {
				t.Errorf("processed = %d, want 1 (row 1 completed before the cancel was seen)", processed)
			}
		})
	}
}
