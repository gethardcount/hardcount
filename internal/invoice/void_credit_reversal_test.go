package invoice

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// fakeCreditReverser records the in-tx consumed-credit reversal Void threads
// through its coordinator transaction.
type fakeCreditReverser struct {
	calls     int
	reversed  int64
	err       error
	gotTenant string
	gotCust   string
	gotInvID  string
	gotInvNum string
}

func (f *fakeCreditReverser) ReverseForInvoiceTx(_ context.Context, _ *sql.Tx, tenantID, customerID, invoiceID, invoiceNumber string) (int64, error) {
	f.calls++
	f.gotTenant, f.gotCust, f.gotInvID, f.gotInvNum = tenantID, customerID, invoiceID, invoiceNumber
	return f.reversed, f.err
}

func (f *fakeCreditReverser) RetireCommitGrantForInvoiceTx(_ context.Context, _ *sql.Tx, _, _ string) (int64, error) {
	return 0, nil // non-commit invoices: clean no-op (ADR-078)
}

// TestVoid_CreditReversalAtomic pins the first-good-practice contract for the
// void → consumed-credit reversal seam: the reversal rides the SAME coordinator
// transaction as the status flip. A reversal failure rolls the void back (the
// invoice never lands voided-but-credits-still-consumed, which would silently
// strip the customer of the credits they paid the invoice with) AND short-
// circuits the post-commit tax reversal. Pre-fix the handler called
// ReverseForInvoice as a separate WARN-swallowed step after the void committed,
// with no reconciler to re-drive a transient failure.
func TestVoid_CreditReversalAtomic(t *testing.T) {
	ctx := context.Background()

	setup := func() (*Service, *memStore, *fakeCreditReverser, *fakeTaxReverser, string) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		taxRev := &fakeTaxReverser{}
		svc.SetTaxReverser(taxRev)
		rev := &fakeCreditReverser{}
		svc.SetCreditReverser(rev)
		inv, err := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		// A committed tax transaction so the post-commit reversal would fire on
		// the success path (and must NOT fire when the void rolls back).
		cur := store.invoices[inv.ID]
		cur.TaxTransactionID = "txn_1"
		store.invoices[inv.ID] = cur
		return svc, store, rev, taxRev, inv.ID
	}

	t.Run("reversal failure rolls the void back and skips tax reversal", func(t *testing.T) {
		svc, store, rev, taxRev, id := setup()
		rev.err = errors.New("ledger append failed")

		if _, err := svc.Void(ctx, "t1", id); err == nil {
			t.Fatal("Void must fail when the consumed-credit reversal fails")
		}
		if rev.calls != 1 {
			t.Errorf("reverser calls=%d, want 1", rev.calls)
		}
		if got := store.invoices[id].Status; got != domain.InvoiceFinalized {
			t.Errorf("void must roll back on reversal failure; status=%s, want finalized", got)
		}
		if len(taxRev.calls) != 0 {
			t.Errorf("post-commit tax reversal must not fire when the void rolled back; got %d calls", len(taxRev.calls))
		}
	})

	t.Run("reversal success commits the void then reverses tax post-commit", func(t *testing.T) {
		svc, store, rev, taxRev, id := setup()
		rev.reversed = 500

		voided, err := svc.Void(ctx, "t1", id)
		if err != nil {
			t.Fatalf("Void: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("returned status=%s, want voided", voided.Status)
		}
		if store.invoices[id].Status != domain.InvoiceVoided {
			t.Errorf("stored status=%s, want voided", store.invoices[id].Status)
		}
		if rev.calls != 1 {
			t.Errorf("reverser calls=%d, want 1", rev.calls)
		}
		if rev.gotTenant != "t1" || rev.gotCust != "c" || rev.gotInvID != id {
			t.Errorf("reverser got tenant=%q cust=%q inv=%q", rev.gotTenant, rev.gotCust, rev.gotInvID)
		}
		if len(taxRev.calls) != 1 {
			t.Errorf("post-commit tax reversal should fire once; got %d calls", len(taxRev.calls))
		}
	})

	t.Run("no reverser wired → void still commits", func(t *testing.T) {
		store := newMemStore()
		svc := NewService(store, nil, newMemNumberer())
		// No SetCreditReverser: a tenant that never applies customer credit.
		inv, err := svc.Create(ctx, "t1", CreateInput{
			CustomerID: "c", SubscriptionID: "s",
			BillingPeriodStart: time.Now(), BillingPeriodEnd: time.Now().AddDate(0, 1, 0),
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.Finalize(ctx, "t1", inv.ID); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		voided, err := svc.Void(ctx, "t1", inv.ID)
		if err != nil {
			t.Fatalf("Void: %v", err)
		}
		if voided.Status != domain.InvoiceVoided {
			t.Errorf("status=%s, want voided", voided.Status)
		}
	})
}
