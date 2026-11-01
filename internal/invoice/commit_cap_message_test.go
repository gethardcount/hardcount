package invoice

import (
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

// TestBuildLineItem_CommitCapMessageNamesCents: a commit line grants at most
// 100,000,000 cents (1,000,000.00), the credit grant cap. The refusal said
// "cannot exceed 1,000,000" on a field counted in cents.
func TestBuildLineItem_CommitCapMessageNamesCents(t *testing.T) {
	over := int64(100_000_001)
	_, err := buildLineItem(AddLineItemInput{
		Description: "Prepaid commit", Quantity: 1, UnitAmountCents: 100, CommitGrantedCents: &over,
	}, "USD")
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if !strings.Contains(err.Error(), "100000000 (1,000,000.00 in that currency)") {
		t.Errorf("message %q does not name the cap in cents", err.Error())
	}
}
