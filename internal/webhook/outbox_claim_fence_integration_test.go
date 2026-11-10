package webhook

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// These tests cover two claims of one webhook_outbox row that overlap.
//
// A claim is one ProcessBatch taking a row: it adds 1 to attempts and leases
// the row by moving next_attempt_at ahead. A handler that outlives its lease
// (a frozen process, a database clock that steps forward) is still at work
// when a second ProcessBatch claims the same row. Below, claim A is the one
// whose lease ran out and claim B is the new one.
//
// Each test ends A's lease from inside A's handler and runs a real second
// ProcessBatch, so Postgres decides which marks apply.

// claimFence is what every test here starts from: a tenant with one endpoint
// that takes every event, and the production handler to dispatch with.
type claimFence struct {
	ctx      context.Context
	admin    *sql.DB
	tenantID string
	outbox   *OutboxStore
	// dispatch is Dispatcher.handle, the handler production runs.
	dispatch OutboxHandler
	// client answers 200 and keeps each POST, one per event sent.
	client *recordingClient
}

func newClaimFence(t *testing.T, tenantName string) *claimFence {
	t.Helper()
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx, cancel := context.WithTimeout(
		leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleWebhookOutbox, leader.RoleWebhookDelivery),
		60*time.Second)
	t.Cleanup(cancel)
	tenantID := testutil.CreateTestTenant(t, db, tenantName)
	seedEndpoint(t, db, ctx, tenantID)

	outbox := NewOutboxStore(db)
	client := &recordingClient{statuses: []int{200}}
	d := NewDispatcher(outbox, NewTestService(NewPostgresStore(db), client), DispatcherConfig{}, leader.AlwaysLead{})
	return &claimFence{ctx: ctx, admin: admin, tenantID: tenantID, outbox: outbox, dispatch: d.handle, client: client}
}

// enqueue queues one invoice.paid row that has already been claimed attempts
// times.
func (f *claimFence) enqueue(t *testing.T, attempts int) string {
	t.Helper()
	id, err := f.outbox.EnqueueCommitted(f.ctx, f.tenantID, "invoice.paid", map[string]any{"invoice_id": "inv_1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if attempts == 0 {
		return id
	}
	// Raw SQL: production reaches a row's last attempt only through 14
	// failed ones, about two days of waiting.
	if _, err := f.admin.ExecContext(f.ctx, `UPDATE webhook_outbox SET attempts = $2 WHERE id = $1`, id, attempts); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
	return id
}

// endLease makes the row due again, as the end of a claim's lease does. Raw
// SQL: the lease itself ends only minutes later.
func (f *claimFence) endLease(t *testing.T, id string) {
	t.Helper()
	if _, err := f.admin.ExecContext(f.ctx, `UPDATE webhook_outbox SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("end the lease: %v", err)
	}
}

// claimAgain is claim B. Call it from inside claim A's handler. It ends the
// row's lease and starts a second ProcessBatch in a goroutine, then returns
// once that has claimed the row, with the row as B's claim returned it. B's
// handler waits there: finish lets it run handler, and returns when B's
// ProcessBatch has returned.
func (f *claimFence) claimAgain(t *testing.T, id string, handler OutboxHandler) (claimed OutboxRow, finish func()) {
	t.Helper()
	f.endLease(t, id)

	claimedCh := make(chan OutboxRow, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.outbox.ProcessBatch(f.ctx, 10, func(ctx context.Context, row OutboxRow) error {
			claimedCh <- row
			<-release
			return handler(ctx, row)
		})
		done <- err
	}()

	var once sync.Once
	finish = func() {
		t.Helper()
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Errorf("claim B: process batch: %v", err)
			}
		})
	}
	select {
	case claimed = <-claimedCh:
	case err := <-done:
		once.Do(func() {}) // B has already returned: nothing to release.
		t.Fatalf("claim B took no row (process batch: %v)", err)
	}
	t.Cleanup(finish)
	return claimed, finish
}

// outboxRowNow is one webhook_outbox row as stored.
type outboxRowNow struct {
	status        string
	attempts      int
	nextAttemptAt time.Time
	lastError     sql.NullString
}

func (f *claimFence) row(t *testing.T, id string) outboxRowNow {
	t.Helper()
	var r outboxRowNow
	if err := f.admin.QueryRowContext(f.ctx,
		`SELECT status, attempts, next_attempt_at, last_error FROM webhook_outbox WHERE id = $1`, id,
	).Scan(&r.status, &r.attempts, &r.nextAttemptAt, &r.lastError); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	return r
}

// stored counts the tenant's webhook events and delivery rows.
func (f *claimFence) stored(t *testing.T) (events, deliveries int) {
	t.Helper()
	if err := f.admin.QueryRowContext(f.ctx, `
		SELECT (SELECT count(*) FROM webhook_events WHERE tenant_id = $1),
		       (SELECT count(*) FROM webhook_deliveries WHERE tenant_id = $1)
	`, f.tenantID).Scan(&events, &deliveries); err != nil {
		t.Fatalf("count events and deliveries: %v", err)
	}
	return events, deliveries
}

// errEventNotWritten is a handler failure that is retried: the event and its
// delivery rows could not be written.
var errEventNotWritten = errors.New("create event: connection refused")

// TestWebhookOutbox_ExpiredClaimWritesNoSecondEvent: one outbox row makes one
// event. Claim B dispatches the row while claim A is still handling it, then
// A dispatches too. A's event must not commit: it would carry a new id, and a
// receiver drops a repeat by its id, so it would take the same event twice.
// A's dispatched mark matches no row, and that is what rolls A's event back.
func TestWebhookOutbox_ExpiredClaimWritesNoSecondEvent(t *testing.T) {
	f := newClaimFence(t, "Outbox Claim Fence Event")
	id := f.enqueue(t, 0)

	var errA error
	if _, err := f.outbox.ProcessBatch(f.ctx, 10, func(ctx context.Context, row OutboxRow) error {
		// A's lease ends here. B claims the row and dispatches it.
		f.endLease(t, id)
		if n, err := f.outbox.ProcessBatch(f.ctx, 10, f.dispatch); err != nil || n != 1 {
			t.Fatalf("claim B: attempted %d rows, err %v; want 1 row", n, err)
		}
		errA = f.dispatch(ctx, row)
		return errA
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}

	if !errors.Is(errA, ErrOutboxRowNotPending) {
		t.Errorf("claim A's dispatch returned %v, want ErrOutboxRowNotPending", errA)
	}
	events, deliveries := f.stored(t)
	if events != 1 {
		t.Errorf("webhook events for one outbox row = %d, want 1", events)
	}
	if deliveries != 1 {
		t.Errorf("delivery rows for one endpoint = %d, want 1", deliveries)
	}
	if got := len(f.client.bodies); got != 1 {
		t.Errorf("POSTs to the endpoint = %d, want 1", got)
	}
	row := f.row(t, id)
	if row.status != OutboxDispatched || row.attempts != 2 || row.lastError.Valid {
		t.Errorf("row = %s after %d attempts, last_error %q; want dispatched after 2 with no last_error",
			row.status, row.attempts, row.lastError.String)
	}
}

// TestWebhookOutbox_StaleRetryMarkLeavesTheNewClaimAlone: claim A fails after
// claim B took the row, and A's retry mark runs while B is still at work. The
// mark must not touch the row. If it applied, it would move next_attempt_at
// from the end of B's lease to one second from now, so a third claim could
// take the row under B. It would also save A's error on B's attempt.
func TestWebhookOutbox_StaleRetryMarkLeavesTheNewClaimAlone(t *testing.T) {
	f := newClaimFence(t, "Outbox Claim Fence Retry")
	id := f.enqueue(t, 0)

	var claimB OutboxRow
	var finishB func()
	if _, err := f.outbox.ProcessBatch(f.ctx, 10, func(context.Context, OutboxRow) error {
		claimB, finishB = f.claimAgain(t, id, func(context.Context, OutboxRow) error { return nil })
		return errEventNotWritten
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}

	// A's retry mark has run, and B has not marked yet.
	row := f.row(t, id)
	if row.attempts != 2 {
		t.Fatalf("attempts = %d, want 2: claim B did not take the row", row.attempts)
	}
	if !row.nextAttemptAt.Equal(claimB.NextAttemptAt) {
		t.Errorf("next_attempt_at = %s, want %s, the end of claim B's lease",
			row.nextAttemptAt.Format(time.RFC3339Nano), claimB.NextAttemptAt.Format(time.RFC3339Nano))
	}
	if row.lastError.Valid {
		t.Errorf("last_error = %q, want none: claim B has not failed", row.lastError.String)
	}

	finishB()
	if got := f.row(t, id).status; got != OutboxDispatched {
		t.Errorf("status after claim B succeeded = %s, want dispatched", got)
	}
}

// TestWebhookOutbox_StaleDLQMarkDoesNotFailTheNewClaim: claim A was the last
// attempt (15) and fails after claim B (16) took the row. A's give-up mark
// runs first, then B dispatches. The row must end dispatched with its one
// event, and no dead letter may be counted.
//
// If A's mark applied, the row would read failed with its event already
// sent. The runbook's requeue of failed rows would then send the event again
// under a new id.
func TestWebhookOutbox_StaleDLQMarkDoesNotFailTheNewClaim(t *testing.T) {
	f := newClaimFence(t, "Outbox Claim Fence Give-up")
	id := f.enqueue(t, MaxOutboxAttempts-1) // claim A makes it the last attempt
	before := outboxDeadLettered(t)

	var finishB func()
	if _, err := f.outbox.ProcessBatch(f.ctx, 10, func(context.Context, OutboxRow) error {
		_, finishB = f.claimAgain(t, id, f.dispatch)
		return errEventNotWritten
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}
	// A's give-up mark has run. Now B dispatches.
	finishB()

	row := f.row(t, id)
	if row.status != OutboxDispatched || row.attempts != MaxOutboxAttempts+1 {
		t.Errorf("row = %s after %d attempts, want dispatched after %d", row.status, row.attempts, MaxOutboxAttempts+1)
	}
	if events, _ := f.stored(t); events != 1 {
		t.Errorf("webhook events = %d, want 1", events)
	}
	if got := outboxDeadLettered(t) - before; got != 0 {
		t.Errorf("dead letters counted for a row that was dispatched: %v, want 0", got)
	}
}
