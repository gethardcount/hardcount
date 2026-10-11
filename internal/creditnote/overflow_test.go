package creditnote

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// An absurd line (3e9 × 4e9 cents) used to wrap int64 to a NEGATIVE total
// that passed the "not more than the invoice" cap. It is refused now, and so
// is a line total close enough to MaxInt64 that existing + new would wrap.
func TestCreate_OverflowingLinesAreRefused(t *testing.T) {
	inv := domain.Invoice{
		ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
		Status: domain.InvoiceFinalized, Currency: "USD",
		TotalAmountCents: 10000, AmountDueCents: 10000,
	}
	for _, tc := range []struct {
		name  string
		lines []CreditLineInput
	}{
		{"one line wraps", []CreditLineInput{{Description: "typo", Quantity: 3_000_000_000, UnitAmountCents: 4_000_000_000}}},
		{"two lines wrap when added", []CreditLineInput{
			{Description: "a", Quantity: 1, UnitAmountCents: math.MaxInt64 - 10},
			{Description: "b", Quantity: 1, UnitAmountCents: 100},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}, nil)
			svc.SetNumberGenerator(&fakeCNNumbers{})
			_, err := svc.Create(context.Background(), "t1", CreateInput{InvoiceID: inv.ID, Reason: "adjustment", Lines: tc.lines})
			if err == nil || !strings.Contains(err.Error(), "too large") {
				t.Fatalf("err = %v, want the line refused as too large", err)
			}
		})
	}

	// A line just under the cap's headroom but huge is refused by the cap, not
	// let through by a wrapped sum.
	store := newMemStore()
	svc := NewService(store, &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	if _, err := svc.Create(context.Background(), "t1", CreateInput{InvoiceID: inv.ID, Reason: "adjustment",
		Lines: []CreditLineInput{{Description: "huge", Quantity: 1, UnitAmountCents: math.MaxInt64 - 1}}}); err == nil {
		t.Fatal("a credit note for MaxInt64-1 cents on a $100 invoice must be refused")
	}
}
