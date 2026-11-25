package subscription_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// TestLifecycleEvents_EnqueuedInTransitionTx is the real-Postgres proof of
// the DispatchTx subscription subset (2026-07-05): subscription.created and
// .canceled are enqueued into webhook_outbox INSIDE the transition tx by the store's writers — so a crash after commit
// can no longer silently drop a lifecycle event (a dropped emission left no
// row anywhere: nothing to replay, nothing to reconcile). Also proves the
// atomicity direction (a failing billFn rolls the event back with the
// cancel) and the CAS exactly-once property (a losing duplicate transition
// enqueues nothing).
func TestLifecycleEvents_EnqueuedInTransitionTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Lifecycle Outbox")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_lifecycle_obx", DisplayName: "Lifecycle Obx",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "lifecycle-monthly", Name: "Lifecycle", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
		BaseAmountCents: 900, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	store := subscription.NewPostgresStore(db)
	store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))

	// events returns (count, lastPayload) for an event type on this tenant.
	events := func(eventType string) (int, map[string]any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin read tx: %v", err)
		}
		defer postgres.Rollback(tx)
		rows, err := tx.QueryContext(ctx,
			`SELECT payload FROM webhook_outbox WHERE event_type = $1 ORDER BY created_at`, eventType)
		if err != nil {
			t.Fatalf("query outbox: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var n int
		var last map[string]any
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatalf("scan payload: %v", err)
			}
			last = nil
			if err := json.Unmarshal(raw, &last); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			n++
		}
		return n, last
	}

	newSub := func(code string) domain.Subscription {
		t.Helper()
		// A real create stamps the current billing period, which the
		// create's interval writer (ADR-101) requires.
		s := domain.Subscription{
			Code: code, DisplayName: code, CustomerID: cust.ID,
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeAnniversary,
			CurrentBillingPeriodStart: bivPS(time.Now().UTC()),
			CurrentBillingPeriodEnd:   bivPE(time.Now().UTC()),
			Items:                     []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		}
		created, err := store.Create(ctx, tenantID, s)
		if err != nil {
			t.Fatalf("create %s: %v", code, err)
		}
		return created
	}

	// resource is the subscription an event carries (ADR-131).
	resource := func(p map[string]any) map[string]any {
		sub, _ := p[domain.WebhookResourceSubscription].(map[string]any)
		return sub
	}

	// (1) created — rides the create tx.
	sub := newSub("lc-created")
	if n, p := events(domain.EventSubscriptionCreated); n != 1 || resource(p)["id"] != sub.ID {
		t.Fatalf("created: got n=%d payload=%v", n, p)
	}

	// (2) canceled (operator) — CancelAtomic; a second cancel (CAS loser)
	// must not enqueue a second event. canceled_by is no longer hardcoded:
	// it derives from the request identity (cancelActorLabel), the same rule
	// the ADR-090 audit row uses, so the webhook and the audit evidence in
	// one tx can't disagree. An OPERATOR cancel is therefore one whose ctx
	// carries operator identity — a bare ctx is a background actor.
	opCtx := auth.WithCaller(ctx, auth.Caller{KeyID: "hc_key_lifecycle_obx"})
	if _, err := subscriptiontest.Cancel(opCtx, store, tenantID, sub.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := subscriptiontest.Cancel(opCtx, store, tenantID, sub.ID); err == nil {
		t.Fatal("second cancel should fail (already canceled)")
	}
	if n, p := events(domain.EventSubscriptionCanceled); n != 1 || p["canceled_by"] != "operator" {
		t.Fatalf("canceled(operator): got n=%d payload=%v", n, p)
	}

	// (3) canceled (dunning) — the origin stamp of the machinery that
	// decided the cancel wins over the request identity.
	subB := newSub("lc-dunning")
	if _, err := subscriptiontest.Cancel(subscription.WithCancelOrigin(opCtx, "dunning"), store, tenantID, subB.ID); err != nil {
		t.Fatalf("dunning cancel: %v", err)
	}
	if n, p := events(domain.EventSubscriptionCanceled); n != 2 || p["canceled_by"] != "dunning" {
		t.Fatalf("canceled(dunning): got n=%d payload=%v", n, p)
	}

	// (4) Atomicity: a failing billFn inside CancelAtomicWithBill rolls the
	// cancel AND its event back together.
	subF := newSub("lc-rollback")
	sentinel := errors.New("bill failed")
	if _, err := store.CancelAtomicWithBill(ctx, tenantID, subF.ID, subscription.SnapshotOf(subF), time.Now().UTC(), func(*sql.Tx, domain.Subscription) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("cancel-with-bill: got %v, want sentinel", err)
	}
	if n, _ := events(domain.EventSubscriptionCanceled); n != 2 {
		t.Fatalf("rolled-back cancel leaked an event: canceled count = %d, want 2", n)
	}
	got, err := store.Get(ctx, tenantID, subF.ID)
	if err != nil || got.Status != domain.SubscriptionActive {
		t.Fatalf("subF must remain active after rollback: status=%v err=%v", got.Status, err)
	}
}
