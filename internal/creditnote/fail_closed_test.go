package creditnote

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// blipFirstGet fails the first invoice read only — the gate's — so create()'s
// own re-read succeeds, which is exactly how the gate used to be bypassed.
type blipFirstGet struct {
	*memInvoiceReader
	blipped bool
}

func (b *blipFirstGet) Get(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	if !b.blipped {
		b.blipped = true
		return domain.Invoice{}, errors.New("connection reset")
	}
	return b.memInvoiceReader.Get(ctx, tenantID, id)
}

// A read error on the in-flight-payment gate fails the operator credit note.
// It used to fall through to create(), so a credit note could be created while
// a charge was in flight whenever create's re-read happened to succeed.
func TestCreate_InFlightGateReadErrorFails(t *testing.T) {
	inv := domain.Invoice{
		ID: "inv_proc", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentProcessing, Currency: "USD",
		TotalAmountCents: 10000, AmountDueCents: 10000,
	}
	store := newMemStore()
	s := NewService(store, &blipFirstGet{memInvoiceReader: &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}}, nil)
	s.SetNumberGenerator(&fakeCNNumbers{})

	_, err := s.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_proc", Reason: "downgrade",
		Lines: []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 1000}},
	})
	if err == nil {
		t.Fatal("Create succeeded past an unreadable in-flight gate")
	}
	if len(store.notes) != 0 {
		t.Fatalf("%d credit note(s) created while a charge may be in flight, want 0", len(store.notes))
	}
}
