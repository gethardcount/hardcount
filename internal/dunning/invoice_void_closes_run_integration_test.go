package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// countingStopper is the Stripe leg of a void, counted per invoice.
type countingStopper struct{ stopped []string }

func (c *countingStopper) StopCollection(_ context.Context, _ string, inv domain.Invoice) {
	c.stopped = append(c.stopped, inv.ID)
}

// ExpireCheckoutSessions is a write-off's leg; these tests void.
func (*countingStopper) ExpireCheckoutSessions(context.Context, string, domain.Invoice) {}

// The invoice's void (POST /v1/invoices/{id}/void calls Service.Void) closes
// its dunning run on the real dunning service: one StopCollection at Stripe,
// the run resolved as invoice_voided, one resolved timeline row and one
// dunning.resolved. A void the invoice service refuses (a charge holds the
// invoice) stops nothing and leaves the run open: the invoice is still
// collectible.
func TestInvoiceVoid_ClosesTheRunOnce_ARefusedVoidLeavesItOpen(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Invoice Void Closes Run")

	dstore := dunning.NewPostgresStore(db)
	dstore.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	invStore := invoice.NewPostgresStore(db)

	type rig struct {
		inv     domain.Invoice
		run     domain.InvoiceDunningRun
		stopper *countingStopper
		svc     *invoice.Service
	}
	newRig := func(t *testing.T, tag string) rig {
		t.Helper()
		cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
			ExternalID: "cus_void_closes_" + tag, DisplayName: "Void Closes " + tag,
		})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		dsvc := dunning.NewService(dstore, nil, clock.Real())
		policy, err := dsvc.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
			Name: "void-closes-" + tag, Enabled: true, RetryDays: []int{3, 3, 3},
			FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible,
		})
		if err != nil {
			t.Fatalf("upsert policy: %v", err)
		}
		if err := dsvc.SetDefaultPolicy(ctx, tenantID, policy.ID); err != nil {
			t.Fatalf("set default policy: %v", err)
		}
		stopper := &countingStopper{}
		invSvc := invoice.NewService(invStore, clock.Real(), tenant.NewSettingsStore(db))
		invSvc.SetCollectionStopper(stopper)
		invSvc.SetDunningResolver(dsvc)
		inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: "INV-VOID-CLOSES-" + tag,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour), BillingPeriodEnd: time.Now().UTC(),
		}, nil)
		if err != nil {
			t.Fatalf("create invoice: %v", err)
		}
		run, err := dsvc.StartDunning(clock.WithRealTime(ctx), tenantID, inv.ID, cust.ID, domain.DunningCausePaymentFailed)
		if err != nil || run.State != domain.DunningActive {
			t.Fatalf("start dunning: state %s, %v", run.State, err)
		}
		return rig{inv: inv, run: run, stopper: stopper, svc: invSvc}
	}
	resolvedWebhooks := func(t *testing.T, runID string) int {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM webhook_outbox WHERE event_type = $1 AND payload->'dunning_run'->>'id' = $2`,
			domain.EventDunningResolved, runID).Scan(&n); err != nil {
			t.Fatalf("count dunning.resolved outbox rows: %v", err)
		}
		return n
	}
	resolvedEvents := func(t *testing.T, runID string) int {
		t.Helper()
		evs, err := dstore.ListEvents(ctx, tenantID, runID)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		n := 0
		for _, e := range evs {
			if e.EventType == domain.DunningEventResolved {
				n++
			}
		}
		return n
	}
	// The door decides a void at its invoice's time: real time here.
	opCtx := clock.WithRealTime(ctx)

	t.Run("void succeeds: one stop, the run resolved once", func(t *testing.T) {
		r := newRig(t, "OK")
		if _, err := r.svc.Void(opCtx, tenantID, r.inv.ID); err != nil {
			t.Fatalf("void: %v", err)
		}
		if len(r.stopper.stopped) != 1 || r.stopper.stopped[0] != r.inv.ID {
			t.Errorf("StopCollection calls = %v, want exactly one for %s", r.stopper.stopped, r.inv.ID)
		}
		run, err := dstore.GetRun(ctx, tenantID, r.run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionInvoiceVoided {
			t.Errorf("run state=%s resolution=%q, want resolved/invoice_voided", run.State, run.Resolution)
		}
		if n := resolvedEvents(t, r.run.ID); n != 1 {
			t.Errorf("resolved timeline events = %d, want 1", n)
		}
		if n := resolvedWebhooks(t, r.run.ID); n != 1 {
			t.Errorf("dunning.resolved events = %d, want 1", n)
		}
	})

	t.Run("void refused: nothing stopped, run stays open", func(t *testing.T) {
		r := newRig(t, "HELD")
		// A dunning retry's charge between its claim and its outcome.
		if h, err := invStore.ClaimChargeForManualCollect(ctx, tenantID, r.inv.ID); err != nil || !h.Held() {
			t.Fatalf("claim: held=%v %v", h.Held(), err)
		}
		if _, err := r.svc.Void(opCtx, tenantID, r.inv.ID); err == nil {
			t.Fatal("void while a charge holds the invoice: want refused")
		}
		if len(r.stopper.stopped) != 0 {
			t.Errorf("StopCollection ran on a refused void: %v", r.stopper.stopped)
		}
		run, err := dstore.GetRun(ctx, tenantID, r.run.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.State != domain.DunningActive {
			t.Errorf("run state=%s, want active: the invoice is still collectible", run.State)
		}
		if n := resolvedWebhooks(t, r.run.ID); n != 0 {
			t.Errorf("dunning.resolved events = %d, want 0", n)
		}
	})
}
