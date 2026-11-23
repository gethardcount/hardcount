package api

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestDunningFinalAction_ReDrivenAfterARenewal_CancelsWhenItRuns: a dunning
// run's final retry declines at S and takes the exhaustion lease, and the
// process stops before the final actions. The subscription renews five
// minutes later, inside the lease, and the run is re-driven at the lease. The
// cancel is dated at the re-drive, after the renewal, so the renewal's
// prebill is relieved: at day grade none of its period was used, and it is
// voided. Real router, real Postgres; the billing and dunning ticks run on a
// fake clock.
func TestDunningFinalAction_ReDrivenAfterARenewal_CancelsWhenItRuns(t *testing.T) {
	start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	wall := clock.NewFake(start)
	r := newDatesRigOn(t, "Dunning ReDrive After Renewal", wall)
	ctx := postgres.WithLivemode(context.Background(), false)
	subID, _ := r.subWithItem("redrive", r.planID)
	dayOneID := flowStr(r.dayOne(subID), "id")

	subs := subscription.NewPostgresStore(r.db)
	sub, err := subs.Get(ctx, r.tenantID, subID)
	if err != nil || sub.CurrentBillingPeriodEnd == nil {
		t.Fatalf("read subscription: %v (period end %v)", err, sub.CurrentBillingPeriodEnd)
	}
	renewal := sub.CurrentBillingPeriodEnd.UTC()
	finalRetry := renewal.Add(-5 * time.Minute)
	redrive := finalRetry.Add(15 * time.Minute) // the exhaustion lease

	// The run as an interrupted exhaustion leaves it: its one retry ran at
	// finalRetry and took the lease, and no final action ran.
	policy, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
		Name: "one retry", Enabled: true, RetryDays: []int{3},
		FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	insertInterruptedRun(t, r, dayOneID, sub.CustomerID, policy.ID, finalRetry, start)
	admin := testutil.AdminPool(t)
	wall.Set(renewal)
	if _, errs := r.srv.BillingEngine.RunCycle(leadertest.Token(t, admin, clock.WithRealTime(ctx), leader.RoleBilling), 50, nil); len(errs) != 0 {
		t.Fatalf("billing tick at the renewal: %v", errs)
	}
	wall.Set(redrive)
	if _, errs := r.srv.DunningSvc.ProcessDueRuns(leadertest.Token(t, admin, clock.WithRealTime(ctx), leader.RoleDunning), r.tenantID, 50); len(errs) != 0 {
		t.Fatalf("dunning tick at the lease: %v", errs)
	}

	got, err := subs.Get(ctx, r.tenantID, subID)
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if got.CurrentBillingPeriodStart == nil || !got.CurrentBillingPeriodStart.Equal(renewal) {
		t.Fatalf("current period starts %v, want the renewal %v", got.CurrentBillingPeriodStart, renewal)
	}
	if got.Status != domain.SubscriptionCanceled || got.CanceledAt == nil || !got.CanceledAt.Equal(redrive) {
		t.Fatalf("subscription %s canceled_at %v, want canceled at the re-drive %v (the renewal was %v)", got.Status, got.CanceledAt, redrive, renewal)
	}
	var renewalID string
	for _, inv := range r.invoices(subID) {
		if inv["billing_reason"] == string(domain.BillingReasonSubscriptionCycle) {
			renewalID = flowStr(inv, "id")
		}
	}
	if renewalID == "" {
		t.Fatalf("no renewal invoice: %s", mustJSON(r.invoices(subID)))
	}
	renewalInv, err := invoice.NewPostgresStore(r.db).Get(ctx, r.tenantID, renewalID)
	if err != nil {
		t.Fatalf("read renewal invoice: %v", err)
	}
	if renewalInv.Status != domain.InvoiceVoided {
		t.Errorf("renewal invoice is %s (amount due %d), want voided: canceled with none of its period used", renewalInv.Status, renewalInv.AmountDueCents)
	}
}

// TestDunningFinalAction_TheFinalInvoiceIsIssuedAtTheCancel: a dunning run
// whose escalation was interrupted is re-driven when its lease falls due,
// and the cancel's final invoice is issued there, with the cancel: not at
// the escalation. The plan bills in arrears, so the cancel bills the part of
// the period used before it. Real router, real Postgres, the jobs run on a
// fake clock.
func TestDunningFinalAction_TheFinalInvoiceIsIssuedAtTheCancel(t *testing.T) {
	start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	wall := clock.NewFake(start)
	r := newDatesRigOn(t, "Dunning Final Invoice At The Cancel", wall)
	ctx := postgres.WithLivemode(context.Background(), false)
	planID := flowStr(r.call("POST", "/v1/plans", flowReq{body: map[string]any{
		"code": "dates-arrears", "name": "Dates Arrears", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 3000, "base_bill_timing": "in_arrears",
	}}).want(t, 201, "create arrears plan").body, "id")
	subID, _ := r.subWithItem("final-invoice", planID)

	subs := subscription.NewPostgresStore(r.db)
	sub, err := subs.Get(ctx, r.tenantID, subID)
	if err != nil || sub.CurrentBillingPeriodEnd == nil {
		t.Fatalf("read subscription: %v (period end %v)", err, sub.CurrentBillingPeriodEnd)
	}
	// The first period closes: its arrears invoice is the one in dunning.
	renewal := sub.CurrentBillingPeriodEnd.UTC()
	tickAt(t, r.srv, wall, r.tenantID, renewal.Add(time.Hour))
	var cycleID string
	for _, inv := range r.invoices(subID) {
		if inv["billing_reason"] == string(domain.BillingReasonSubscriptionCycle) {
			cycleID = flowStr(inv, "id")
		}
	}
	if cycleID == "" {
		t.Fatalf("no cycle invoice after the first period: %s", mustJSON(r.invoices(subID)))
	}
	// The run's last retry declines five days into the next period, and its
	// exhaustion stops there; the next tick is at its lease.
	finalRetry := renewal.Add(5 * 24 * time.Hour)
	wall.Set(finalRetry)
	policy, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
		Name: "one retry", Enabled: true, RetryDays: []int{3},
		FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	insertInterruptedRun(t, r, cycleID, sub.CustomerID, policy.ID, finalRetry, start)
	redrive := finalRetry.Add(15 * time.Minute)
	tickAt(t, r.srv, wall, r.tenantID, redrive)

	got, err := subs.Get(ctx, r.tenantID, subID)
	if err != nil || got.Status != domain.SubscriptionCanceled || got.CanceledAt == nil || !got.CanceledAt.Equal(redrive) {
		t.Fatalf("subscription %s canceled_at %v (%v); want canceled at the re-drive %v", got.Status, got.CanceledAt, err, redrive)
	}
	var finalID string
	for _, inv := range r.invoices(subID) {
		if inv["billing_reason"] == string(domain.BillingReasonSubscriptionCancel) {
			finalID = flowStr(inv, "id")
		}
	}
	if finalID == "" {
		t.Fatalf("no final invoice after the cancel: %s", mustJSON(r.invoices(subID)))
	}
	final, err := invoice.NewPostgresStore(r.db).Get(ctx, r.tenantID, finalID)
	if err != nil {
		t.Fatalf("read final invoice: %v", err)
	}
	if final.IssuedAt == nil || !final.IssuedAt.Equal(redrive) || !final.CreatedAt.Equal(redrive) {
		t.Errorf("final invoice issued %v, created %v; want both at the re-drive %v, not the escalation %v",
			final.IssuedAt, final.CreatedAt, redrive, finalRetry)
	}
}

// insertInterruptedRun writes an active dunning run for invoiceID whose last
// retry ran at escalation and took the exhaustion lease (next_action_at a
// quarter hour later), as a run whose exhaustion was interrupted leaves it.
func insertInterruptedRun(t *testing.T, r *datesRig, invoiceID, customerID, policyID string, escalation, created time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("pin livemode GUC: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason,
			attempt_count, last_attempt_at, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', 1, $6, $7, false, $8, $8, false)`,
		"run_"+invoiceID, r.tenantID, invoiceID, customerID, policyID, escalation, escalation.Add(15*time.Minute), created); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("insert run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit run: %v", err)
	}
}
