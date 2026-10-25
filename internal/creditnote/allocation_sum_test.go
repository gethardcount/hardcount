package creditnote

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// A paid invoice's credit note is allocated across refund, credit and
// out-of-band amounts, each 0 or more, which must add up to the note's
// total. Their sum is checked against int64: amounts whose sum does not fit
// are refused like any other allocation that does not add up.
func TestCreate_AllocationSumPastInt64Refused(t *testing.T) {
	inv := cardInvoice() // paid by card: 5362
	svc, _ := mkRailSvc(inv)
	_, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: inv.ID, Reason: "adjustment",
		Lines:                []CreditLineInput{{Description: "partial", Quantity: 1, UnitAmountCents: 1000}},
		RefundAmountCents:    1002,
		CreditAmountCents:    math.MaxInt64,
		OutOfBandAmountCents: math.MaxInt64,
	})
	if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "allocation" {
		t.Fatalf("err = %v, want a validation error on allocation", err)
	}
	notes, lerr := svc.List(context.Background(), ListFilter{TenantID: "t1", InvoiceID: inv.ID})
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	if len(notes) != 0 {
		t.Errorf("%d credit notes written, want none", len(notes))
	}

	// Inside int64 the same check holds as before: amounts that add up to
	// the total are taken.
	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: inv.ID, Reason: "adjustment",
		Lines:             []CreditLineInput{{Description: "partial", Quantity: 1, UnitAmountCents: 1000}},
		RefundAmountCents: 400, CreditAmountCents: 500, OutOfBandAmountCents: 100,
	})
	if err != nil {
		t.Fatalf("valid allocation refused: %v", err)
	}
	if cn.RefundAmountCents != 400 || cn.CreditAmountCents != 500 || cn.OutOfBandAmountCents != 100 || cn.TotalCents != 1000 {
		t.Errorf("credit note %+v, want 400/500/100 of 1000", cn)
	}
}
