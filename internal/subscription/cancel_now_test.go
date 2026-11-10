package subscription

import (
	"context"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// cancelNow cancels a subscription at clock.Now(ctx) with no bill, for tests
// that need a canceled row: it reads the row and runs the store's cancel
// against that snapshot, as Service.Cancel does after it plans. A canceled
// row answers "already terminated".
func cancelNow(ctx context.Context, store *PostgresStore, tenantID, id string) (domain.Subscription, error) {
	sub, err := store.Get(ctx, tenantID, id)
	if err != nil {
		return domain.Subscription{}, err
	}
	return store.CancelAtomicWithBill(ctx, tenantID, id, SnapshotOf(sub), clock.Now(ctx), nil)
}
