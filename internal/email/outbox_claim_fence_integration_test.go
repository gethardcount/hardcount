package email_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// These tests cover two claims of one email_outbox row that overlap.
//
// A claim is one ProcessBatch taking a row: it adds 1 to attempts and leases
// the row by moving next_attempt_at ahead. A handler that outlives its lease
// (a frozen process, a database clock that steps forward) is still at work
// when a second ProcessBatch claims the same row. Below, claim A is the one
// whose lease ran out and claim B is the new one.
//
// Each test ends A's lease from inside A's handler and runs a real second
// ProcessBatch, so Postgres decides which marks apply. No email is sent: the
// handlers return what a send would.

// emailClaimFence is what every test here starts from: one tenant and its
// outbox.
type emailClaimFence struct {
	ctx      context.Context
	admin    *sql.DB
	tenantID string
	store    *email.OutboxStore
}

func newEmailClaimFence(t *testing.T, tenantName string) *emailClaimFence {
	t.Helper()
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	ctx, cancel := context.WithTimeout(
		leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox),
		60*time.Second)
	t.Cleanup(cancel)
	return &emailClaimFence{
		ctx:      ctx,
		admin:    admin,
		tenantID: testutil.CreateTestTenant(t, db, tenantName),
		store:    email.NewOutboxStore(db),
	}
}

// enqueue queues one invoice email that has already been claimed attempts
// times. Its payload holds a link token, which the sent mark removes.
func (f *emailClaimFence) enqueue(t *testing.T, attempts int) string {
	t.Helper()
	id, err := f.store.EnqueueStandalone(f.ctx, f.tenantID, email.TypeInvoice, map[string]any{
		"to": "customer@example.com", "invoice_number": "HC-000042", "public_token": "hc_pinv_rawpublictoken",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if attempts == 0 {
		return id
	}
	// Raw SQL: production reaches a row's last attempt only through 14
	// failed ones, about two days of waiting.
	if _, err := f.admin.ExecContext(f.ctx, `UPDATE email_outbox SET attempts = $2 WHERE id = $1`, id, attempts); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
	return id
}

// endLease makes the row due again, as the end of a claim's lease does. Raw
// SQL: the lease itself ends only minutes later.
func (f *emailClaimFence) endLease(t *testing.T, id string) {
	t.Helper()
	if _, err := f.admin.ExecContext(f.ctx, `UPDATE email_outbox SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("end the lease: %v", err)
	}
}

// claimAgain is claim B. Call it from inside claim A's handler. It ends the
// row's lease and starts a second ProcessBatch in a goroutine, then returns
// once that has claimed the row, with the row as B's claim returned it. B's
// handler waits there: finish makes it return result, and returns when B's
// ProcessBatch has returned.
func (f *emailClaimFence) claimAgain(t *testing.T, id string, result error) (claimed email.OutboxRow, finish func()) {
	t.Helper()
	f.endLease(t, id)

	claimedCh := make(chan email.OutboxRow, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.store.ProcessBatch(f.ctx, 5, func(_ context.Context, row email.OutboxRow) error {
			claimedCh <- row
			<-release
			return result
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

// emailRowNow is one email_outbox row as stored.
type emailRowNow struct {
	status        string
	attempts      int
	nextAttemptAt time.Time
	lastError     sql.NullString
	dispatchedAt  sql.NullTime
	skipReason    sql.NullString
	payload       map[string]any
}

func (f *emailClaimFence) row(t *testing.T, id string) emailRowNow {
	t.Helper()
	var r emailRowNow
	var payload []byte
	if err := f.admin.QueryRowContext(f.ctx, `
		SELECT status, attempts, next_attempt_at, last_error, dispatched_at, skip_reason, payload
		FROM email_outbox WHERE id = $1
	`, id).Scan(&r.status, &r.attempts, &r.nextAttemptAt, &r.lastError, &r.dispatchedAt, &r.skipReason, &payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if err := json.Unmarshal(payload, &r.payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return r
}

// wantSent fails the test unless the row reads as a sent email: dispatched,
// with its send time, no error, no skip reason, and its link token removed.
func (r emailRowNow) wantSent(t *testing.T) {
	t.Helper()
	if r.status != email.OutboxDispatched {
		t.Errorf("status = %s, want dispatched: the email was sent", r.status)
	}
	if !r.dispatchedAt.Valid {
		t.Error("dispatched_at is not set on a sent email")
	}
	if r.lastError.Valid {
		t.Errorf("last_error = %q, want none on a sent email", r.lastError.String)
	}
	if r.skipReason.Valid {
		t.Errorf("skip_reason = %q, want none on a sent email", r.skipReason.String)
	}
	if _, kept := r.payload["public_token"]; kept {
		t.Error("the link token is still stored on a sent email")
	}
}

// waitForDatabaseClock returns once the database clock has passed at. A claim
// takes rows that are due on that clock, and a test cannot move it.
func (f *emailClaimFence) waitForDatabaseClock(t *testing.T, at time.Time) {
	t.Helper()
	if _, err := f.admin.ExecContext(f.ctx,
		`SELECT pg_sleep(GREATEST(0, EXTRACT(EPOCH FROM $1::timestamptz - clock_timestamp())) + 0.05)`, at); err != nil {
		t.Fatalf("wait for the database clock: %v", err)
	}
}

// errRelayBusy is a send failure that is retried.
var errRelayBusy = errors.New("smtp: 451 try again later")

// TestEmailOutbox_StaleRetryMarkLeavesTheNewClaimAlone: claim A fails after
// claim B took the row, and A's retry mark runs while B is still sending. The
// mark must not touch the row. If it applied, it would move next_attempt_at
// from the end of B's lease to one second from now, and save A's error on B's
// attempt. A third claim would then take the row under B and send the email
// a third time.
func TestEmailOutbox_StaleRetryMarkLeavesTheNewClaimAlone(t *testing.T) {
	f := newEmailClaimFence(t, "Email Claim Fence Retry")
	id := f.enqueue(t, 0)

	var claimB email.OutboxRow
	var finishB func()
	if _, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error {
		claimB, finishB = f.claimAgain(t, id, nil)
		return errRelayBusy
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}
	// One second is the wait after a first failed attempt: an applied retry
	// mark from A makes the row due no later than this.
	dueIfMarked := time.Now().Add(time.Second)

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

	// While B is still sending, no third claim may take the row.
	f.waitForDatabaseClock(t, dueIfMarked)
	n, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error { return nil })
	if err != nil {
		t.Fatalf("third process batch: %v", err)
	}
	if n != 0 {
		t.Errorf("a third claim took %d row while claim B was still sending, want 0", n)
	}

	finishB()
	f.row(t, id).wantSent(t)
}

// TestEmailOutbox_StaleDLQMarkDoesNotFailTheNewClaim: claim A ends in an
// error that is never retried (a suppressed recipient) after claim B took the
// row. A's give-up mark runs first, then B sends. The row must read sent,
// and no dead letter may be counted.
//
// If A's mark applied, the row would read failed for a delivered email, and
// the runbook's requeue of failed rows would send the email again.
func TestEmailOutbox_StaleDLQMarkDoesNotFailTheNewClaim(t *testing.T) {
	f := newEmailClaimFence(t, "Email Claim Fence Give-up")
	id := f.enqueue(t, 0)
	before := deadLettered(t, "permanent")

	var finishB func()
	if _, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error {
		_, finishB = f.claimAgain(t, id, nil)
		return fmt.Errorf("check recipient: %w", email.ErrRecipientSuppressed)
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}
	// A's give-up mark has run. Now B's send succeeds.
	finishB()

	f.row(t, id).wantSent(t)
	if got := deadLettered(t, "permanent") - before; got != 0 {
		t.Errorf("dead letters counted for an email that was sent: %v, want 0", got)
	}
}

// TestEmailOutbox_LateSendOutranksANewClaimsGiveUp: claim A was the last
// attempt (15) and its send succeeds, but only after claim B (16) failed and
// gave the row up. The row must read sent: the email reached the relay. So
// the sent mark applies to a failed row too. Left failed, the row would be
// sent again by the runbook's requeue of failed rows.
func TestEmailOutbox_LateSendOutranksANewClaimsGiveUp(t *testing.T) {
	f := newEmailClaimFence(t, "Email Claim Fence Late Send")
	id := f.enqueue(t, email.MaxOutboxAttempts-1) // claim A makes it the last attempt

	if _, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error {
		// A's lease ends here. B claims the row, fails and gives it up.
		f.endLease(t, id)
		n, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error { return errRelayBusy })
		if err != nil || n != 1 {
			t.Fatalf("claim B: attempted %d rows, err %v; want 1 row", n, err)
		}
		if got := f.row(t, id).status; got != email.OutboxFailed {
			t.Fatalf("status after claim B gave up = %s, want failed", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}

	f.row(t, id).wantSent(t)
}

// TestEmailOutbox_StaleSkipDoesNotHideANewClaimsSend: claim A finds the
// invoice paid and skips the email after claim B took the row. A's skip mark
// runs first, then B's send succeeds: B checked the invoice before it was
// paid. The row must read sent, with no skip reason. So the sent mark applies
// to a skipped row too. Left skipped, the Sent emails card would say
// "skipped" for a delivered email.
func TestEmailOutbox_StaleSkipDoesNotHideANewClaimsSend(t *testing.T) {
	f := newEmailClaimFence(t, "Email Claim Fence Skip")
	id := f.enqueue(t, 0)

	var finishB func()
	if _, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error {
		_, finishB = f.claimAgain(t, id, nil)
		return &email.ObsoleteError{InvoiceNumber: "HC-000042", State: email.SkipReasonPaid}
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}
	// A's skip mark has run: a skip is a decision every claim may record.
	if row := f.row(t, id); row.status != "skipped" || row.skipReason.String != email.SkipReasonPaid {
		t.Fatalf("row after claim A's skip = %s with skip_reason %q, want skipped with paid", row.status, row.skipReason.String)
	}
	// Now B's send succeeds.
	finishB()

	f.row(t, id).wantSent(t)
}

// TestEmailOutbox_LateSuccessFromExpiredClaimIsKept: claim A's send succeeds
// after claim B took the row, and B then fails. The row must read sent and
// must not be claimed again. A's sent mark is not tied to its claim: dropping
// it would leave the row pending, and the email would be sent again.
func TestEmailOutbox_LateSuccessFromExpiredClaimIsKept(t *testing.T) {
	f := newEmailClaimFence(t, "Email Claim Fence Late Success")
	id := f.enqueue(t, 0)

	var finishB func()
	if _, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error {
		_, finishB = f.claimAgain(t, id, errRelayBusy)
		return nil
	}); err != nil {
		t.Fatalf("claim A: process batch: %v", err)
	}
	// A's sent mark has run. Now B fails.
	finishB()

	f.row(t, id).wantSent(t)
	f.endLease(t, id)
	n, err := f.store.ProcessBatch(f.ctx, 5, func(context.Context, email.OutboxRow) error { return nil })
	if err != nil {
		t.Fatalf("next process batch: %v", err)
	}
	if n != 0 {
		t.Errorf("the next batch claimed %d row of a sent email, want 0", n)
	}
}
