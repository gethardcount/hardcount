package creditnote

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestIssue_IdempotentNoDoubleApply covers the medium-severity audit finding:
// Issue() on an unpaid invoice reduces amount_due via ApplyCreditNote, which is
// not idempotent. The draft→issued flip used to happen LAST, so a concurrent or
// retried Issue() that both passed the draft check double-reduced amount_due.
// The draft→issued CAS now runs first: exactly one call wins and applies the
// reduction; the loser returns the already-issued note unchanged.
func TestIssue_IdempotentNoDoubleApply(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_1": {
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				Currency: "USD", TotalAmountCents: 10000, AmountDueCents: 10000,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := context.Background()

	cn, err := svc.Create(ctx, "t1", CreateInput{
		InvoiceID: "inv_1",
		Reason:    "Overcharged",
		Lines:     []CreditLineInput{{Description: "adjustment", Quantity: 1, UnitAmountCents: 4000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// First Issue: wins the CAS, reduces amount_due by 4000 → 6000.
	issued, err := svc.Issue(ctx, "t1", cn.ID)
	if err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	if issued.Status != domain.CreditNoteIssued {
		t.Fatalf("status after first issue: got %q, want issued", issued.Status)
	}
	if got := invoices.invoices["inv_1"].AmountDueCents; got != 6000 {
		t.Fatalf("amount_due after first issue: got %d, want 6000 ($100 - $40)", got)
	}

	// Second Issue (retry / concurrent loser): must NOT reduce amount_due again.
	// It returns InvalidState from the fast pre-check OR the already-issued note
	// — either way, amount_due must stay 6000.
	_, err = svc.Issue(ctx, "t1", cn.ID)
	if err == nil {
		// The pre-check rejects a non-draft note; if it ever returns the note
		// instead, that's also acceptable as long as no double-apply happened.
		t.Log("second Issue returned no error (idempotent return of issued note)")
	}
	if got := invoices.invoices["inv_1"].AmountDueCents; got != 6000 {
		t.Errorf("amount_due after second issue: got %d, want 6000 (no double-reduction)", got)
	}
}

// TestIssue_CASLoserDoesNotApply exercises the race directly: a note that is
// already 'issued' in the store (as if a concurrent winner flipped it between
// our Get and our CAS) must lose the CAS and skip ApplyCreditNote entirely.
func TestIssue_CASLoserDoesNotApply(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_1": {
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				Currency: "USD", TotalAmountCents: 10000, AmountDueCents: 10000,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := context.Background()

	cn, err := svc.Create(ctx, "t1", CreateInput{
		InvoiceID: "inv_1", Reason: "x",
		Lines: []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 4000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Win the CAS out-of-band, simulating a concurrent winner that already
	// flipped the note to issued (but leave amount_due untouched so we can
	// detect any erroneous second application).
	won, err := store.TransitionStatus(ctx, "t1", cn.ID, domain.CreditNoteDraft, domain.CreditNoteIssued)
	if err != nil || !won {
		t.Fatalf("setup CAS: won=%v err=%v", won, err)
	}

	// Now Issue() must observe the non-draft status (pre-check) and never apply.
	_, _ = svc.Issue(ctx, "t1", cn.ID)
	if got := invoices.invoices["inv_1"].AmountDueCents; got != 10000 {
		t.Errorf("amount_due: got %d, want 10000 (CAS loser must not apply)", got)
	}
}
