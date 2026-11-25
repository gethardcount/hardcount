package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// A canceled subscription carries no cancel schedule and no pending plan
// change on its items: both say what a live subscription does next. Every
// cancel writer used to leave them, so the API and the dashboard showed a
// scheduled cancel (with Undo) or a coming plan change on a canceled
// subscription. The collection pause stays: it still holds the
// subscription's unpaid invoices until an operator resumes collection.

// liveStateRig is one tenant with two plans, for subscriptions that carry a
// cancel schedule, a keep_as_draft pause with resumes_at, and a pending plan
// change on their item.
type liveStateRig struct {
	t        *testing.T
	ctx      context.Context
	db       *postgres.DB
	store    *PostgresStore
	tenantID string
	custID   string
	planA    domain.Plan
	planB    domain.Plan
	now      time.Time
}

func newLiveStateRig(t *testing.T, name string) *liveStateRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	return &liveStateRig{
		t: t, ctx: ctx, db: db, store: NewPostgresStore(db), tenantID: tenantID,
		custID: ivCustomer(t, db, ctx, tenantID, "cus_live_state"),
		planA:  ivPlan(t, db, ctx, tenantID, "live-state-a", 1000),
		planB:  ivPlan(t, db, ctx, tenantID, "live-state-b", 2000),
		now:    time.Now().UTC().Truncate(time.Second),
	}
}

// seed creates an active subscription with a pause and a pending plan change.
// The caller schedules its cancel.
func (r *liveStateRig) seed(code string) domain.Subscription {
	r.t.Helper()
	start := r.now.Add(-24 * time.Hour)
	end := start.AddDate(0, 1, 0)
	in := domain.Subscription{
		Code: code, DisplayName: code, CustomerID: r.custID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &start, CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: &end, NextBillingAt: &end,
		Items: []domain.SubscriptionItem{{PlanID: r.planA.ID, Quantity: 1}},
	}
	sub, err := r.store.Create(r.ctx, r.tenantID, in)
	if err != nil {
		r.t.Fatalf("create %s: %v", code, err)
	}
	resumes := r.now.Add(10 * 24 * time.Hour)
	if _, err := r.store.SetPauseCollection(r.ctx, r.tenantID, sub.ID, domain.PauseCollection{
		Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumes,
	}, nil); err != nil {
		r.t.Fatalf("pause %s: %v", code, err)
	}
	if _, err := r.store.SetItemPendingPlan(r.ctx, r.tenantID, sub.Items[0].ID, r.planB.ID, end); err != nil {
		r.t.Fatalf("pending plan %s: %v", code, err)
	}
	if sub, err = r.store.Get(r.ctx, r.tenantID, sub.ID); err != nil {
		r.t.Fatalf("re-read %s: %v", code, err)
	}
	if sub.PauseCollection == nil || sub.Items[0].PendingPlanID != r.planB.ID {
		r.t.Fatalf("seed %s: pause=%v pending=%q, want both set", code, sub.PauseCollection, sub.Items[0].PendingPlanID)
	}
	return sub
}

func (r *liveStateRig) schedule(sub domain.Subscription) {
	r.t.Helper()
	if _, err := r.store.ScheduleCancellation(r.ctx, r.tenantID, sub.ID); err != nil {
		r.t.Fatalf("schedule cancel: %v", err)
	}
}

func (r *liveStateRig) itemChanges(subID string) int {
	r.t.Helper()
	var n int
	if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.ctx, `SELECT count(*) FROM subscription_item_changes WHERE subscription_id = $1`, subID).Scan(&n)
	}); err != nil {
		r.t.Fatalf("count item changes: %v", err)
	}
	return n
}

// requireRetired fails unless sub (the returned and the stored row) is
// canceled with no schedule and no pending plan change, and keeps its pause.
func (r *liveStateRig) requireRetired(label string, sub domain.Subscription) {
	r.t.Helper()
	if sub.Status != domain.SubscriptionCanceled {
		r.t.Fatalf("%s: status %s, want canceled", label, sub.Status)
	}
	if sub.CancelAtPeriodEnd {
		r.t.Errorf("%s: cancel schedule left on a canceled subscription: cancel_at_period_end=%v", label, sub.CancelAtPeriodEnd)
	}
	if sub.PauseCollection == nil {
		r.t.Errorf("%s: the cancel cleared the pause: it still holds the subscription's unpaid invoices", label)
	}
	if len(sub.Items) == 0 {
		r.t.Fatalf("%s: no items", label)
	}
	for _, it := range sub.Items {
		if it.PendingPlanID != "" || it.PendingPlanEffectiveAt != nil {
			r.t.Errorf("%s: item %s keeps pending plan %q at %v", label, it.ID, it.PendingPlanID, it.PendingPlanEffectiveAt)
		}
	}
}

// TestCancelWriters_RetireLiveOnlyState runs each cancel writer on a
// subscription with a cancel schedule, a pause and a pending plan change.
// Each must hand back, and store, a row with no schedule and no
// pending plan change but with its pause, without writing an item-change
// row (clearing the pending columns is no plan or quantity change). A writer
// that runs billFn must give it the row with its pause: the final invoice is
// decided against the pause in force at the cancel instant. Each writer
// queues one subscription.canceled whose subscription is the stored row
// (ADR-131): queued before the retire, it would still show the pending plan
// change, and on the operator cancel the schedule.
//
// Mutation: drop retireLiveOnlyStateTx from any one writer and its case
// fails; drop its items UPDATE and every case fails; queue the event before
// the retire and every case fails.
func TestCancelWriters_RetireLiveOnlyState(t *testing.T) {
	r := newLiveStateRig(t, "Cancel Retires Live State")
	r.store.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))

	type writer struct {
		name   string
		cancel func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription)
	}
	billSaw := func(saw **domain.Subscription) func(tx *sql.Tx, c domain.Subscription) error {
		return func(_ *sql.Tx, c domain.Subscription) error {
			cp := c
			*saw = &cp
			return nil
		}
	}
	writers := []writer{
		{"CancelAtomicWithBill", func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription) {
			r.schedule(sub)
			var saw *domain.Subscription
			got, err := r.store.CancelAtomicWithBill(r.ctx, r.tenantID, sub.ID, SnapshotOf(sub), r.now, billSaw(&saw))
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			return got, saw
		}},
		{"FireScheduledCancellationTx", func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription) {
			r.schedule(sub)
			var got domain.Subscription
			if err := r.store.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
				var err error
				got, err = r.store.FireScheduledCancellationTx(r.ctx, tx, r.tenantID, sub.ID, SnapshotOf(sub), r.now)
				return err
			}); err != nil {
				t.Fatalf("fire at period end: %v", err)
			}
			return got, nil
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			sub := r.seed("sub-live-state-" + strings.ToLower(w.name))
			changesBefore := r.itemChanges(sub.ID)

			got, billInput := w.cancel(t, sub)

			if billInput != nil && billInput.PauseCollection == nil {
				t.Error("billFn got the row without its pause: the final invoice must be decided against the pause in force at the cancel instant")
			}
			r.requireRetired("returned row", got)
			stored, err := r.store.Get(r.ctx, r.tenantID, sub.ID)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			r.requireRetired("stored row", stored)
			if after := r.itemChanges(sub.ID); after != changesBefore {
				t.Errorf("item-change rows %d → %d: clearing a pending plan is not a plan change", changesBefore, after)
			}
			sent := r.canceledEvent(sub.ID)
			var sentSub domain.Subscription
			if err := json.Unmarshal(sent, &sentSub); err != nil {
				t.Fatalf("decode subscription.canceled: %v", err)
			}
			r.requireRetired("subscription.canceled", sentSub)
			var sentMap any
			if err := json.Unmarshal(sent, &sentMap); err != nil {
				t.Fatalf("decode subscription.canceled: %v", err)
			}
			sentJSON, _ := json.Marshal(sentMap)
			want, _ := json.Marshal(domain.SubscriptionWebhookData(stored, nil)[domain.WebhookResourceSubscription])
			if string(sentJSON) != string(want) {
				t.Errorf("subscription.canceled is not the stored row:\n event %s\nstored %s", sentJSON, want)
			}
		})
	}
}

// canceledEvent is the subscription, as JSON, of the one
// subscription.canceled queued for subID.
func (r *liveStateRig) canceledEvent(subID string) json.RawMessage {
	r.t.Helper()
	var raws [][]byte
	if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.ctx, `SELECT payload FROM webhook_outbox
			WHERE event_type = $1 AND payload->'subscription'->>'id' = $2`, domain.EventSubscriptionCanceled, subID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			raws = append(raws, raw)
		}
		return rows.Err()
	}); err != nil {
		r.t.Fatalf("read outbox: %v", err)
	}
	if len(raws) != 1 {
		r.t.Fatalf("subscription.canceled rows for %s: %d, want 1", subID, len(raws))
	}
	var payload struct {
		Subscription json.RawMessage `json:"subscription"`
	}
	if err := json.Unmarshal(raws[0], &payload); err != nil {
		r.t.Fatalf("decode subscription.canceled: %v", err)
	}
	return payload.Subscription
}
