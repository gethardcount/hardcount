package subscription

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// Every subscription event is queued in the transaction of the change it
// reports (ADR-040), so the change and its event commit together. Each test
// here ends the request's context right after the change commits, the way a
// deploy, a dropped connection or the route timeout does, and checks that the
// event was saved anyway: once, with the subscription as GET returns it after
// the commit (ADR-131).
//
// The subscription store, its transactions and the webhook outbox are real
// Postgres. The proration's invoices and credits are newItemRaceRig's
// recorders.

// stopAfterCommit is the audit logger of the handler and the service. Every
// change here writes its audit row right after its transaction commits, so
// Log is the first step after the commit: it ends the request's context.
type stopAfterCommit struct {
	mu   sync.Mutex
	stop context.CancelFunc
}

func (s *stopAfterCommit) set(stop context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop = stop
}

func (s *stopAfterCommit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
	}
	return nil
}

func (s *stopAfterCommit) LogInTx(context.Context, *sql.Tx, audit.Entry) error { return nil }

func (s *stopAfterCommit) Query(context.Context, string, audit.QueryFilter) ([]domain.AuditEntry, int, error) {
	return nil, 0, nil
}

// eventRig is an itemRaceRig whose webhook events are wired as the router
// wires them.
type eventRig struct {
	*itemRaceRig
	stopper *stopAfterCommit
}

func newEventRig(t *testing.T, db *postgres.DB, name string, withKeeper bool) *eventRig {
	t.Helper()
	r := newItemRaceRig(t, db, name, 5, withKeeper)
	close(r.gate.release) // nothing here holds a change open
	r.store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	stopper := &stopAfterCommit{}
	r.h.SetAuditLogger(stopper)
	r.h.svc.SetAuditLogger(stopper)
	return &eventRig{itemRaceRig: r, stopper: stopper}
}

// handlerCall is one handler method.
type handlerCall func(*Handler, http.ResponseWriter, *http.Request)

// call sends one request at `at` whose context ends at its first step after
// the commit (stopAfterCommit). params are chi URL parameters, in pairs.
func (r *eventRig) call(t *testing.T, at time.Time, handle handlerCall, body any, params ...string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(clock.WithEffectiveNow(auth.WithTenantID(r.ctx, r.tenantID), at))
	defer cancel()
	r.stopper.set(cancel)
	defer r.stopper.set(nil)
	return r.serve(t, ctx, handle, body, params...)
}

// serve sends one request with ctx, which nothing ends early.
func (r *eventRig) serve(t *testing.T, ctx context.Context, handle handlerCall, body any, params ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/", rd)
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		rctx.URLParams.Add(params[i], params[i+1])
	}
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	handle(r.h, rr, req)
	return rr
}

// events returns the data of the tenant's eventType outbox rows about
// subscription subID (and item itemID, when set), read under its row-level
// security.
func (r *eventRig) events(t *testing.T, eventType, subID, itemID string) []map[string]any {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT payload FROM webhook_outbox WHERE event_type = $1`, eventType)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		sub, _ := data[domain.WebhookResourceSubscription].(map[string]any)
		if sub["id"] != subID || (itemID != "" && data["item_id"] != itemID) {
			continue
		}
		out = append(out, data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// checkOneEvent checks that exactly one eventType row is queued for subID
// (and extra's item_id), and that it carries the subscription as GET returns
// it now, with extra beside it.
func (r *eventRig) checkOneEvent(t *testing.T, eventType, subID string, extra map[string]any) {
	t.Helper()
	itemID, _ := extra["item_id"].(string)
	got := r.events(t, eventType, subID, itemID)
	switch {
	case len(got) == 0:
		t.Errorf("0 %s rows, want 1: the change committed, so its event must be saved with it", eventType)
		return
	case len(got) > 1:
		t.Errorf("%d %s rows, want 1: one change sends one event", len(got), eventType)
		return
	}
	if err := domain.CheckWebhookData(eventType, got[0]); err != nil {
		t.Error(err)
	}
	sub, err := r.store.Get(r.ctx, r.tenantID, subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	enc, err := json.Marshal(domain.SubscriptionWebhookData(sub, extra))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(enc, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("%s data differs from the subscription the change committed:\n got %v\nwant %v", eventType, got[0], want)
	}
}

// checkStatus checks a response's status code.
func checkStatus(t *testing.T, what string, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("%s: status %d, want %d. body=%s", what, rr.Code, want, rr.Body.String())
	}
}

// newTrial creates a trialing subscription on the basic and keeper plans for
// the rig's customer, its trial ending June 24, 2026.
func (r *eventRig) newTrial(t *testing.T) domain.Subscription {
	t.Helper()
	active, err := r.store.Get(r.ctx, r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	trialStart := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	trialEnd := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	periodEnd := trialEnd.AddDate(0, 1, 0)
	trial, err := r.store.Create(r.ctx, r.tenantID, domain.Subscription{
		Code: "sub-outbox-trial", DisplayName: "Outbox Trial", CustomerID: active.CustomerID,
		Status: domain.SubscriptionTrialing, BillingTime: domain.BillingTimeCalendar,
		TrialStartAt: &trialStart, TrialEndAt: &trialEnd, StartedAt: &trialStart,
		CurrentBillingPeriodStart: &trialEnd, CurrentBillingPeriodEnd: &periodEnd, NextBillingAt: &periodEnd,
		Items: []domain.SubscriptionItem{
			{PlanID: r.plans["basic"].ID, Quantity: 1},
			{PlanID: r.plans["keeper"].ID, Quantity: 1},
		},
	})
	if err != nil {
		t.Fatalf("create trialing subscription: %v", err)
	}
	return trial
}

// itemOn returns the id of sub's item on planID.
func itemOn(t *testing.T, sub domain.Subscription, planID string) string {
	t.Helper()
	for _, it := range sub.Items {
		if it.PlanID == planID {
			return it.ID
		}
	}
	t.Fatalf("subscription %s has no item on %s: %+v", sub.ID, planID, sub.Items)
	return ""
}

// TestLifecycleEvents_ContextEndsAfterCommit_EventIsQueued: the operator's
// schedule and clear of a cancel, pause and resume of collection, and trial
// extension. Each request's context ends right after its change commits, and
// the event is saved. A clear or resume that changes nothing sends none.
func TestLifecycleEvents_ContextEndsAfterCommit_EventIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newEventRig(t, db, "Outbox Lifecycle Ends", true)
	sub := []string{"id", r.subID}
	operator := map[string]any{"triggered_by": "operator"}

	checkStatus(t, "schedule cancel", r.call(t, proNow, (*Handler).scheduleCancel, map[string]any{"at_period_end": true}, sub...), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionCancelScheduled, r.subID, nil)

	checkStatus(t, "clear scheduled cancel", r.call(t, proNow, (*Handler).clearScheduledCancel, nil, sub...), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionCancelCleared, r.subID, nil)
	checkStatus(t, "clear again", r.call(t, proNow, (*Handler).clearScheduledCancel, nil, sub...), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionCancelCleared, r.subID, nil)

	checkStatus(t, "pause collection", r.call(t, proNow, (*Handler).pauseCollection, map[string]any{"behavior": "keep_as_draft"}, sub...), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionCollectionPaused, r.subID, operator)

	checkStatus(t, "resume collection", r.call(t, proNow, (*Handler).resumeCollection, nil, sub...), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionCollectionResumed, r.subID, operator)
	checkStatus(t, "resume again", r.call(t, proNow, (*Handler).resumeCollection, nil, sub...), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionCollectionResumed, r.subID, operator)

	trial := r.newTrial(t)
	newEnd := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	checkStatus(t, "extend trial", r.call(t, proNow, (*Handler).extendTrial, map[string]any{"trial_end": newEnd}, "id", trial.ID), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionTrialExtended, trial.ID, operator)
}

// TestItemEvents_ContextEndsAfterCommit_EventIsQueued: each item change, on
// the path that writes its proration in the change's transaction (an active
// subscription mid-period) and on the path that writes none (a trialing
// subscription, a scheduled change). Each request's context ends right after
// its change commits, and the event is saved.
func TestItemEvents_ContextEndsAfterCommit_EventIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newEventRig(t, db, "Outbox Items Ends", true)
	sub := []string{"id", r.subID}

	// Prorated: add, quantity change, remove.
	checkStatus(t, "add item", r.call(t, proNow, (*Handler).addItem, AddItemInput{PlanID: r.plans["pro"].ID, Quantity: 1}, sub...), http.StatusCreated)
	active, err := r.store.Get(r.ctx, r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	added := itemOn(t, active, r.plans["pro"].ID)
	r.checkOneEvent(t, domain.EventSubscriptionItemAdded, r.subID, map[string]any{"item_id": added})

	qty := int64(7)
	checkStatus(t, "change quantity", r.call(t, proNow, (*Handler).updateItem, UpdateItemInput{Quantity: &qty, Immediate: true}, "id", r.subID, "itemID", r.itemID), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionItemUpdated, r.subID, map[string]any{"item_id": r.itemID})

	checkStatus(t, "remove item", r.call(t, proNow, (*Handler).removeItem, nil, "id", r.subID, "itemID", added), http.StatusNoContent)
	r.checkOneEvent(t, domain.EventSubscriptionItemRemoved, r.subID, map[string]any{"item_id": added})

	// Not prorated: a scheduled plan change, and an add and a removal on a
	// trialing subscription.
	checkStatus(t, "schedule plan change", r.call(t, proNow, (*Handler).updateItem, UpdateItemInput{NewPlanID: r.plans["meter-a"].ID}, "id", r.subID, "itemID", r.keeperID), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionPendingChangeScheduled, r.subID, map[string]any{"item_id": r.keeperID})

	trial := r.newTrial(t)
	checkStatus(t, "add item to a trial", r.call(t, proNow, (*Handler).addItem, AddItemInput{PlanID: r.plans["pro"].ID, Quantity: 1}, "id", trial.ID), http.StatusCreated)
	trial, err = r.store.Get(r.ctx, r.tenantID, trial.ID)
	if err != nil {
		t.Fatalf("get trial: %v", err)
	}
	trialAdded := itemOn(t, trial, r.plans["pro"].ID)
	r.checkOneEvent(t, domain.EventSubscriptionItemAdded, trial.ID, map[string]any{"item_id": trialAdded})

	checkStatus(t, "remove item from a trial", r.call(t, proNow, (*Handler).removeItem, nil, "id", trial.ID, "itemID", trialAdded), http.StatusNoContent)
	r.checkOneEvent(t, domain.EventSubscriptionItemRemoved, trial.ID, map[string]any{"item_id": trialAdded})
}

// TestCrossIntervalSwap_ContextEndsAfterCommit_ItemUpdatedIsQueued: an
// immediate swap to a plan with another billing interval restructures the
// period in the change's transaction. The request's context ends right after
// it commits, and subscription.item.updated is saved with the new period.
func TestCrossIntervalSwap_ContextEndsAfterCommit_ItemUpdatedIsQueued(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newEventRig(t, db, "Outbox Swap Ends", false)

	checkStatus(t, "swap to yearly", r.call(t, proNow, (*Handler).updateItem, UpdateItemInput{NewPlanID: r.plans["basic-yearly"].ID, Immediate: true}, "id", r.subID, "itemID", r.itemID), http.StatusOK)
	r.checkOneEvent(t, domain.EventSubscriptionItemUpdated, r.subID, map[string]any{"item_id": r.itemID})
}

// cancelPending sends DELETE /v1/subscriptions/{id}/items/{itemID}/pending-change
// at proNow, with a context nothing ends early.
func (r *eventRig) cancelPending(t *testing.T, h *Handler, itemID string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := clock.WithEffectiveNow(auth.WithTenantID(r.ctx, r.tenantID), proNow)
	saved := r.h
	r.h = h
	defer func() { r.h = saved }()
	return r.serve(t, ctx, (*Handler).cancelPendingItemChange, nil, "id", r.subID, "itemID", itemID)
}

// schedule schedules the rig's basic item onto the pro plan at the period end.
func (r *eventRig) schedule(t *testing.T) {
	t.Helper()
	ctx := clock.WithEffectiveNow(r.ctx, proNow)
	if _, err := r.store.SetItemPendingPlan(ctx, r.tenantID, r.itemID, r.plans["pro"].ID, proPeriodEnd); err != nil {
		t.Fatalf("schedule the plan change: %v", err)
	}
}

// applyFirst is the subscription store, except that ClearItemPendingPlan
// first runs apply: a concurrent writer that commits between the service's
// read of the item and the clear (playbook §5.3).
type applyFirst struct {
	*PostgresStore
	apply func()
}

func (s *applyFirst) ClearItemPendingPlan(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	s.apply()
	return s.PostgresStore.ClearItemPendingPlan(ctx, tenantID, itemID)
}

// TestCancelPendingItemChange_EventOnlyWhenAChangeIsCanceled:
// subscription.pending_change.canceled says a scheduled plan change was
// canceled, so it is sent once for each change that really was, and never
// for a cancel that changed nothing.
func TestCancelPendingItemChange_EventOnlyWhenAChangeIsCanceled(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("nothing scheduled", func(t *testing.T) {
		r := newEventRig(t, db, "Outbox Pending Nothing", true)
		checkStatus(t, "cancel", r.cancelPending(t, r.h, r.itemID), http.StatusOK)
		if got := r.events(t, domain.EventSubscriptionPendingChangeCanceled, r.subID, r.itemID); len(got) != 0 {
			t.Errorf("%d pending_change.canceled rows, want 0: nothing was scheduled", len(got))
		}
	})

	t.Run("canceled twice", func(t *testing.T) {
		r := newEventRig(t, db, "Outbox Pending Twice", true)
		r.schedule(t)
		checkStatus(t, "cancel", r.cancelPending(t, r.h, r.itemID), http.StatusOK)
		checkStatus(t, "cancel again", r.cancelPending(t, r.h, r.itemID), http.StatusOK)
		r.checkOneEvent(t, domain.EventSubscriptionPendingChangeCanceled, r.subID, map[string]any{"item_id": r.itemID})
	})

	t.Run("two cancels at once", func(t *testing.T) {
		r := newEventRig(t, db, "Outbox Pending Concurrent", true)
		r.schedule(t)
		// Hold the subscription row, as a writer in the middle of a change
		// does, so both cancels read the scheduled change before either
		// clears it.
		tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		committed := false
		defer func() {
			if !committed {
				postgres.Rollback(tx)
			}
		}()
		if _, err := r.store.LockForItemChangeTx(r.ctx, tx, r.tenantID, r.subID); err != nil {
			t.Fatalf("lock subscription: %v", err)
		}
		out := make([]*httptest.ResponseRecorder, 2)
		var wg sync.WaitGroup
		for i := range out {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := clock.WithEffectiveNow(auth.WithTenantID(r.ctx, r.tenantID), proNow)
				out[i] = r.serve(t, ctx, (*Handler).cancelPendingItemChange, nil, "id", r.subID, "itemID", r.itemID)
			}()
		}
		testutil.WaitForLockWaiters(t, r.ctx, testutil.AdminPool(t), "subscription", len(out))
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		committed = true
		wg.Wait()
		for i, rr := range out {
			checkStatus(t, "cancel "+string(rune('A'+i)), rr, http.StatusOK)
		}
		r.checkOneEvent(t, domain.EventSubscriptionPendingChangeCanceled, r.subID, map[string]any{"item_id": r.itemID})
	})

	t.Run("the change applied first", func(t *testing.T) {
		r := newEventRig(t, db, "Outbox Pending Applied First", true)
		r.schedule(t)
		// The billing run applies the change at the period end, between the
		// cancel's read of the item and its clear.
		racing := &applyFirst{PostgresStore: r.store, apply: func() {
			if _, err := r.store.ApplyDuePendingItemPlansAtomic(r.ctx, r.tenantID, r.subID, proPeriodEnd, proPeriodEnd); err != nil {
				t.Errorf("apply the change: %v", err)
			}
		}}
		svc := svcWithStore(racing)
		svc.SetPlanReader(r.h.svc.plans)
		rr := r.cancelPending(t, NewHandler(svc), r.itemID)
		checkStatus(t, "cancel", rr, http.StatusConflict)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error.Code != "invalid_state" {
			t.Errorf("error %s, want code invalid_state", rr.Body.String())
		}
		if got := r.events(t, domain.EventSubscriptionPendingChangeCanceled, r.subID, r.itemID); len(got) != 0 {
			t.Errorf("%d pending_change.canceled rows, want 0: the change took effect, nothing was canceled", len(got))
		}
		r.checkOneEvent(t, domain.EventSubscriptionPendingChangeApplied, r.subID, map[string]any{
			"item_id": r.itemID, "old_plan_id": r.plans["basic"].ID, "applied_at": proPeriodEnd,
		})
	})
}

// TestItemEvent_RollsBackWithItsChange: an item change's event is written on
// the change's transaction, so a change that rolls back leaves no event.
func TestItemEvent_RollsBackWithItsChange(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newEventRig(t, db, "Outbox Item Rollback", true)
	ctx := clock.WithEffectiveNow(r.ctx, proNow)
	errLater := errors.New("a later write of the change fails")

	var added string
	err := r.store.WithTenantTx(ctx, r.tenantID, func(tx *sql.Tx) error {
		res, err := r.h.svc.AddItemTx(ctx, tx, r.tenantID, r.subID, AddItemInput{PlanID: r.plans["pro"].ID, Quantity: 1})
		if err != nil {
			return err
		}
		added = res.Item.ID
		return errLater
	})
	if !errors.Is(err, errLater) {
		t.Fatalf("add: %v, want the later write's error", err)
	}
	if got := r.events(t, domain.EventSubscriptionItemAdded, r.subID, added); len(got) != 0 {
		t.Errorf("%d item.added rows, want 0: the add rolled back", len(got))
	}
}
