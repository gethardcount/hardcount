package api

import (
	"context"
	"database/sql"
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
// cancel is dated at the re-drive, after the renewal, on a test clock as in
// real time, so the renewal's prebill is relieved: at day grade none of its
// period was used, and it is voided. Real router, real Postgres; the clock
// arm runs the clock's catch-up through each step to a target past the lease,
// the real-time arm runs the billing and dunning ticks at the same instants.
func TestDunningFinalAction_ReDrivenAfterARenewal_CancelsWhenItRuns(t *testing.T) {
	for _, onClock := range []bool{true, false} {
		name := "real time"
		if onClock {
			name = "test clock"
		}
		t.Run(name, func(t *testing.T) {
			start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
			wall := clock.NewFake(start)
			r := newDatesRigOn(t, "Dunning ReDrive After Renewal "+name, wall)
			ctx := postgres.WithLivemode(context.Background(), false)
			clockID := ""
			if onClock {
				clockID = r.newClock("re-drive", start)
			}
			subID, _ := r.subWithItem("redrive", clockID, r.planID)
			dayOneID := flowStr(r.dayOne(subID), "id")

			subs := subscription.NewPostgresStore(r.db)
			sub, err := subs.Get(ctx, r.tenantID, subID)
			if err != nil || sub.CurrentBillingPeriodEnd == nil {
				t.Fatalf("read subscription: %v (period end %v)", err, sub.CurrentBillingPeriodEnd)
			}
			renewal := sub.CurrentBillingPeriodEnd.UTC()
			finalRetry := renewal.Add(-5 * time.Minute)
			redrive := finalRetry.Add(15 * time.Minute) // the exhaustion lease

			// The run as an interrupted exhaustion leaves it: its one retry
			// ran at finalRetry and took the lease, and no final action ran.
			interrupted := func() {
				policy, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
					Name: "one retry", Enabled: true, RetryDays: []int{3},
					FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
				})
				if err != nil {
					t.Fatalf("upsert policy: %v", err)
				}
				insertInterruptedRun(t, r, dayOneID, sub.CustomerID, policy.ID, finalRetry, start)
			}
			if onClock {
				r.advance(clockID, finalRetry)
				interrupted()
				r.advance(clockID, redrive.Add(48*time.Hour))
			} else {
				interrupted()
				admin := testutil.AdminPool(t)
				wall.Set(renewal)
				if _, errs := r.srv.BillingEngine.RunCycle(leadertest.Token(t, admin, clock.WithRealTime(ctx), leader.RoleBilling), 50, nil); len(errs) != 0 {
					t.Fatalf("billing tick at the renewal: %v", errs)
				}
				wall.Set(redrive)
				if _, errs := r.srv.DunningSvc.ProcessDueRuns(leadertest.Token(t, admin, clock.WithRealTime(ctx), leader.RoleDunning), r.tenantID, 50); len(errs) != 0 {
					t.Fatalf("dunning tick at the lease: %v", errs)
				}
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
		})
	}
}

// TestDunningFinalAction_TestClock_TheFinalInvoiceIsIssuedAtTheCancel: a
// dunning run on a test clock whose escalation was interrupted is re-driven
// at the step its lease falls due, and the cancel's final invoice is issued
// there, with the cancel: not at the escalation, and not at the advance's
// target. The plan bills in arrears, so the cancel bills the part of the
// period used before it. Real router, real Postgres, the clock's catch-up.
func TestDunningFinalAction_TestClock_TheFinalInvoiceIsIssuedAtTheCancel(t *testing.T) {
	r := newDatesRig(t, "Dunning Final Invoice At The Cancel")
	ctx := postgres.WithLivemode(context.Background(), false)
	start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	clockID := r.newClock("final invoice", start)
	planID := flowStr(r.call("POST", "/v1/plans", flowReq{body: map[string]any{
		"code": "dates-arrears", "name": "Dates Arrears", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 3000, "base_bill_timing": "in_arrears",
	}}).want(t, 201, "create arrears plan").body, "id")
	subID, _ := r.subWithItem("final-invoice", clockID, planID)

	subs := subscription.NewPostgresStore(r.db)
	sub, err := subs.Get(ctx, r.tenantID, subID)
	if err != nil || sub.CurrentBillingPeriodEnd == nil {
		t.Fatalf("read subscription: %v (period end %v)", err, sub.CurrentBillingPeriodEnd)
	}
	// The first period closes: its arrears invoice is the one in dunning.
	renewal := sub.CurrentBillingPeriodEnd.UTC()
	r.advance(clockID, renewal.Add(time.Hour))
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
	// exhaustion stops there; the next advance passes its lease.
	finalRetry := renewal.Add(5 * 24 * time.Hour)
	r.advance(clockID, finalRetry)
	policy, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
		Name: "one retry", Enabled: true, RetryDays: []int{3},
		FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	insertInterruptedRun(t, r, cycleID, sub.CustomerID, policy.ID, finalRetry, start)
	redrive := finalRetry.Add(15 * time.Minute)
	target := redrive.Add(48 * time.Hour)
	r.advance(clockID, target)

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
		t.Errorf("final invoice issued %v, created %v; want both at the re-drive %v, not the escalation %v or the target %v",
			final.IssuedAt, final.CreatedAt, redrive, finalRetry, target)
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

// pauseRowSimAt is the sim_effective_at of the subscription's one
// collection-paused audit row.
func pauseRowSimAt(t *testing.T, admin *sql.DB, subID string) time.Time {
	t.Helper()
	rows, err := admin.Query(`SELECT sim_effective_at FROM audit_log
		WHERE resource_type = 'subscription' AND resource_id = $1 AND action = $2 AND metadata->>'action' = $3`,
		subID, domain.AuditActionUpdate, domain.SubAuditCollectionPaused)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var found []sql.NullTime
	for rows.Next() {
		var v sql.NullTime
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		found = append(found, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	if len(found) != 1 || !found[0].Valid {
		t.Fatalf("subscription %s collection-paused rows: %v, want one with a sim_effective_at", subID, found)
	}
	return found[0].Time
}

// TestDunningFinalAction_TestClock_OneAdvanceOverThreePeriods (#1276): a
// card-less subscription billed in advance, whose one dunning retry runs out
// three days after its day-1 invoice, and one advance over three renewals.
// The customer has no card on purpose: a declined card starts and runs
// dunning in the same order. The advance moves through each due time in
// order, so the final action comes at the escalation, before the first
// renewal: a cancel leaves no renewal billed, and a pause leaves the three
// renewals as drafts, never charged.
func TestDunningFinalAction_TestClock_OneAdvanceOverThreePeriods(t *testing.T) {
	for _, action := range []domain.DunningSubscriptionAction{domain.SubActionCancel, domain.SubActionPause} {
		t.Run(string(action), func(t *testing.T) {
			r := newDatesRig(t, "Dunning Three Periods "+string(action))
			ctx := postgres.WithLivemode(context.Background(), false)
			if _, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
				Name: "one retry", Enabled: true, RetryDays: []int{3},
				FinalSubscriptionAction: action, FinalInvoiceAction: domain.InvActionMarkUncollectible,
			}); err != nil {
				t.Fatalf("upsert policy: %v", err)
			}
			start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
			escalation := start.AddDate(0, 0, 3)
			clockID := r.newClock("three periods "+string(action), start)
			subID, _ := r.subWithItem("three-"+string(action), clockID, r.planID)
			r.advance(clockID, start.AddDate(0, 3, 2))

			var renewals []map[string]any
			for _, inv := range r.invoices(subID) {
				if inv["billing_reason"] == "subscription_cycle" {
					renewals = append(renewals, inv)
				}
			}
			if action == domain.SubActionCancel {
				sub, err := subscription.NewPostgresStore(r.db).Get(ctx, r.tenantID, subID)
				if err != nil {
					t.Fatalf("read subscription: %v", err)
				}
				if sub.CanceledAt == nil || !sub.CanceledAt.Equal(escalation) {
					t.Errorf("canceled_at = %v, want the escalation %v", sub.CanceledAt, escalation)
				}
				if len(renewals) != 0 {
					t.Errorf("%d renewals after a cancel at the escalation, want none: %s", len(renewals), mustJSON(renewals))
				}
				return
			}
			if got := pauseRowSimAt(t, testutil.AdminPool(t), subID); !got.Equal(escalation) {
				t.Errorf("collection paused at %v, want the escalation %v", got, escalation)
			}
			if len(renewals) != 3 {
				t.Fatalf("%d renewals, want 3: %s", len(renewals), mustJSON(renewals))
			}
			for i, inv := range renewals {
				if got := flowStr(inv, "status"); got != "draft" {
					t.Errorf("renewal %d status = %s, want draft: collection is paused", i, got)
				}
			}
		})
	}
}
