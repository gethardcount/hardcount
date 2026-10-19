package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// outboxDeadLettered reads velox_webhook_outbox_dead_lettered_total for
// max_attempts in test mode; 0 when the series does not exist.
func outboxDeadLettered(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "velox_webhook_outbox_dead_lettered_total" {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			want := map[string]string{"cause": "max_attempts", "mode": "test"}
			if len(m.GetLabel()) != len(want) {
				continue
			}
			for _, l := range m.GetLabel() {
				if want[l.GetName()] != l.GetValue() {
					continue metrics
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// TestWebhookOutboxDeadLetter_CountedOnlyWhenTheMarkApplied (G6): a webhook
// outbox row that is given up on is an event no endpoint will ever get, so
// the dead-letter counter pages on it. It counts a row once, and only when
// this processor's mark moved it to 'failed'. Before, the mark's row count
// was never read, so a mark that matched nothing — another processor
// dispatched the row after this one's lease ran out — still logged "row
// moved to DLQ".
func TestWebhookOutboxDeadLetter_CountedOnlyWhenTheMarkApplied(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleWebhookOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Webhook Outbox Dead Letter")
	store := NewOutboxStore(db)

	// One row on its last attempt (the claim adds the last one).
	lastAttempt := func() string {
		t.Helper()
		id, err := store.EnqueueStandalone(ctx, tenantID, "invoice.paid", map[string]any{"invoice_id": "inv_1"})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if _, err := admin.ExecContext(ctx, `UPDATE webhook_outbox SET attempts = $2 WHERE id = $1`, id, MaxOutboxAttempts-1); err != nil {
			t.Fatalf("set attempts: %v", err)
		}
		return id
	}
	status := func(id string) string {
		t.Helper()
		var s string
		if err := admin.QueryRowContext(ctx, `SELECT status FROM webhook_outbox WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read row: %v", err)
		}
		return s
	}
	// The handler's own failure: the event and delivery rows could not be
	// written.
	dbDown := errors.New("create event: connection refused")

	before := outboxDeadLettered(t)
	id := lastAttempt()
	if _, err := store.ProcessBatch(ctx, 10, func(context.Context, OutboxRow) error { return dbDown }); err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if s := status(id); s != OutboxFailed {
		t.Fatalf("status = %q, want failed", s)
	}
	if got := outboxDeadLettered(t) - before; got != 1 {
		t.Errorf("dead letters counted: %v, want 1", got)
	}

	before = outboxDeadLettered(t)
	id = lastAttempt()
	if _, err := store.ProcessBatch(ctx, 10, func(context.Context, OutboxRow) error {
		// Another processor dispatched it after this one's lease ran out.
		if _, err := admin.ExecContext(ctx, `UPDATE webhook_outbox SET status = 'dispatched', dispatched_at = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("dispatch elsewhere: %v", err)
		}
		return dbDown
	}); err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if s := status(id); s != OutboxDispatched {
		t.Fatalf("status = %q, want dispatched: a stale mark regressed a terminal row", s)
	}
	if got := outboxDeadLettered(t) - before; got != 0 {
		t.Errorf("dead letters counted for a mark that moved nothing: %v, want 0", got)
	}
}
