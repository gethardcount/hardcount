package creditnote

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// A cancel's relief note may target a funding invoice that is still a DRAFT
// (pause_collection keeps cycle invoices as drafts). The engine's issue_pending
// note is allowed and Issue holds it until the invoice is finalized; once it
// is, the note reduces the amount due. An operator credit note on a draft is
// still refused — a draft is edited, not credited (ADR-118).
func TestReliefNote_DraftSourceHeldUntilFinalized(t *testing.T) {
	ctx := context.Background()
	draft := domain.Invoice{
		ID: "inv_draft", TenantID: "t1", CustomerID: "cus_1", Currency: "USD",
		Status: domain.InvoiceDraft, PaymentStatus: domain.PaymentPending,
		SubtotalCents: 10000, TotalAmountCents: 10000, AmountDueCents: 10000,
	}
	store := newMemStore()
	reader := &memInvoiceReader{invoices: map[string]domain.Invoice{draft.ID: draft}}
	s := NewService(store, reader, nil)
	s.SetNumberGenerator(&fakeCNNumbers{})

	line := []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 1000}}
	if _, err := s.Create(ctx, "t1", CreateInput{InvoiceID: draft.ID, Reason: "other", Lines: line}); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("operator credit note on a draft: err = %v, want InvalidState", err)
	}

	cn, err := s.CreateAdjustmentDraftTx(ctx, nil, "t1", draft.ID, 4000, "subscription_cancellation", "unused prebill")
	if err != nil {
		t.Fatalf("relief note on a draft source: %v", err)
	}
	held, err := s.Issue(ctx, "t1", cn.ID)
	if err != nil {
		t.Fatalf("Issue on a draft source: %v", err)
	}
	if held.Status != domain.CreditNoteDraft || reader.invoices[draft.ID].AmountDueCents != 10000 {
		t.Fatalf("note %s, amount due %d: want the note held as draft and amount due untouched",
			held.Status, reader.invoices[draft.ID].AmountDueCents)
	}

	// The invoice is finalized; the reconciler's next Issue applies the note.
	fin := reader.invoices[draft.ID]
	fin.Status = domain.InvoiceFinalized
	reader.invoices[draft.ID] = fin
	issued, err := s.Issue(ctx, "t1", cn.ID)
	if err != nil {
		t.Fatalf("Issue after finalize: %v", err)
	}
	if issued.Status != domain.CreditNoteIssued || reader.invoices[draft.ID].AmountDueCents != 6000 {
		t.Errorf("note %s, amount due %d: want issued and 6000", issued.Status, reader.invoices[draft.ID].AmountDueCents)
	}
}
