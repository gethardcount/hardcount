package invoice_test

import (
	"context"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
)

// The seams the charge-hold tests in this package reach production through.
// The only test file here written against the API's current shape.

// collectCharger counts the charges Collect makes.
type collectCharger struct{ calls int }

func (c *collectCharger) ChargeInvoiceNow(_ context.Context, _ string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	c.calls++
	return inv, nil
}

// listStaleHolds is the stale-hold sweep's read, from the start.
func listStaleHolds(ctx context.Context, store *invoice.PostgresStore, limit int) ([]domain.StaleChargeHold, error) {
	return store.ListStaleChargeHolds(ctx, limit, nil)
}
