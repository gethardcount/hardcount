package billing_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// An invoice the engine creates already finalized queues invoice.finalized
// in the transaction that inserts it (ADR-040), so the invoice and its event
// commit together. Each test ends the caller's context right after that
// transaction commits, the way a deploy, a lost lease or a stopping billing
// run does, and checks that the event was saved anyway.

// wireEvents wires the fixture's webhook events as the router wires them.
func (f *thresholdFixture) wireEvents() {
	outbox := webhook.NewOutboxStore(f.db)
	f.invStore.SetOutboxEnqueuer(outbox)
	f.subStore.SetOutboxEnqueuer(outbox)
	f.engine.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
}

// finalizedEvents returns the data of every invoice.finalized outbox row of
// the tenant, read under its row-level security.
func finalizedEvents(t *testing.T, db *postgres.DB, tenantID string) []map[string]any {
	t.Helper()
	return queuedEvents(t, db, tenantID, domain.EventInvoiceFinalized)
}

// queuedEvents returns the data of every eventType outbox row of the tenant,
// read under its row-level security.
func queuedEvents(t *testing.T, db *postgres.DB, tenantID, eventType string) []map[string]any {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM webhook_outbox WHERE event_type = $1`, eventType)
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
		out = append(out, data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// checkOneFinalizedEvent checks that the tenant has exactly one invoice and
// one invoice.finalized row, and that the row is about that invoice.
func (f *thresholdFixture) checkOneFinalizedEvent(t *testing.T, reason domain.InvoiceBillingReason) {
	t.Helper()
	invs := f.listInvoices(t, postgres.WithLivemode(context.Background(), false))
	if len(invs) != 1 || invs[0].BillingReason != reason || invs[0].Status != domain.InvoiceFinalized {
		t.Fatalf("invoices %+v, want one finalized %s invoice", invs, reason)
	}
	got := finalizedEvents(t, f.db, f.tenantID)
	if len(got) != 1 {
		t.Fatalf("%d invoice.finalized rows, want 1: the invoice committed, so its event must be saved with it", len(got))
	}
	if err := domain.CheckWebhookData(domain.EventInvoiceFinalized, got[0]); err != nil {
		t.Fatal(err)
	}
	inv, _ := got[0][domain.WebhookResourceInvoice].(map[string]any)
	if inv["id"] != invs[0].ID || inv["status"] != string(domain.InvoiceFinalized) {
		t.Errorf("invoice.finalized is about %v (status %v), want %s (finalized)", inv["id"], inv["status"], invs[0].ID)
	}
	if token, _ := inv["public_token"].(string); token == "" {
		t.Error("invoice.finalized carries no public_token: an invoice born finalized has a hosted page")
	}
}

// TestCycleClose_ContextEndsAfterCommit_InvoiceFinalizedIsQueued: the
// billing run closes a period, and its context ends right after the close
// commits. The cycle invoice's invoice.finalized is saved.
func TestCycleClose_ContextEndsAfterCommit_InvoiceFinalizedIsQueued(t *testing.T) {
	f := newThresholdFixture(t, "Outbox Cycle Close Ends")
	f.wireEvents()
	base := postgres.WithLivemode(context.Background(), false)
	f.ingestUsage(t, base, 10, 10)

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	f.subAdapter.afterTx = cancel
	f.engine.RunCycleForTenant(clock.WithEffectiveNow(ctx, f.cycleEnd.Add(time.Hour)), f.tenantID, 50)

	f.checkOneFinalizedEvent(t, domain.BillingReasonSubscriptionCycle)
}

// TestThresholdFire_ContextEndsAfterCommit_InvoiceFinalizedIsQueued: a spend
// threshold fires, and the scan's context ends right after the fire commits.
// The threshold invoice's invoice.finalized is saved.
func TestThresholdFire_ContextEndsAfterCommit_InvoiceFinalizedIsQueued(t *testing.T) {
	f := newThresholdFixture(t, "Outbox Threshold Ends")
	f.wireEvents()
	base, stop := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer stop()
	f.ingestUsage(t, base, 100, 10)
	if _, err := f.subSvc.SetBillingThresholds(base, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 500}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	f.subAdapter.afterTx = cancel
	f.engine.ScanThresholds(ctx, 50)

	f.checkOneFinalizedEvent(t, domain.BillingReasonThreshold)
}

// checkSubscriptionData checks that data carries the subscription as GET
// /v1/subscriptions/{id} returns it now: the change committed with its event,
// and nothing wrote the subscription after it.
func (f *thresholdFixture) checkSubscriptionData(t *testing.T, eventType string, data map[string]any) {
	t.Helper()
	if err := domain.CheckWebhookData(eventType, data); err != nil {
		t.Fatal(err)
	}
	sub, err := f.subStore.Get(postgres.WithLivemode(context.Background(), false), f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	enc, err := json.Marshal(domain.SubscriptionWebhookData(sub, nil))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(enc, &want); err != nil {
		t.Fatal(err)
	}
	if got := data[domain.WebhookResourceSubscription]; !reflect.DeepEqual(got, want[domain.WebhookResourceSubscription]) {
		t.Errorf("%s data.subscription differs from the subscription the change committed:\n got %v\nwant %v",
			eventType, got, want[domain.WebhookResourceSubscription])
	}
}

// TestThresholdFire_ContextEndsAfterCommit_ThresholdCrossedIsQueued: a spend
// threshold fires, and the scan's context ends right after the fire commits.
// subscription.threshold_crossed is saved, once, with the crossing's facts.
func TestThresholdFire_ContextEndsAfterCommit_ThresholdCrossedIsQueued(t *testing.T) {
	f := newThresholdFixture(t, "Outbox Threshold Crossed Ends")
	f.wireEvents()
	base, stop := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer stop()
	f.ingestUsage(t, base, 100, 10)
	if _, err := f.subSvc.SetBillingThresholds(base, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 500}); err != nil {
		t.Fatalf("set threshold: %v", err)
	}

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	f.subAdapter.afterTx = cancel
	f.engine.ScanThresholds(ctx, 50)

	invs := f.listInvoices(t, base)
	if len(invs) != 1 || invs[0].BillingReason != domain.BillingReasonThreshold {
		t.Fatalf("invoices %+v, want one threshold invoice", invs)
	}
	got := queuedEvents(t, f.db, f.tenantID, domain.EventSubscriptionThresholdCrossed)
	if len(got) != 1 {
		t.Fatalf("%d subscription.threshold_crossed rows, want 1: the fire committed, so its event must be saved with it", len(got))
	}
	f.checkSubscriptionData(t, domain.EventSubscriptionThresholdCrossed, got[0])
	if got[0]["invoice_id"] != invs[0].ID || got[0]["running_subtotal"] != float64(1000) ||
		got[0]["crossed_amount"] != true || got[0]["crossed_item_id"] != "" {
		t.Errorf("threshold facts = invoice_id %v running_subtotal %v crossed_amount %v crossed_item_id %q, want %s 1000 true \"\"",
			got[0]["invoice_id"], got[0]["running_subtotal"], got[0]["crossed_amount"], got[0]["crossed_item_id"], invs[0].ID)
	}
}

// TestCycleClose_ContextEndsAfterPlanChangeApplies_PendingChangeAppliedIsQueued:
// an item has a plan change scheduled for the period end. The billing run
// applies it at the close, and the run's context ends right after the change
// commits. subscription.pending_change.applied is saved, once, naming the
// plan the item left.
func TestCycleClose_ContextEndsAfterPlanChangeApplies_PendingChangeAppliedIsQueued(t *testing.T) {
	f := newThresholdFixture(t, "Outbox Pending Applied Ends")
	f.wireEvents()
	base, stop := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer stop()
	next, _, err := f.pricingSvc.CreatePlan(base, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_thresh_next", Name: "Next Plan", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Prices: []pricing.PriceInput{{MeterID: f.meterID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(2)}}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := f.subSvc.UpdateItem(base, f.tenantID, f.subID, f.itemID, subscription.UpdateItemInput{NewPlanID: next.ID}); err != nil {
		t.Fatalf("schedule the plan change: %v", err)
	}

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	f.subAdapter.afterApply = cancel
	f.engine.RunCycleForTenant(clock.WithEffectiveNow(ctx, f.cycleEnd.Add(time.Hour)), f.tenantID, 50)

	sub, err := f.subStore.Get(base, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("get subscription: %v", err)
	}
	if len(sub.Items) != 1 || sub.Items[0].PlanID != next.ID {
		t.Fatalf("items %+v, want the item on %s: the change applied at the close", sub.Items, next.ID)
	}
	got := queuedEvents(t, f.db, f.tenantID, domain.EventSubscriptionPendingChangeApplied)
	if len(got) != 1 {
		t.Fatalf("%d subscription.pending_change.applied rows, want 1: the change committed, so its event must be saved with it", len(got))
	}
	f.checkSubscriptionData(t, domain.EventSubscriptionPendingChangeApplied, got[0])
	appliedAt, _ := got[0]["applied_at"].(string)
	at, err := time.Parse(time.RFC3339Nano, appliedAt)
	if got[0]["item_id"] != f.itemID || got[0]["old_plan_id"] != f.planID || err != nil || !at.Equal(f.cycleEnd) {
		t.Errorf("pending_change.applied extras = item_id %v old_plan_id %v applied_at %v, want %s %s %s",
			got[0]["item_id"], got[0]["old_plan_id"], got[0]["applied_at"], f.itemID, f.planID, f.cycleEnd)
	}
}
