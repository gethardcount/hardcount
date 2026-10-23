package domain

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

// An invoice line has one quantity and one unit price on the wire (B5):
// `quantity` is the exact decimal string, and `unit_amount_decimal` the unit
// price in decimal cents. The int/decimal duplicates (`quantity_decimal`,
// `unit_amount_cents`) are gone. The values are what the dashboard and the
// hosted page already showed: the exact quantity (the stored decimal when
// set, else the stored integer) and DisplayUnitAmountDecimal.
//
// Mutation-verify: emit the integer quantity in MarshalJSON → the fractional
// case goes red; keep the quantity_decimal or unit_amount_cents tag → the
// absent-field checks go red.
func TestInvoiceLineItem_WireHasOneQuantityAndOneUnitPrice(t *testing.T) {
	nominal := decimal.RequireFromString("0.0003")
	cases := []struct {
		name     string
		line     InvoiceLineItem
		wantQty  string
		wantUnit string
	}{
		{"fractional usage", InvoiceLineItem{LineType: LineTypeUsage, Quantity: 1, QuantityDecimal: decimal.RequireFromString("1.5"), UnitAmountCents: 667, AmountCents: 1000}, "1.5", "666.666666666667"},
		{"whole usage with a nominal rate", InvoiceLineItem{LineType: LineTypeUsage, Quantity: 6000, QuantityDecimal: decimal.NewFromInt(6000), AmountCents: 2, NominalUnitAmountDecimal: &nominal}, "6000", "0.0003"},
		{"base fee (no stored decimal)", InvoiceLineItem{LineType: LineTypeBaseFee, Quantity: 3, UnitAmountCents: 1000, AmountCents: 3000}, "3", "1000"},
		{"zero usage", InvoiceLineItem{LineType: LineTypeUsage, AmountCents: 0}, "0", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.line)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got["quantity"] != tc.wantQty {
				t.Errorf("quantity = %#v, want the string %q; json %s", got["quantity"], tc.wantQty, raw)
			}
			if got["unit_amount_decimal"] != tc.wantUnit {
				t.Errorf("unit_amount_decimal = %#v, want %q; json %s", got["unit_amount_decimal"], tc.wantUnit, raw)
			}
			for _, gone := range []string{"quantity_decimal", "unit_amount_cents"} {
				if _, ok := got[gone]; ok {
					t.Errorf("%s is still on the wire: %s", gone, raw)
				}
			}
			for _, kept := range []string{"amount_cents", "total_amount_cents", "tax_amount_cents"} {
				if _, ok := got[kept]; !ok {
					t.Errorf("%s is missing from the wire: %s", kept, raw)
				}
			}
		})
	}
}
