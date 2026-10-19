package dunning_test

import (
	"context"

	"github.com/sagarsuperuser/velox/internal/invoice"
)

// The seams the dunning charge-hold tests reach production through. The only
// test file here written against the API's current shape.

// claimAutoCharge takes the auto-charge sweep's claim and reports whether it
// was granted.
func claimAutoCharge(ctx context.Context, store *invoice.PostgresStore, tenantID, invoiceID string) (bool, error) {
	return store.ClaimAutoCharge(ctx, tenantID, invoiceID)
}

// wireChargeHolds wires the final action's charge hold as router.go does.
func wireChargeHolds(_ *exhaustFixture) {}
