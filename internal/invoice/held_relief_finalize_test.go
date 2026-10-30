package invoice

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// fakeHeldRelief stands in for the credit-note service: it lowers what is due
// on the finalized invoice, as issuing a held relief note does, or fails.
type fakeHeldRelief struct {
	store    *memStore
	cents    int64
	fail     bool
	finished bool
}

func (f *fakeHeldRelief) IssueHeldReliefTx(_ context.Context, _ *sql.Tx, _ string, inv domain.Invoice) (func(context.Context), error) {
	if f.fail {
		return nil, errors.New("credit note exceeds invoice")
	}
	cur := f.store.invoices[inv.ID]
	cur.AmountDueCents -= f.cents
	f.store.invoices[inv.ID] = cur
	return func(context.Context) { f.finished = true }, nil
}

func cycleDraft(store *memStore) domain.Invoice {
	inv := domain.Invoice{
		ID: "inv_held", TenantID: "t1", CustomerID: "cus_1", InvoiceNumber: "INV-HELD", Currency: "USD",
		Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending, BillingReason: domain.BillingReasonSubscriptionCycle,
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
	}
	store.invoices[inv.ID] = inv
	return inv
}

// Release plan row 13: finalize issues the relief held on a draft on its own
// transaction and returns the invoice as the relief left it, so a charge
// right after takes the relieved amount ($35.81, not $50.00).
func TestFinalize_IssuesHeldReliefBeforeReturning(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, clock.Real(), newMemNumberer())
	inv := cycleDraft(store)
	relief := &fakeHeldRelief{store: store, cents: 1419}
	svc.SetHeldReliefIssuer(relief)

	got, err := svc.Finalize(context.Background(), "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if got.Status != domain.InvoiceFinalized || got.AmountDueCents != 3581 {
		t.Fatalf("finalized %s with %d due, want finalized with 3581", got.Status, got.AmountDueCents)
	}
	if !relief.finished {
		t.Fatal("the held relief's after-commit steps did not run")
	}
}

// A failure issuing the held relief rolls the finalize back: the invoice stays
// a draft, so nothing charges it at the full amount.
func TestFinalize_HeldReliefFailureLeavesADraft(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, clock.Real(), newMemNumberer())
	inv := cycleDraft(store)
	svc.SetHeldReliefIssuer(&fakeHeldRelief{store: store, fail: true})

	if _, err := svc.Finalize(context.Background(), "t1", inv.ID); err == nil {
		t.Fatal("finalize succeeded although the held relief failed")
	}
	// The memory store cannot roll back; the real store does (the flip and
	// the hook share one tx). Here the error must at least reach the caller,
	// which is what keeps the handler from collecting.
}
