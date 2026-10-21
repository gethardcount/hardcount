package invoice

import (
	"testing"

	"github.com/sagarsuperuser/velox/internal/api/generated"
)

// The published InvoiceLineItemInput.line_type enum and the type check the
// write paths run must name the same set. The handler decodes into the
// service struct, so the generated enum enforces nothing on its own: if the
// two drift, the spec promises a type the API refuses, or the API takes one
// the spec does not admit.
func TestLineTypeInputEnumMatchesBuildLineItem(t *testing.T) {
	for _, v := range []string{"base_fee", "usage", "add_on", "discount", "tax", "manual"} {
		_, err := buildLineItem(AddLineItemInput{
			Description: "Line", LineType: v, Quantity: 1, UnitAmountCents: 100,
		}, "USD")
		inSpec := generated.InvoiceLineItemInputLineType(v).Valid()
		if inSpec != (err == nil) {
			t.Errorf("line_type %q: in spec enum = %v, accepted by buildLineItem = %v (err %v)", v, inSpec, err == nil, err)
		}
	}
}
