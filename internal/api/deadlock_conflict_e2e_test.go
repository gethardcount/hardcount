package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_CancelDeadlock_AnswersConflict_SameKeyRunsAgain: a cancel that
// Postgres stops to break a real deadlock answers 409 concurrent_change, the
// idempotency layer does not store it, and the same request with the same
// Idempotency-Key then cancels once (ADR-136).
//
// The deadlock is the one a cancel meets with a payment (subscription
// PostgresStore.LockDunningPauseTx): the payment holds the invoice and waits
// for the subscription, and the cancel holds the subscription and waits for
// the invoice. The test's own session plays the payment. It holds the unpaid,
// wholly unused first invoice, which the cancel voids, and waits for the
// cancel to queue on it. Then it asks for the subscription row the cancel
// holds. The test's session waits 10s before its deadlock check and the
// server's sessions 1s, so the cancel's check runs first and Postgres aborts
// the cancel.
//
// It also proves the driver's 40P01 reaches the handler with its chain
// intact: a layer that swallowed it and ran another statement would turn it
// into 25P02 (transaction aborted), which answers 500.
//
// Mutation-verify: drop respond.DeadlockAsConflict from the cancel handler →
// the first answer is a 500, and the same key replays it.
func TestE2E_CancelDeadlock_AnswersConflict_SameKeyRunsAgain(t *testing.T) {
	r := newLagRig(t, "in_advance", false)
	invID := r.firstInvoiceID()
	cancelAt := lagP0.Add(2 * time.Hour) // the period's first day: nothing used, so the cancel voids it
	const key = "idem-e2e-cancel-deadlock"
	path := "/v1/subscriptions/" + r.subID + "/cancel"
	canceledEvents := func() int {
		return flowCount(t, r.db, `SELECT count(*) FROM webhook_outbox
			WHERE event_type = $1 AND payload->'subscription'->>'id' = $2`, domain.EventSubscriptionCanceled, r.subID)
	}

	ctx := t.Context()
	admin := testutil.AdminPool(t)
	payment, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = payment.Rollback() }()
	if _, err := payment.ExecContext(ctx, `SET LOCAL deadlock_timeout = '10s'`); err != nil {
		t.Fatalf("deadlock_timeout: %v", err)
	}
	var pid int
	if err := payment.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	if _, err := payment.ExecContext(ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, invID); err != nil {
		t.Fatalf("lock the invoice: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.doKey(http.MethodPost, path, key, cancelAt, nil) }()
	waitBlockedBy(t, ctx, admin, pid)
	// Closes the cycle. Postgres aborts the cancel, which frees the row.
	if _, err := payment.ExecContext(ctx, `UPDATE subscriptions SET updated_at = updated_at WHERE id = $1`, r.subID); err != nil {
		t.Fatalf("the test's own session was chosen as the deadlock victim (the interleaving was not reached): %v", err)
	}
	if err := payment.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var first *httptest.ResponseRecorder
	select {
	case first = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the cancel never answered")
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &env)
	if first.Code != http.StatusConflict || env.Error.Code != "concurrent_change" || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("cancel that lost the deadlock: %d %s, want 409 concurrent_change from the handler", first.Code, first.Body.String())
	}
	if s := r.sub(); s.Status != domain.SubscriptionActive {
		t.Fatalf("after the lost cancel: subscription %s, want active", s.Status)
	}
	if status, due, _ := r.invoiceMoney(invID, cancelAt); status != string(domain.InvoiceFinalized) || due != 3000 {
		t.Fatalf("after the lost cancel: invoice %s with %dc due, want finalized with 3000c due", status, due)
	}
	if n := canceledEvents(); n != 0 {
		t.Fatalf("%d subscription.canceled rows after the lost cancel, want 0", n)
	}

	again := r.doKey(http.MethodPost, path, key, cancelAt, nil)
	if again.Code != http.StatusOK || again.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("resend with the same key: %d (replayed=%q) %s, want 200 from the handler",
			again.Code, again.Header().Get("Idempotent-Replayed"), again.Body.String())
	}
	if s := r.sub(); s.Status != domain.SubscriptionCanceled {
		t.Fatalf("after the resend: subscription %s, want canceled", s.Status)
	}
	if status, _, _ := r.invoiceMoney(invID, cancelAt); status != string(domain.InvoiceVoided) {
		t.Fatalf("after the resend: invoice %s, want voided", status)
	}
	if n := canceledEvents(); n != 1 {
		t.Fatalf("%d subscription.canceled rows, want 1", n)
	}
}

// waitBlockedBy returns once some backend waits for a lock that the backend
// pid holds.
func waitBlockedBy(t *testing.T, ctx context.Context, admin *sql.DB, pid int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		if err := admin.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend waited for a lock that backend %d holds within 30s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
