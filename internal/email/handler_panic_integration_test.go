package email

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// panicOnceDeliverer panics on its first receipt send, standing in for a
// nil dereference in a send path, and records every later send.
type panicOnceDeliverer struct {
	recordingDeliverer
	panicked bool
}

func (p *panicOnceDeliverer) SendPaymentReceipt(ctx context.Context, tenantID, to string, cc []string, customerName, invoiceNumber string, amountCents int64, currency, publicToken string) error {
	if !p.panicked {
		p.panicked = true
		panic("receipt: nil dereference")
	}
	return p.recordingDeliverer.SendPaymentReceipt(ctx, tenantID, to, cc, customerName, invoiceNumber, amountCents, currency, publicToken)
}

// captureLogs sends slog's default logger to a buffer for the rest of the
// test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// TestEmailOutbox_HandlerPanicIsRetried: a send that panics is one failed
// attempt on the retry ramp, not a dead letter, and the panic is logged at
// ERROR with its stack. The email is sent on the next attempt.
//
// Mutation-verify: wrap the recovered panic in ErrPayloadDecode again. The
// row is marked failed on its first attempt, and this test fails.
func TestEmailOutbox_HandlerPanicIsRetried(t *testing.T) {
	logs := captureLogs(t)
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Panic Retried")
	store := NewOutboxStore(db)

	id, err := store.EnqueueStandalone(ctx, tenantID, TypePaymentReceipt, map[string]any{"to": "c@example.com", "invoice_number": "INV-1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	sender := &panicOnceDeliverer{}
	d := NewDispatcher(store, sender, DispatcherConfig{}, leader.AlwaysLead{})

	if n, err := store.ProcessBatch(ctx, 5, d.handle); err != nil || n != 1 {
		t.Fatalf("first batch: n=%d err=%v", n, err)
	}
	status, attempts, lastErr, future := skewRowState(t, db, id)
	if status != OutboxPending {
		t.Fatalf("after the panic: status %q, want pending (a panic is one failed attempt, not a dead letter)", status)
	}
	if attempts != 1 || !future || !strings.Contains(lastErr, "handler panic: receipt: nil dereference") {
		t.Fatalf("after the panic: attempts=%d retry_in_future=%v last_error=%q, want 1/true/the panic", attempts, future, lastErr)
	}
	out := logs.String()
	if !strings.Contains(out, `level=ERROR msg="email outbox: handler panicked; counting a failed attempt"`) ||
		!strings.Contains(out, "outbox_id="+id) || !strings.Contains(out, "runtime/debug.Stack") {
		t.Fatalf("the panic was not logged at ERROR with its row and stack:\n%s", out)
	}

	skewMakeDue(t, db, id)
	if n, err := store.ProcessBatch(ctx, 5, d.handle); err != nil || n != 1 {
		t.Fatalf("second batch: n=%d err=%v", n, err)
	}
	if status, attempts, _, _ := skewRowState(t, db, id); status != OutboxDispatched || attempts != 2 {
		t.Fatalf("after the second attempt: status %q attempts %d, want dispatched/2", status, attempts)
	}
	if !slices.Equal(sender.sent, []string{TypePaymentReceipt}) {
		t.Fatalf("sends recorded: %v, want one receipt", sender.sent)
	}
}

// TestEmailOutbox_UndecodablePayloadFailsAtOnce: a payload the dispatcher
// cannot decode fails the same way on every attempt, so the row is
// dead-lettered on its first attempt and nothing is sent.
func TestEmailOutbox_UndecodablePayloadFailsAtOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Undecodable Payload")
	store := NewOutboxStore(db)

	// No "to": decodeMessage refuses the payload.
	id, err := store.EnqueueStandalone(ctx, tenantID, TypePaymentReceipt, map[string]any{"invoice_number": "INV-2"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	sender := &recordingDeliverer{}
	d := NewDispatcher(store, sender, DispatcherConfig{}, leader.AlwaysLead{})

	if n, err := store.ProcessBatch(ctx, 5, d.handle); err != nil || n != 1 {
		t.Fatalf("batch: n=%d err=%v", n, err)
	}
	status, attempts, lastErr, _ := skewRowState(t, db, id)
	if status != OutboxFailed || attempts != 1 || !strings.Contains(lastErr, "payload decode failed") {
		t.Fatalf("undecodable payload: status %q attempts %d last_error %q, want failed/1/payload decode failed", status, attempts, lastErr)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sends recorded: %v, want none", sender.sent)
	}
}
