package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tax"
)

// A stored proration pair (a credit line and the charge that absorbs it) is
// taxed as one net line, and the tax is split back across the pair by their
// amounts (expandTaxResultLines). The shares are worked in 128 bits: exact
// whenever they fit int64 cents, ErrAmountOverflow when one does not.
// Amounts this large are not realistic; they used to wrap.
func TestApplyTaxToLineItems_SplitPairTaxShares(t *testing.T) {
	apply := func(credit, charge, groupTax int64) ([]domain.InvoiceLineItem, error) {
		net := charge + credit
		stub := &stubProvider{result: &tax.Result{
			Provider: "stub", TotalTaxCents: groupTax,
			Lines: []tax.ResultLine{{Ref: "line_0", NetAmountCents: net, TaxAmountCents: groupTax}},
		}}
		e := &Engine{settings: &taxSettings{provider: "stripe_tax"}, taxProviders: stubResolver(stub)}
		lines := []domain.InvoiceLineItem{
			{Description: "Unused time", AmountCents: credit, Quantity: 1},
			{Description: "Remaining time", AmountCents: charge, Quantity: 1},
		}
		_, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", net, 0, lines)
		return lines, err
	}

	// Net 100 taxed 2e16: the credit (-900) takes 2e16 × -900/100 = -1.8e17,
	// the charge the rest, 2e17. The product 2e16 × 900 is past int64.
	lines, err := apply(-900, 1000, 20_000_000_000_000_000)
	if err != nil {
		t.Fatalf("ApplyTaxToLineItems: %v", err)
	}
	if lines[0].TaxAmountCents != -180_000_000_000_000_000 || lines[1].TaxAmountCents != 200_000_000_000_000_000 {
		t.Errorf("credit/charge tax = %d/%d, want -180000000000000000/200000000000000000", lines[0].TaxAmountCents, lines[1].TaxAmountCents)
	}
	if lines[0].TotalAmountCents != -180_000_000_000_000_900 || lines[1].TotalAmountCents != 200_000_000_000_001_000 {
		t.Errorf("credit/charge totals = %d/%d", lines[0].TotalAmountCents, lines[1].TotalAmountCents)
	}

	// Net 1 taxed 1e17: the credit's share, 1e17 × -1000, is past int64.
	if _, err := apply(-1000, 1001, 100_000_000_000_000_000); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("share past int64: err = %v, want ErrAmountOverflow", err)
	}
}
