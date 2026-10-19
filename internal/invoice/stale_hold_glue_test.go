package invoice_test

import (
	"context"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The seams the charge-hold tests in this package reach production through.
// The only test file here written against the API's current shape.

// collectCharger counts the charges Collect makes.
type collectCharger struct{ calls int }

func (c *collectCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	c.calls++
	return inv, nil
}
