package invoice

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

// A top-up's grant runs after its payment is taken. Its error must never read
// as ErrInvalidState, which the settle records as a payment on a voided
// invoice and acknowledges, or as ErrNotFound, which the webhook
// acknowledges: either would keep the money and drop the credit. It must
// fail the settle, so the payment is retried.
func TestTopupGrantError_NeverMatchesInvalidStateOrNotFound(t *testing.T) {
	for _, cause := range []error{
		errs.InvalidState("cannot grant"),
		errs.NotFound("customer"),
		fmt.Errorf("wrapped: %w", errs.ErrNotFound),
	} {
		err := topupGrantError("hc_inv_1", cause)
		if errors.Is(err, errs.ErrInvalidState) || errors.Is(err, errs.ErrNotFound) {
			t.Errorf("grant error %v still matches %v", err, cause)
		}
		if err == nil || err.Error() == "" {
			t.Errorf("grant error for %v lost its text", cause)
		}
	}
}
