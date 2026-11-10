package subscription

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// A cancel or an item change that loses a database deadlock answers 409
// concurrent_change, and the idempotency layer does not store it. The same
// request with the same Idempotency-Key then runs again and makes the change
// once (ADR-136).
//
// The subscription store, its transactions, the idempotency middleware and
// the webhook outbox are real Postgres. deadlockOnceStore makes the first
// transaction of the change fail with SQLSTATE 40P01, as Postgres fails the
// side it stops to break a deadlock. The proration's invoices and tax are
// itemRaceRig's recorders.
//
// Mutation-verify: drop respond.ConcurrentChangeAsConflict from the cancel handler
// (cancel) or from updateItem (update_item): the first answer is a 500, and
// the same key replays it.

// errDeadlockLost is the error Postgres gives the transaction it stops to
// break a deadlock, wrapped as a store wraps it.
func errDeadlockLost() error {
	return fmt.Errorf("lock invoice for status flip: %w", &pgconn.PgError{Severity: "ERROR", Code: "40P01", Message: "deadlock detected"})
}

// deadlockOnceStore is the real store, except that the first cancel or the
// first WithTenantTx, whichever is armed, loses a deadlock.
type deadlockOnceStore struct {
	*PostgresStore
	cancel, tx atomic.Bool
}

// CancelAtomicWithBill runs the real cancel transaction. When armed, its
// bill step (the relief's void, where the deadlock happens) fails, so the
// flip it made is rolled back.
func (s *deadlockOnceStore) CancelAtomicWithBill(ctx context.Context, tenantID, id string, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error) {
	if s.cancel.CompareAndSwap(true, false) {
		billFn = func(*sql.Tx, domain.Subscription) error { return errDeadlockLost() }
	}
	return s.PostgresStore.CancelAtomicWithBill(ctx, tenantID, id, billFn)
}

// WithTenantTx opens the real transaction. When armed, its first statement
// fails, before fn runs.
func (s *deadlockOnceStore) WithTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	if s.tx.CompareAndSwap(true, false) {
		fn = func(*sql.Tx) error { return errDeadlockLost() }
	}
	return s.PostgresStore.WithTenantTx(ctx, tenantID, fn)
}

// deadlockRig is an itemRaceRig whose store loses one deadlock, behind the
// idempotency middleware and the handler's own routes.
type deadlockRig struct {
	*itemRaceRig
	store *deadlockOnceStore
	srv   http.Handler
}

func newDeadlockRig(t *testing.T, db *postgres.DB, name string) *deadlockRig {
	t.Helper()
	r := newItemRaceRig(t, db, name, 5, false)
	close(r.gate.release) // nothing here holds a change open
	r.store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	ds := &deadlockOnceStore{PostgresStore: r.store}
	r.h.svc.store = ds
	return &deadlockRig{itemRaceRig: r, store: ds, srv: middleware.Idempotency(db, nil)(r.h.Routes())}
}

// send serves one request at proNow with the Idempotency-Key key.
func (r *deadlockRig) send(t *testing.T, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	ctx := clock.WithEffectiveNow(auth.WithTenantID(r.ctx, r.tenantID), proNow)
	rr := httptest.NewRecorder()
	r.srv.ServeHTTP(rr, req.WithContext(ctx))
	return rr
}

// events counts the eventType outbox rows about the rig's subscription.
func (r *deadlockRig) events(t *testing.T, eventType string) int {
	t.Helper()
	return len((&eventRig{itemRaceRig: r.itemRaceRig}).events(t, eventType, r.subID, ""))
}

// wantConcurrentChange checks the deadlock loser's answer: 409
// concurrent_change, answered by the handler, not replayed.
func wantConcurrentChange(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	if rr.Code != http.StatusConflict || env.Error.Code != "concurrent_change" {
		t.Fatalf("deadlock loser: %d %s, want 409 concurrent_change", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("deadlock loser answered from the idempotency cache")
	}
}

// wantRanAgain checks the resend's answer: want, from the handler, not
// replayed from the cache.
func wantRanAgain(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want || rr.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("resend with the same key: %d (replayed=%q) %s, want %d from the handler",
			rr.Code, rr.Header().Get("Idempotent-Replayed"), rr.Body.String(), want)
	}
}

func TestDeadlockLoss_AnswersConflict_NotStored(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("cancel", func(t *testing.T) {
		r := newDeadlockRig(t, db, "Deadlock Cancel")
		r.store.cancel.Store(true)
		path := "/" + r.subID + "/cancel"

		wantConcurrentChange(t, r.send(t, http.MethodPost, path, "idem-deadlock-cancel", nil))
		if sub, err := r.store.Get(r.ctx, r.tenantID, r.subID); err != nil || sub.Status != domain.SubscriptionActive {
			t.Fatalf("after the lost cancel: status %q (err %v), want active", sub.Status, err)
		}
		if n := r.events(t, domain.EventSubscriptionCanceled); n != 0 {
			t.Fatalf("%d subscription.canceled rows after the lost cancel, want 0", n)
		}

		wantRanAgain(t, r.send(t, http.MethodPost, path, "idem-deadlock-cancel", nil), http.StatusOK)
		if sub, err := r.store.Get(r.ctx, r.tenantID, r.subID); err != nil || sub.Status != domain.SubscriptionCanceled {
			t.Fatalf("after the resend: status %q (err %v), want canceled", sub.Status, err)
		}
		if n := r.events(t, domain.EventSubscriptionCanceled); n != 1 {
			t.Fatalf("%d subscription.canceled rows, want 1", n)
		}

		// The 200 is stored: a third send replays it and cancels nothing again.
		if rr := r.send(t, http.MethodPost, path, "idem-deadlock-cancel", nil); rr.Code != http.StatusOK || rr.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("third send: %d (replayed=%q), want the stored 200", rr.Code, rr.Header().Get("Idempotent-Replayed"))
		}
		if n := r.events(t, domain.EventSubscriptionCanceled); n != 1 {
			t.Fatalf("%d subscription.canceled rows after the replay, want 1", n)
		}
	})

	t.Run("update_item", func(t *testing.T) {
		// Five seats to six on June 16, with 15 of 30 days left: the change
		// and its proration invoice share the handler's transaction.
		r := newDeadlockRig(t, db, "Deadlock Item")
		r.store.tx.Store(true)
		path := "/" + r.subID + "/items/" + r.itemID
		body := UpdateItemInput{Quantity: new(int64(6)), Immediate: true}

		wantConcurrentChange(t, r.send(t, http.MethodPatch, path, "idem-deadlock-item", body))
		if items := r.liveItems(t); len(items) != 1 || items[0].Quantity != 5 {
			t.Fatalf("after the lost change: items %+v, want the one item at 5 seats", items)
		}
		if n := r.events(t, domain.EventSubscriptionItemUpdated); n != 0 {
			t.Fatalf("%d subscription.item.updated rows after the lost change, want 0", n)
		}
		if made := r.invoices.all(); len(made) != 0 {
			t.Fatalf("the lost change wrote %d proration invoices, want 0", len(made))
		}

		wantRanAgain(t, r.send(t, http.MethodPatch, path, "idem-deadlock-item", body), http.StatusOK)
		if items := r.liveItems(t); len(items) != 1 || items[0].Quantity != 6 {
			t.Fatalf("after the resend: items %+v, want the one item at 6 seats", items)
		}
		if n := r.events(t, domain.EventSubscriptionItemUpdated); n != 1 {
			t.Fatalf("%d subscription.item.updated rows, want 1", n)
		}
		if made := r.invoices.all(); len(made) != 1 {
			t.Fatalf("%d proration invoices, want 1", len(made))
		}
	})
}
