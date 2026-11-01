package credit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

// TestGrantCap_MessageNamesTheCapInTheFieldsUnit: every credit inflow is
// capped at 100,000,000 cents (1,000,000.00). The refusal said "cannot exceed
// 1,000,000" on a field counted in cents, which reads as 1,000,000 cents
// (10,000.00), a hundredth of the real cap.
//
// Mutation: put "cannot exceed 1,000,000" back on any site and its case fails.
func TestGrantCap_MessageNamesTheCapInTheFieldsUnit(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore())
	const over = 100_000_001
	cases := map[string]func() error{
		"Grant": func() error {
			_, err := svc.Grant(ctx, "tnt_1", GrantInput{CustomerID: "cus_1", Currency: "USD", AmountCents: over, Description: "x"})
			return err
		},
		"GrantTx": func() error {
			_, err := svc.GrantTx(ctx, nil, "tnt_1", GrantInput{CustomerID: "cus_1", Currency: "USD", AmountCents: over, Description: "x"})
			return err
		},
		"GrantCommitForInvoiceTx": func() error {
			_, err := svc.GrantCommitForInvoiceTx(ctx, nil, "tnt_1", "cus_1", "inv_1", "INV-1", "USD", over, nil)
			return err
		},
		"GrantForCreditNoteTx": func() error {
			_, err := svc.GrantForCreditNoteTx(ctx, nil, "tnt_1", "cn_1", CreditNoteReturnInput{CustomerID: "cus_1", Currency: "USD", AmountCents: over, Description: "x", InvoiceID: "inv_1"})
			return err
		},
		"Adjust": func() error {
			_, err := svc.Adjust(ctx, "tnt_1", AdjustInput{CustomerID: "cus_1", Currency: "USD", AmountCents: over, Description: "x"})
			return err
		},
	}
	for name, call := range cases {
		err := call()
		if !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s over the cap: err = %v, want a validation error", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "100000000 (1,000,000.00 in that currency)") {
			t.Errorf("%s over the cap: message %q does not name the cap in cents", name, err.Error())
		}
	}
}
