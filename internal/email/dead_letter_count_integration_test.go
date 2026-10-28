package email_test

import (
	"context"
	"fmt"
	"net/textproto"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// deadLettered reads hardcount_email_outbox_dead_lettered_total for one cause in
// test mode; 0 when the series does not exist.
func deadLettered(t *testing.T, cause string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "hardcount_email_outbox_dead_lettered_total" {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			want := map[string]string{"cause": cause, "mode": "test"}
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

// TestDeadLetter_CountedOnlyWhenTheMarkApplied (G6): the dead-letter counter
// is what pages when an email is given up on, so it counts each row that
// reached 'failed' once, and only when this processor's mark moved it there.
//
// The real trigger is the relay refusing the sender: AWS SES answers "554
// Message rejected: Email address is not verified." 554 is outside the
// 550-553 bounce range, so it is retried and dead-letters after the last
// attempt (max_attempts). A suppressed recipient dead-letters at once
// (permanent). A mark that matches no row — another processor already
// resolved it after this one's lease ran out — moved nothing and must not
// count, nor log the row as moved to the dead-letter queue.
func TestDeadLetter_CountedOnlyWhenTheMarkApplied(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx := leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Dead Letter Count")
	store := email.NewOutboxStore(db)

	enqueue := func(attempts int) string {
		t.Helper()
		id, err := store.EnqueueStandalone(ctx, tenantID, email.TypeInvoice, map[string]any{"to": "a@b.test"})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if _, err := admin.ExecContext(ctx, `UPDATE email_outbox SET attempts = $2 WHERE id = $1`, id, attempts); err != nil {
			t.Fatalf("set attempts: %v", err)
		}
		return id
	}
	process := func(handler email.OutboxHandler) {
		t.Helper()
		if _, err := store.ProcessBatch(ctx, 5, handler); err != nil {
			t.Fatalf("process batch: %v", err)
		}
	}
	// What net/smtp returns for the relay's reply.
	rejected := fmt.Errorf("smtp send: %w", &textproto.Error{Code: 554, Msg: "Message rejected: Email address is not verified."})
	if email.IsPermanentSendError(rejected) {
		t.Fatal("554 classified permanent: this test needs the retried path")
	}

	t.Run("last attempt refused by the relay counts once as max_attempts", func(t *testing.T) {
		before := deadLettered(t, "max_attempts")
		id := enqueue(email.MaxOutboxAttempts - 1) // the claim makes it the last attempt
		process(func(context.Context, email.OutboxRow) error { return rejected })
		if status, _ := emailRowState(t, db, id); status != "failed" {
			t.Fatalf("status = %q, want failed", status)
		}
		if got := deadLettered(t, "max_attempts") - before; got != 1 {
			t.Errorf("max_attempts dead letters counted: %v, want 1", got)
		}
	})

	t.Run("a suppressed recipient counts once as permanent", func(t *testing.T) {
		before := deadLettered(t, "permanent")
		id := enqueue(0)
		process(func(context.Context, email.OutboxRow) error {
			return fmt.Errorf("wrapped: %w", email.ErrRecipientSuppressed)
		})
		if status, _ := emailRowState(t, db, id); status != "failed" {
			t.Fatalf("status = %q, want failed", status)
		}
		if got := deadLettered(t, "permanent") - before; got != 1 {
			t.Errorf("permanent dead letters counted: %v, want 1", got)
		}
	})

	t.Run("a mark that matched no row is not counted", func(t *testing.T) {
		beforePermanent, beforeMax := deadLettered(t, "permanent"), deadLettered(t, "max_attempts")
		id := enqueue(email.MaxOutboxAttempts - 1)
		process(func(context.Context, email.OutboxRow) error {
			// Another processor sent it after this one's lease ran out.
			if _, err := admin.ExecContext(ctx, `UPDATE email_outbox SET status = 'dispatched', dispatched_at = now() WHERE id = $1`, id); err != nil {
				t.Fatalf("resolve elsewhere: %v", err)
			}
			return fmt.Errorf("wrapped: %w", email.ErrRecipientSuppressed)
		})
		if status, _ := emailRowState(t, db, id); status != "dispatched" {
			t.Fatalf("status = %q, want dispatched: a stale mark regressed a terminal row", status)
		}
		if got := deadLettered(t, "permanent") - beforePermanent; got != 0 {
			t.Errorf("permanent dead letters counted for a mark that moved nothing: %v, want 0", got)
		}
		if got := deadLettered(t, "max_attempts") - beforeMax; got != 0 {
			t.Errorf("max_attempts dead letters counted for a mark that moved nothing: %v, want 0", got)
		}
	})
}
