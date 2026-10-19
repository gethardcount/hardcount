package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// finalizeRecorderMock records what the proration writer asks the billing
// engine for: the finalize audit row (with the tx it rides) and the
// invoice.finalized webhook, in call order.
type finalizeRecorderMock struct {
	auditErr   error
	audits     []domain.Invoice
	auditTxs   []*sql.Tx
	dispatched []domain.Invoice
	calls      []string
}

func (m *finalizeRecorderMock) EmitFinalizeAuditTx(_ context.Context, tx *sql.Tx, inv domain.Invoice) error {
	m.calls = append(m.calls, "audit")
	if m.auditErr != nil {
		return m.auditErr
	}
	m.audits = append(m.audits, inv)
	m.auditTxs = append(m.auditTxs, tx)
	return nil
}

func (m *finalizeRecorderMock) DispatchInvoiceFinalized(_ context.Context, _ string, inv domain.Invoice) {
	m.calls = append(m.calls, "dispatch")
	m.dispatched = append(m.dispatched, inv)
}

// prorationFinalizeRig is an upgrade (Basic → Pro, 15 of 30 days left) on the
// non-atomic path (no db wired) against a paid source invoice.
type prorationFinalizeRig struct {
	h        *Handler
	invoices *invoicesMock
	rec      *finalizeRecorderMock
	subID    string
	itemID   string
}

func newProrationFinalizeRig(t *testing.T) *prorationFinalizeRig {
	t.Helper()
	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, "t1", "cus_1", "plan_basic", proPeriodStart, proPeriodEnd)
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{sourceInvoice: domain.Invoice{
		ID: "src_inv", PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 2000, TotalAmountCents: 2000, AmountPaidCents: 2000,
	}}
	rec := &finalizeRecorderMock{}
	h := NewHandler(NewService(store, nil))
	h.SetProrationDeps(plans, invoices, &creditsMock{})
	h.SetInvoiceFinalizeRecorder(rec)
	return &prorationFinalizeRig{h: h, invoices: invoices, rec: rec, subID: subID, itemID: itemID}
}

func (r *prorationFinalizeRig) upgrade(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.WithValue(clock.WithEffectiveNow(context.Background(), proNow), auth.TestTenantIDKey(), "t1")
	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_pro", Immediate: true})
	rr := httptest.NewRecorder()
	r.h.updateItem(rr, updateItemURL(ctx, r.subID, r.itemID, body))
	return rr
}

// TestProration_NonAtomic_FinalizedInvoice_AuditThenDispatchOnce (#1279): the
// non-atomic path writes the finalize row inside the invoice's own create tx
// (the store hands emit the tx), then sends invoice.finalized once, for the
// invoice it created.
func TestProration_NonAtomic_FinalizedInvoice_AuditThenDispatchOnce(t *testing.T) {
	r := newProrationFinalizeRig(t)
	if rr := r.upgrade(t); rr.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", rr.Code, rr.Body.String())
	}
	if len(r.invoices.createdInvoices) != 1 {
		t.Fatalf("created %d proration invoices, want 1", len(r.invoices.createdInvoices))
	}
	created := r.invoices.createdInvoices[0]
	if created.Status != domain.InvoiceFinalized || created.BillingReason != domain.BillingReasonSubscriptionUpdate {
		t.Fatalf("created invoice status=%s reason=%s, want finalized subscription_update", created.Status, created.BillingReason)
	}
	if len(r.rec.audits) != 1 || r.rec.audits[0].ID != created.ID {
		t.Errorf("finalize audit rows: %v, want one for %s", r.rec.audits, created.ID)
	}
	if len(r.rec.dispatched) != 1 || r.rec.dispatched[0].ID != created.ID {
		t.Errorf("invoice.finalized dispatches: %v, want one for %s", r.rec.dispatched, created.ID)
	}
	if got := r.rec.calls; len(got) != 2 || got[0] != "audit" || got[1] != "dispatch" {
		t.Errorf("call order %v, want [audit dispatch]: the row commits with the invoice, the webhook goes out after", got)
	}
}

// TestProration_DraftInvoice_NoAuditNoDispatch: tax deferred → the proration
// invoice is born a draft. It gets neither the row nor the webhook here;
// invoice.Service.Finalize writes both when it finalizes.
func TestProration_DraftInvoice_NoAuditNoDispatch(t *testing.T) {
	r := newProrationFinalizeRig(t)
	r.h.SetProrationTaxApplier(&prorationTaxApplierMock{err: errors.New("tax provider unreachable")})
	if rr := r.upgrade(t); rr.Code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", rr.Code, rr.Body.String())
	}
	if len(r.invoices.createdInvoices) != 1 || r.invoices.createdInvoices[0].Status != domain.InvoiceDraft {
		t.Fatalf("created %v, want one draft proration invoice (tax pending)", r.invoices.createdInvoices)
	}
	if len(r.rec.audits) != 0 || len(r.rec.dispatched) != 0 {
		t.Errorf("draft proration invoice: %d audit rows, %d dispatches, want none", len(r.rec.audits), len(r.rec.dispatched))
	}
}

// TestProration_DedupReplay_NoAuditNoDispatch: a replay of the same item
// change finds the invoice the first attempt created; that attempt already
// wrote the row and sent the webhook, so the replay sends nothing.
func TestProration_DedupReplay_NoAuditNoDispatch(t *testing.T) {
	r := newProrationFinalizeRig(t)
	r.invoices.createInvoiceErr = errs.AlreadyExists("proration_source",
		"proration invoice already exists for this item change").WithCode("invoice_proration_source_taken")
	r.invoices.lookupInvoice = domain.Invoice{ID: "vlx_inv_prior", Status: domain.InvoiceFinalized, TotalAmountCents: 2000}
	if rr := r.upgrade(t); rr.Code != http.StatusOK {
		t.Fatalf("upgrade replay: %d %s", rr.Code, rr.Body.String())
	}
	if len(r.rec.calls) != 0 {
		t.Errorf("dedup replay called the recorder %v, want nothing", r.rec.calls)
	}
}

// TestProration_NonAtomic_AuditFailure_NoInvoiceNoDispatch: the row shares the
// invoice's fate, so a failed audit write leaves no invoice and sends no
// webhook (the error surfaces as proration_failed).
func TestProration_NonAtomic_AuditFailure_NoInvoiceNoDispatch(t *testing.T) {
	r := newProrationFinalizeRig(t)
	r.rec.auditErr = errors.New("audit insert failed")
	rr := r.upgrade(t)
	if rr.Code < 500 {
		t.Fatalf("upgrade with a failing audit write: %d %s, want a 5xx", rr.Code, rr.Body.String())
	}
	if len(r.invoices.createdInvoices) != 0 {
		t.Errorf("invoice kept after its audit write failed: %v", r.invoices.createdInvoices)
	}
	if len(r.rec.dispatched) != 0 {
		t.Errorf("invoice.finalized sent for an invoice that was rolled back: %v", r.rec.dispatched)
	}
}
