package billing

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tax"
)

// Every invoice amount is int64 cents. Each priced line fits (ComputeAmountCents
// refuses one that does not), but the lines' sum and the total with tax are
// worked out in ApplyTaxToLineItems, which every invoice with priced usage
// passes through. An invoice whose sum or total does not fit int64 is an
// ErrAmountOverflow there, whatever the tax provider: the close fails and is
// retried, and no invoice is written with a wrapped amount.

func noneEngine() *Engine {
	return &Engine{
		settings: &taxSettings{provider: "none"},
		taxProviders: resolverFunc(func(_ context.Context, _ domain.TenantSettings) (tax.Provider, error) {
			return tax.NewNoneProvider(), nil
		}),
	}
}

func TestApplyTaxToLineItems_LinesTooLargeToSum(t *testing.T) {
	lines := func() []domain.InvoiceLineItem {
		return []domain.InvoiceLineItem{
			{AmountCents: 5_000_000_000_000_000_000, Quantity: 1},
			{AmountCents: 5_000_000_000_000_000_000, Quantity: 1},
		}
	}
	// The caller's sum has already wrapped.
	var wrapped int64 = 5_000_000_000_000_000_000
	wrapped += 5_000_000_000_000_000_000
	for name, e := range map[string]*Engine{
		"none":   noneEngine(),
		"manual": newManualEngine(2000, "VAT", nil),
	} {
		_, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", wrapped, 0, lines())
		if !errors.Is(err, domain.ErrAmountOverflow) {
			t.Errorf("%s: err = %v, want ErrAmountOverflow", name, err)
		}
	}
}

func TestApplyTaxToLineItems_TotalWithTaxTooLarge(t *testing.T) {
	// 9e18 + 20% tax (1.8e18) does not fit int64.
	e := newManualEngine(2000, "VAT", nil)
	lines := []domain.InvoiceLineItem{{AmountCents: 9_000_000_000_000_000_000, Quantity: 1}}
	_, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 9_000_000_000_000_000_000, 0, lines)
	if !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("err = %v, want ErrAmountOverflow", err)
	}

	// Two lines of 4e18: each line's total (4.8e18) fits, the invoice's
	// (9.6e18) does not.
	lines = []domain.InvoiceLineItem{
		{AmountCents: 4_000_000_000_000_000_000, Quantity: 1},
		{AmountCents: 4_000_000_000_000_000_000, Quantity: 1},
	}
	_, err = e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 8_000_000_000_000_000_000, 0, lines)
	if !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("two lines: err = %v, want ErrAmountOverflow", err)
	}

	// A provider's own result is checked the same way.
	stub := &stubProvider{result: &tax.Result{
		Provider: "stub", TotalTaxCents: math.MaxInt64,
		Lines: []tax.ResultLine{{Ref: "line_0", NetAmountCents: 10, TaxAmountCents: math.MaxInt64}},
	}}
	e = &Engine{settings: &taxSettings{provider: "stripe_tax"}, taxProviders: stubResolver(stub)}
	_, err = e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 10, 0,
		[]domain.InvoiceLineItem{{AmountCents: 10, Quantity: 1}})
	if !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("stub provider: err = %v, want ErrAmountOverflow", err)
	}

	// A line's own total is checked even when the invoice's fits: a split
	// group gives its credit line a negative tax and its absorber the rest.
	stub = &stubProvider{result: &tax.Result{
		Provider: "stub", TotalTaxCents: 0,
		Lines: []tax.ResultLine{
			{Ref: "line_0", NetAmountCents: 10, TaxAmountCents: math.MaxInt64},
			{Ref: "line_1", NetAmountCents: 10, TaxAmountCents: -math.MaxInt64},
		},
	}}
	e = &Engine{settings: &taxSettings{provider: "stripe_tax"}, taxProviders: stubResolver(stub)}
	_, err = e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 20, 0,
		[]domain.InvoiceLineItem{{AmountCents: 10, Quantity: 1}, {AmountCents: 10, Quantity: 1}})
	if !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("line total: err = %v, want ErrAmountOverflow", err)
	}
}

func TestApplyTaxToLineItems_LargeAmountTaxedExactly(t *testing.T) {
	// 1e14 cents at 20%: tax 2e13, total 1.2e14.
	e := newManualEngine(2000, "VAT", nil)
	lines := []domain.InvoiceLineItem{{AmountCents: 100_000_000_000_000, Quantity: 1}}
	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 100_000_000_000_000, 0, lines)
	if err != nil {
		t.Fatalf("ApplyTaxToLineItems: %v", err)
	}
	if r.TaxAmountCents != 20_000_000_000_000 || lines[0].TotalAmountCents != 120_000_000_000_000 {
		t.Errorf("tax = %d, line total = %d, want 20000000000000 and 120000000000000", r.TaxAmountCents, lines[0].TotalAmountCents)
	}
}
