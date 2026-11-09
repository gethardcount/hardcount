package webhook_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Postgres refuses text that is not valid UTF-8, or that holds a NUL byte
// (SQLSTATE 22021). These tests store text that a cut at a byte index would
// split inside a character, and a receiver's answer that is not text at all.

// answeringClient answers every POST with status and body, or fails it with
// err, and counts the POSTs. The retry worker calls it from goroutines.
type answeringClient struct {
	status int
	body   string
	err    error
	posts  atomic.Int32
}

func (c *answeringClient) Do(*http.Request) (*http.Response, error) {
	c.posts.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return &http.Response{StatusCode: c.status, Body: io.NopCloser(strings.NewReader(c.body))}, nil
}

// storedDelivery is the one delivery row of a tenant, as stored.
type storedDelivery struct {
	status       domain.DeliveryStatus
	attempts     int
	responseBody string
	errorMessage string
}

// sendOneEvent creates an endpoint and sends one invoice.paid event to it
// through client, the way the outbox dispatcher does.
func sendOneEvent(t *testing.T, name string, client webhook.HTTPClient) (context.Context, *postgres.DB, string, *webhook.Service) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleWebhookDelivery), 30*time.Second)
	t.Cleanup(cancel)
	tenantID := testutil.CreateTestTenant(t, db, name)
	svc := webhook.NewTestService(webhook.NewPostgresStore(db), client)
	if _, err := svc.CreateEndpoint(ctx, tenantID, webhook.CreateEndpointInput{
		URL: "http://localhost:9999/hook", Events: []string{"invoice.paid"},
	}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	if err := svc.DispatchFromOutbox(ctx, "", tenantID, "invoice.paid", map[string]any{"invoice_id": "hc_inv_1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return ctx, db, tenantID, svc
}

func readOnlyDelivery(t *testing.T, ctx context.Context, db *postgres.DB, tenantID string) storedDelivery {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var d storedDelivery
	if err := tx.QueryRowContext(ctx, `
		SELECT status, attempt_count, COALESCE(response_body, ''), COALESCE(error_message, '')
		FROM webhook_deliveries`).Scan(&d.status, &d.attempts, &d.responseBody, &d.errorMessage); err != nil {
		t.Fatalf("read the delivery: %v", err)
	}
	return d
}

// endLease moves a pending delivery's lease into the past, as waiting it out
// would. A delivery that is no longer pending is left alone.
func endLease(t *testing.T, ctx context.Context, db *postgres.DB, tenantID string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE webhook_deliveries SET next_retry_at = now() - interval '1 minute'
		WHERE status = 'pending'`); err != nil {
		t.Fatalf("end the lease: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestDelivery_2xxAnswerThatIsNotCleanText_IsMarkedDeliveredOnce: the
// receiver answers 200. Hardcount keeps the start of the answer as text in
// response_body. When that text could not be stored, the success mark failed,
// the delivery stayed pending, and the retry worker sent the event again
// after the lease: a duplicate webhook.
func TestDelivery_2xxAnswerThatIsNotCleanText_IsMarkedDeliveredOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			// The 1 KB cut lands inside "é" (bytes 1023 and 1024): the
			// stored text ends before it.
			name: "a character across the 1 KB cut",
			body: strings.Repeat("a", 1023) + "é and more",
			want: strings.Repeat("a", 1023),
		},
		{
			// A receiver that answers with bytes that are not UTF-8 text,
			// here the start of a PNG. Each run of invalid bytes, and each
			// NUL, is stored as U+FFFD.
			name: "an answer that is not text",
			body: "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR",
			want: "\uFFFDPNG\r\n\x1a\n\uFFFD\uFFFD\uFFFD\rIHDR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &answeringClient{status: http.StatusOK, body: tc.body}
			ctx, db, tenantID, svc := sendOneEvent(t, "Webhook answer "+tc.name, client)

			got := readOnlyDelivery(t, ctx, db, tenantID)
			if got.status != domain.DeliverySucceeded || got.attempts != 1 {
				t.Errorf("delivery status/attempts = %s/%d, want succeeded/1: the success mark was not saved", got.status, got.attempts)
			}
			if got.responseBody != tc.want {
				t.Errorf("response_body = %q, want %q", got.responseBody, tc.want)
			}

			endLease(t, ctx, db, tenantID)
			if err := svc.RetryPendingDeliveries(ctx); err != nil {
				t.Fatalf("retry pass: %v", err)
			}
			if n := client.posts.Load(); n != 1 {
				t.Errorf("POSTs = %d, want 1: the receiver got the event again", n)
			}
		})
	}
}

// TestDelivery_FailedAttemptWithLongError_IsRecorded: an attempt fails with
// an error whose 500-byte cut lands inside a character. The attempt is
// counted and its error stored, so the retry ladder moves on. When the error
// could not be stored, the attempt was not counted.
func TestDelivery_FailedAttemptWithLongError_IsRecorded(t *testing.T) {
	kept := strings.Repeat("a", 499)
	client := &answeringClient{err: errors.New(kept + "é and more")}
	ctx, db, tenantID, _ := sendOneEvent(t, "Webhook long error", client)

	got := readOnlyDelivery(t, ctx, db, tenantID)
	if got.status != domain.DeliveryPending || got.attempts != 1 {
		t.Fatalf("delivery status/attempts = %s/%d, want pending/1: the failed attempt was not recorded", got.status, got.attempts)
	}
	if got.errorMessage != kept {
		t.Errorf("error_message = %q, want the first 499 bytes, which end before the split character", got.errorMessage)
	}
}

// TestOutbox_HandlerErrorCutInsideACharacter_SchedulesTheRetry: the outbox
// handler fails with an error whose 500-byte cut lands inside a character.
// The retry mark saves it and sets next_attempt_at to the first backoff, a
// few seconds away. When the error could not be stored, the mark failed and
// the row waited out its claim lease, more than a minute.
func TestOutbox_HandlerErrorCutInsideACharacter_SchedulesTheRetry(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleWebhookOutbox), 15*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Outbox long error")
	store := webhook.NewOutboxStore(db)

	id, err := store.EnqueueCommitted(ctx, tenantID, "evt.flaky", map[string]any{})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	kept := strings.Repeat("a", 499)
	start := time.Now().UTC()
	if _, err := store.ProcessBatch(ctx, 10, func(context.Context, webhook.OutboxRow) error {
		return errors.New(kept + "é and more")
	}); err != nil {
		t.Fatalf("process batch: %v", err)
	}

	lastErr, nextAt := readOutboxRetry(t, db, id)
	if lastErr != kept {
		t.Errorf("last_error = %q, want the first 499 bytes, which end before the split character", lastErr)
	}
	if !nextAt.Before(start.Add(time.Minute)) {
		t.Errorf("next_attempt_at = %v, want the first backoff after %v, not the claim lease: the retry mark was not saved", nextAt, start)
	}
}
