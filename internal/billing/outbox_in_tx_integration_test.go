package billing_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
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
	f.engine.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
}

// finalizedEvents returns the data of every invoice.finalized outbox row of
// the tenant, read under its row-level security.
func finalizedEvents(t *testing.T, db *postgres.DB, tenantID string) []map[string]any {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM webhook_outbox WHERE event_type = $1`, domain.EventInvoiceFinalized)
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
