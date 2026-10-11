package creditnote

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// A void reverses the invoice's tax minus what ISSUED credit notes already
// reversed. A still-pending draft reversed nothing and is voided along with the
// invoice, so it must not be subtracted — counting it under-reversed the
// tenant's output tax by the draft's share.
func TestIssuedCreditedCents_CountsOnlyIssued(t *testing.T) {
	store := newMemStore()
	for id, st := range map[string]domain.CreditNoteStatus{
		"cn_issued": domain.CreditNoteIssued,
		"cn_draft":  domain.CreditNoteDraft,
		"cn_voided": domain.CreditNoteVoided,
	} {
		store.notes[id] = domain.CreditNote{ID: id, TenantID: "t1", InvoiceID: "inv_1", Status: st, TotalCents: 1000}
	}
	s := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{}}, nil)
	got, err := s.IssuedCreditedCents(context.Background(), "t1", "inv_1")
	if err != nil {
		t.Fatalf("IssuedCreditedCents: %v", err)
	}
	if got != 1000 {
		t.Errorf("issued credited = %d, want 1000 (the issued note only; the draft has reversed no tax)", got)
	}
}
