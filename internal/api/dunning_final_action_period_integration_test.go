package api

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestDunningFinalAction_TestClock_NeverBeforeTheCurrentPeriod: a dunning
// run's final action on a test clock is dated at its escalation (P-5), but
// never before the subscription's current period. Real router, real
// Postgres, the catchup's dunning phase on the ctx the catchup binds.
//
// The shape: one advance crosses the next renewal. The catchup bills the
// renewal (its cycle phase) before it runs the dunning work due before it
// (its dunning phase), so a run whose escalation falls before the renewal
// acts on a subscription already in the new period. Here the run's final
// retry ran in an earlier advance and its exhaustion was interrupted, so this
// advance re-drives it from that retry's instant, ten days before the
// renewal.
//
// A cancel dated before current_period_start bills no final invoice and gives
// the renewal's prebill no relief: both treat the cancel as outside the
// period. The prebill, which began after the escalation, stayed open and
// unpaid on a canceled subscription. The cancel lands at the period start
// instead: nothing of the period used, its prebill voided. A pause is held to
// the same bound, so it never reads as earlier than the renewal it followed.
// The write-off of the escalated invoice keeps the escalation's instant.
func TestDunningFinalAction_TestClock_NeverBeforeTheCurrentPeriod(t *testing.T) {
	cases := []struct {
		name   string
		action domain.DunningSubscriptionAction
	}{
		{"cancel", domain.SubActionCancel},
		{"pause", domain.SubActionPause},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newDatesRig(t, "Dunning Final Action Period "+tc.name)
			ctx := postgres.WithLivemode(context.Background(), false)
			start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
			clockID := r.newClock("final action "+tc.name, start)
			subID, _ := r.subWithItem("final-"+tc.name, clockID, r.planID)
			dayOneID := flowStr(r.dayOne(subID), "id")

			subs := subscription.NewPostgresStore(r.db)
			sub, err := subs.Get(ctx, r.tenantID, subID)
			if err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			if sub.CurrentBillingPeriodEnd == nil {
				t.Fatal("subscription has no current period")
			}
			renewal := sub.CurrentBillingPeriodEnd.UTC()
			frozen := renewal.Add(48 * time.Hour)
			escalation := renewal.Add(-10 * 24 * time.Hour) // the final retry, in an earlier advance
			r.advance(clockID, frozen)

			if sub, err = subs.Get(ctx, r.tenantID, subID); err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(renewal) {
				t.Fatalf("current period starts %v after the advance, want the renewal %v", sub.CurrentBillingPeriodStart, renewal)
			}
			var renewalID string
			for _, inv := range r.invoices(subID) {
				if inv["billing_reason"] == "subscription_cycle" {
					renewalID = flowStr(inv, "id")
				}
			}
			if renewalID == "" {
				t.Fatalf("no renewal invoice after the advance: %s", mustJSON(r.invoices(subID)))
			}

			policy, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
				Name: "one retry", Enabled: true, RetryDays: []int{3},
				FinalSubscriptionAction: tc.action, FinalInvoiceAction: domain.InvActionMarkUncollectible,
			})
			if err != nil {
				t.Fatalf("upsert policy: %v", err)
			}
			// The day-1 invoice's run: its one retry ran at escalation and
			// took the exhaustion lease; the exhaustion did not finish.
			insertInterruptedRun(t, r, dayOneID, sub.CustomerID, policy.ID, escalation, start)

			// The catchup's dunning phase, as the clock's catch-up pass
			// calls it.
			simCtx := clock.WithSim(ctx, clock.Sim{At: frozen, TestClockID: clockID})
			if _, errs := r.srv.DunningSvc.ProcessDueRunsForClock(simCtx, r.tenantID, clockID, frozen, 50); len(errs) != 0 {
				t.Fatalf("dunning phase: %v", errs)
			}

			invoices := invoice.NewPostgresStore(r.db)
			dayOne, err := invoices.Get(ctx, r.tenantID, dayOneID)
			if err != nil {
				t.Fatalf("read day-1 invoice: %v", err)
			}
			if dayOne.Status != domain.InvoiceUncollectible || dayOne.UncollectibleAt == nil {
				t.Fatalf("day-1 invoice status=%s uncollectible_at=%v, want written off", dayOne.Status, dayOne.UncollectibleAt)
			}
			if !dayOne.UncollectibleAt.Equal(escalation) {
				t.Errorf("day-1 invoice uncollectible_at = %v, want the escalation %v", dayOne.UncollectibleAt.UTC(), escalation)
			}

			got, err := subs.Get(ctx, r.tenantID, subID)
			if err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			renewalInv, err := invoices.Get(ctx, r.tenantID, renewalID)
			if err != nil {
				t.Fatalf("read renewal invoice: %v", err)
			}

			if tc.action == domain.SubActionCancel {
				if got.Status != domain.SubscriptionCanceled || got.CanceledAt == nil {
					t.Fatalf("subscription status=%s canceled_at=%v, want canceled", got.Status, got.CanceledAt)
				}
				if got.CanceledAt.Before(renewal) {
					t.Errorf("canceled_at %v is before current_period_start %v: no final invoice and no relief for the period", got.CanceledAt.UTC(), renewal)
				}
				if !got.CanceledAt.Equal(renewal) {
					t.Errorf("canceled_at = %v, want the current period's start %v", got.CanceledAt.UTC(), renewal)
				}
				if renewalInv.Status != domain.InvoiceVoided {
					t.Errorf("renewal invoice %s is %s (amount due %d), want voided: the subscription was canceled with none of its period used",
						renewalID, renewalInv.Status, renewalInv.AmountDueCents)
				}
				return
			}

			if renewalInv.Status != domain.InvoiceFinalized {
				t.Errorf("renewal invoice is %s, want finalized: a pause changes no invoice", renewalInv.Status)
			}
			pausedAt := pauseRowSimAt(t, testutil.AdminPool(t), subID)
			if pausedAt.Before(renewal) {
				t.Errorf(`"Collection paused" row dated %v, before current_period_start %v: it reads as earlier than the renewal it followed`, pausedAt.UTC(), renewal)
			}
			if !pausedAt.Equal(renewal) {
				t.Errorf(`"Collection paused" row dated %v, want the current period's start %v`, pausedAt.UTC(), renewal)
			}
		})
	}
}

// TestDunningFinalAction_TestClock_TheFinalInvoiceIsIssuedAtTheEscalation: a
// dunning run on a test clock that ends in a cancel inside the subscription's
// current period issues the cancel's final invoice at the escalation, not at
// the instant the advance landed on. The plan bills in arrears, so the cancel
// bills the part of the period used before it. Real router, real Postgres,
// the catchup's dunning phase on the ctx the catchup binds.
func TestDunningFinalAction_TestClock_TheFinalInvoiceIsIssuedAtTheEscalation(t *testing.T) {
	r := newDatesRig(t, "Dunning Final Invoice At The Escalation")
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
	// The run's last retry escalates five days into the next period; the
	// advance lands two days after that.
	escalation := renewal.Add(5 * 24 * time.Hour)
	frozen := escalation.Add(48 * time.Hour)
	r.advance(clockID, frozen)

	policy, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
		Name: "one retry", Enabled: true, RetryDays: []int{3},
		FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	insertInterruptedRun(t, r, cycleID, sub.CustomerID, policy.ID, escalation, start)
	simCtx := clock.WithSim(ctx, clock.Sim{At: frozen, TestClockID: clockID})
	if _, errs := r.srv.DunningSvc.ProcessDueRunsForClock(simCtx, r.tenantID, clockID, frozen, 50); len(errs) != 0 {
		t.Fatalf("dunning phase: %v", errs)
	}

	got, err := subs.Get(ctx, r.tenantID, subID)
	if err != nil || got.Status != domain.SubscriptionCanceled || got.CanceledAt == nil || !got.CanceledAt.Equal(escalation) {
		t.Fatalf("subscription %s canceled_at %v (%v); want canceled at the escalation %v", got.Status, got.CanceledAt, err, escalation)
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
	if final.IssuedAt == nil || !final.IssuedAt.Equal(escalation) || !final.CreatedAt.Equal(escalation) {
		t.Errorf("final invoice issued %v, created %v; want both at the escalation %v, not the advance's %v",
			final.IssuedAt, final.CreatedAt, escalation, frozen)
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
// dunning in the same order. One advance closes every period before it runs
// any payment retry, so all three renewals are billed and go through their
// own dunning, and the final action is dated at the last renewal (the
// current period's start), not at the escalation.
func TestDunningFinalAction_TestClock_OneAdvanceOverThreePeriods(t *testing.T) {
	for _, tc := range []struct {
		action     domain.DunningSubscriptionAction
		lastStatus string // the third renewal's status
	}{
		{domain.SubActionCancel, "voided"}, // relieved by the cancel
		{domain.SubActionPause, "finalized"},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			r := newDatesRig(t, "Dunning Three Periods "+string(tc.action))
			ctx := postgres.WithLivemode(context.Background(), false)
			if _, err := r.srv.DunningSvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
				Name: "one retry", Enabled: true, RetryDays: []int{3},
				FinalSubscriptionAction: tc.action, FinalInvoiceAction: domain.InvActionMarkUncollectible,
			}); err != nil {
				t.Fatalf("upsert policy: %v", err)
			}
			start := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
			clockID := r.newClock("three periods "+string(tc.action), start)
			subID, _ := r.subWithItem("three-"+string(tc.action), clockID, r.planID)
			r.advance(clockID, start.AddDate(0, 3, 2))

			var renewals []map[string]any
			for _, inv := range r.invoices(subID) {
				if inv["billing_reason"] == "subscription_cycle" {
					renewals = append(renewals, inv)
				}
			}
			if len(renewals) != 3 {
				t.Fatalf("%d renewals, want 3: %s", len(renewals), mustJSON(renewals))
			}
			lastRenewal := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
			for i, want := range []string{"uncollectible", "uncollectible", tc.lastStatus} {
				wantAt(t, fmt.Sprintf("renewal %d", i), renewals[i], "issued_at", lastRenewal.AddDate(0, i-2, 0))
				if got := flowStr(renewals[i], "status"); got != want {
					t.Errorf("renewal %d status = %s, want %s", i, got, want)
				}
			}
			sub, err := subscription.NewPostgresStore(r.db).Get(ctx, r.tenantID, subID)
			if err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			if tc.action == domain.SubActionCancel {
				if sub.CanceledAt == nil || !sub.CanceledAt.Equal(lastRenewal) {
					t.Errorf("canceled_at = %v, want the last renewal %v", sub.CanceledAt, lastRenewal)
				}
				return
			}
			if got := pauseRowSimAt(t, testutil.AdminPool(t), subID); !got.Equal(lastRenewal) {
				t.Errorf("collection paused at %v, want the last renewal %v", got, lastRenewal)
			}
		})
	}
}
