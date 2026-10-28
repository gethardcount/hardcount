package payment

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestWebhookStore_ConcurrentIngestDedup verifies that when Stripe redelivers
// the same event_id to multiple replicas in parallel, the ON CONFLICT DO NOTHING
// RETURNING pattern guarantees exactly one insert wins and every racer sees
// isNew=false for the duplicates — with no errors on any caller.
func TestWebhookStore_ConcurrentIngestDedup(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Webhook Dedup Corp")
	store := NewPostgresWebhookStore(db)

	const racers = 16
	event := domain.StripeWebhookEvent{
		StripeEventID: "evt_concurrent_dedup_test",
		EventType:     "payment_intent.succeeded",
		ObjectType:    "payment_intent",
	}

	var (
		wg       sync.WaitGroup
		newCount atomic.Int32
		dupCount atomic.Int32
		errCount atomic.Int32
		start    = make(chan struct{})
	)

	for range racers {
		wg.Go(func() {
			<-start // release all goroutines together to maximise contention
			_, isNew, err := store.IngestEvent(ctx, tenantID, event)
			switch {
			case err != nil:
				errCount.Add(1)
				t.Errorf("IngestEvent: unexpected error: %v", err)
			case isNew:
				newCount.Add(1)
			default:
				dupCount.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if errCount.Load() != 0 {
		t.Fatalf("unexpected errors: %d", errCount.Load())
	}
	if got := newCount.Load(); got != 1 {
		t.Errorf("isNew=true count: got %d, want 1", got)
	}
	if got := dupCount.Load(); got != racers-1 {
		t.Errorf("isNew=false count: got %d, want %d", got, racers-1)
	}

	// Verify DB state: exactly one row persisted. Read via TxTenant so RLS
	// allows visibility of the tenant's rows.
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)

	var rows int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stripe_webhook_events WHERE stripe_event_id = $1`,
		event.StripeEventID,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("persisted rows: got %d, want 1", rows)
	}
}
