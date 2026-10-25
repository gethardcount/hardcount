package invoice

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// A manual invoice's lines are each checked (quantity × unit amount must
// fit int64 cents); their sum is checked too. Lines whose sum does not fit
// are refused with nothing stored, never saved with a wrapped subtotal.
// Amounts this large (near 9.2 × 10^18 cents) are not realistic.
func TestCreate_LinesPastInt64Refused(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, newMemNumberer())
	_, err := svc.Create(context.Background(), "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems: []AddLineItemInput{
			{Description: "First", Quantity: 1, UnitAmountCents: 5_000_000_000_000_000_000},
			{Description: "Second", Quantity: 1, UnitAmountCents: 5_000_000_000_000_000_000},
		},
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if len(store.invoices) != 0 {
		t.Fatalf("%d invoice(s) stored, want 0", len(store.invoices))
	}

	// Lines that sum to MaxInt64 are taken.
	inv, err := svc.Create(context.Background(), "t1", CreateInput{
		CustomerID: "cus_1",
		LineItems: []AddLineItemInput{
			{Description: "First", Quantity: 1, UnitAmountCents: 5_000_000_000_000_000_000},
			{Description: "Second", Quantity: 1, UnitAmountCents: 4_223_372_036_854_775_807},
		},
	})
	if err != nil {
		t.Fatalf("lines that fit refused: %v", err)
	}
	if inv.SubtotalCents != 9_223_372_036_854_775_807 {
		t.Errorf("subtotal = %d, want 9223372036854775807", inv.SubtotalCents)
	}
}

// The PDF's per-jurisdiction tax rows are sums of line taxes. A row that
// does not fit int64 cents leaves the breakdown out (the PDF then shows the
// invoice's tax total alone) rather than print a wrapped amount.
func TestAggregateTaxByJurisdiction_SumPastInt64LeavesBreakdownOut(t *testing.T) {
	line := func(j string, cents int64) domain.InvoiceLineItem {
		return domain.InvoiceLineItem{TaxJurisdiction: j, TaxRate: 20, TaxAmountCents: cents}
	}
	rows := aggregateTaxByJurisdiction([]domain.InvoiceLineItem{line("DE", 5), line("FR", 7), line("DE", 6)})
	if len(rows) != 2 || rows[0].amount != 11 || rows[1].amount != 7 {
		t.Fatalf("rows = %+v, want DE 11 then FR 7", rows)
	}
	if rows := aggregateTaxByJurisdiction([]domain.InvoiceLineItem{
		line("DE", 5_000_000_000_000_000_000), line("FR", 1), line("DE", 5_000_000_000_000_000_000),
	}); rows != nil {
		t.Errorf("rows = %+v, want no breakdown", rows)
	}
}
