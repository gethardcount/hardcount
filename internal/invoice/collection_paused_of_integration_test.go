package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// CollectionPausedOf reads the claim's own pause predicate: false with no
// pause, true while collection is paused, false again once the pause's
// resumes_at has passed. Mutation: drop the NOT, and every answer flips.
func TestCollectionPausedOf_ReadsTheClaimsPause(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Pause Of")
	store := invoice.NewPostgresStore(db)
	inv, subID, _ := seedQueuedSubInvoice(t, db, ctx, tenantID, "INV-PAUSEOF-1")
	check := func(when string, want bool) {
		t.Helper()
		if got, err := store.CollectionPausedOf(ctx, tenantID, inv.ID); err != nil || got != want {
			t.Errorf("%s: paused = %v (err %v), want %v", when, got, err, want)
		}
	}

	check("no pause", false)
	if _, err := subscription.NewPostgresStore(db).SetPauseCollection(ctx, tenantID, subID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
		t.Fatalf("pause: %v", err)
	}
	check("paused", true)
	bypassExec(t, db, `UPDATE subscriptions SET pause_collection_resumes_at = $1 WHERE id = $2`, time.Now().UTC().Add(-time.Hour), subID)
	check("after the pause's resumes_at", false)
}
