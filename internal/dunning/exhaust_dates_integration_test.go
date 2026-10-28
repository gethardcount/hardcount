package dunning_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// oneClockResolver resolves every pin to the one test clock this test puts
// all its entities on, reading frozen_time from the database each time: the
// answer billing.Engine's resolvers give for a pinned entity.
type oneClockResolver struct {
	clocks   *testclock.PostgresStore
	tenantID string
	clockID  string
}

func (r oneClockResolver) sim(ctx context.Context) (clock.Sim, error) {
	tc, err := r.clocks.Get(ctx, r.tenantID, r.clockID)
	if err != nil {
		return clock.Sim{}, err
	}
	return clock.Sim{At: tc.FrozenTime, TestClockID: r.clockID}, nil
}

func (r oneClockResolver) SimForCustomer(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx)
}
func (r oneClockResolver) SimForSubscription(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx)
}
func (r oneClockResolver) SimForInvoice(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx)
}

// declinedRetry is the card charge on every retry: declined, recorded on the
// invoice as the charge path records a decline with no PaymentIntent.
type declinedRetry struct {
	invoices *invoice.PostgresStore
	calls    []time.Time
}

func (d *declinedRetry) RetryPayment(ctx context.Context, tenantID, invoiceID, _ string) error {
	d.calls = append(d.calls, clock.Now(ctx))
	inv, err := d.invoices.Get(ctx, tenantID, invoiceID)
	if err != nil {
		return err
	}
	if _, _, err := d.invoices.StampChargeOutcome(ctx, tenantID, invoiceID, inv.ChargeAttemptSeq, domain.PaymentFailed, "", "card_declined"); err != nil {
		return err
	}
	return errors.New("card_declined: your card was declined")
}

// TestExhaust_TestClock_FinalActionsDatedAtTheEscalation is P-5: when a
// dunning run on a test clock runs out of retries, the final actions are
// dated with the escalation, not at the instant the advance landed on. The
// clock is advanced to two days past the final retry's slot; the
// subscription's pause or cancel row, its canceled_at, the invoice's
// uncollectible_at and the escalated event must all read the escalation's
// instant.
//
// Before the fix every nested service re-bound ctx to the clock's frozen_time:
// exhaustRun's own bind reset both ways in (inline after the final retry, and
// exhaust-on-entry), then the subscription mover and the write-off bound again.
// Only the escalated event, which takes its stamp explicitly, was right.
//
// Arms: the final retry declines and exhausts inline (stamp = the retry slot);
// exhaust-on-entry after an interrupted exhaustion (next_action_at is the
// lease, stamp = last_attempt_at); a lowered max_retry (stamp = the run's due
// slot); and cancel instead of pause.
//
// The renewed arms bound it: the advance's cycle phase already moved the
// subscription into a period that began after the slot (the catchup bills
// before it duns). The pause or cancel then lands at that period's start,
// never before it; the write-off and the escalated event keep the slot. A
// cancel dated before current_period_start bills no final invoice and gives
// the period's prebill no relief (internal/api
// TestDunningFinalAction_TestClock_NeverBeforeTheCurrentPeriod covers that
// with the engine wired).
func TestExhaust_TestClock_FinalActionsDatedAtTheEscalation(t *testing.T) {
	slot := time.Date(2027, 4, 10, 6, 0, 0, 0, time.UTC) // the run's due instant
	frozen := slot.Add(48 * time.Hour)                   // the advance target
	last := slot.Add(-72 * time.Hour)

	cases := []struct {
		name         string
		sub          domain.DunningSubscriptionAction
		attemptsUsed int
		lastAttempt  time.Time
		nextAction   time.Time
		wantStamp    time.Time
		wantRetries  int
		renewed      bool // the current period began a day after the slot
	}{
		{"inline after the final retry, pause", domain.SubActionPause, 1, last, slot, slot, 1, false},
		{"inline after the final retry, cancel", domain.SubActionCancel, 1, last, slot, slot, 1, false},
		// The final retry ran at slot and wrote the lease; the process died
		// before the final actions. The next advance re-drives them.
		{"exhaust on entry after an interrupted exhaustion", domain.SubActionPause, 2, slot, slot.Add(15 * time.Minute), slot, 0, false},
		// The policy's max_retry was lowered under a run already at it.
		{"exhaust on entry after max_retry was lowered", domain.SubActionPause, 2, last, slot, slot, 0, false},
		{"inline after the final retry, renewed since, pause", domain.SubActionPause, 1, last, slot, slot, 1, true},
		{"inline after the final retry, renewed since, cancel", domain.SubActionCancel, 1, last, slot, slot, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			ctx := postgres.WithLivemode(context.Background(), false)
			tenantID := testutil.CreateTestTenant(t, db, "Exhaust Dates")
			admin := testutil.AdminPool(t)

			clocks := testclock.NewPostgresStore(db)
			clockID := postgres.NewID("hc_tclk")
			if _, err := admin.Exec(`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
				VALUES ($1, $2, false, 'exhaust dates', $3, 'advancing')`, clockID, tenantID, frozen); err != nil {
				t.Fatalf("insert clock: %v", err)
			}
			resolver := oneClockResolver{clocks: clocks, tenantID: tenantID, clockID: clockID}

			cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
				ExternalID: "cus_exhaust_dates", DisplayName: "Exhaust Dates", TestClockID: clockID,
			})
			if err != nil {
				t.Fatalf("create customer: %v", err)
			}
			plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
				Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
				Status: domain.PlanActive, BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
			})
			if err != nil {
				t.Fatalf("create plan: %v", err)
			}
			// The current period runs past the advance, so the cancel finds
			// no ended period to bill first. The invoice in dunning bills it,
			// or, renewed, the period before it.
			periodStart := slot.Add(-10 * 24 * time.Hour)
			if tc.renewed {
				periodStart = slot.Add(24 * time.Hour)
			}
			periodEnd := periodStart.AddDate(0, 1, 0)
			invStart, invEnd := periodStart, periodEnd
			if tc.renewed {
				invStart, invEnd = periodStart.AddDate(0, -1, 0), periodStart
			}
			// The subscription's action: at the escalation, but never before
			// the current period.
			wantSub := tc.wantStamp
			if tc.renewed {
				wantSub = periodStart
			}
			subStore := subscription.NewPostgresStore(db)
			sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
				Code: "sub-exhaust-dates", DisplayName: "Exhaust Dates", CustomerID: cust.ID, TestClockID: clockID,
				Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
			})
			if err != nil {
				t.Fatalf("create subscription: %v", err)
			}
			subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)

			invoices := invoice.NewPostgresStore(db)
			inv, err := invoices.Create(ctx, tenantID, domain.Invoice{
				CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: "INV-EXHAUST-DATES",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
				SubtotalCents: 4900, TotalAmountCents: 4900, AmountDueCents: 4900, IsSimulated: true,
				BillingPeriodStart: invStart, BillingPeriodEnd: invEnd, IssuedAt: &invStart,
			})
			if err != nil {
				t.Fatalf("create invoice: %v", err)
			}

			retrier := &declinedRetry{invoices: invoices}
			runs := dunning.NewPostgresStore(db)
			svc := dunning.NewService(runs, retrier, nil)
			svc.SetResolver(resolver)
			policy, err := svc.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
				Name: "two retries", Enabled: true, RetryDays: []int{3, 3},
				FinalSubscriptionAction: tc.sub, FinalInvoiceAction: domain.InvActionMarkUncollectible,
			})
			if err != nil {
				t.Fatalf("upsert policy: %v", err)
			}
			runID := "run_" + inv.ID
			tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			// Trigger 0021 sets livemode from the GUC; pin it to test mode.
			if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
				postgres.Rollback(tx)
				t.Fatalf("pin livemode GUC: %v", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason,
					attempt_count, last_attempt_at, next_action_at, paused, created_at, updated_at, livemode)
				VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', $6, $7, $8, false, $9, $9, false)`,
				runID, tenantID, inv.ID, cust.ID, policy.ID, tc.attemptsUsed, tc.lastAttempt, tc.nextAction, invStart); err != nil {
				postgres.Rollback(tx)
				t.Fatalf("insert run: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit run: %v", err)
			}

			subSvc := subscription.NewService(subStore, clock.Real())
			subSvc.SetResolver(resolver)
			subSvc.SetAuditLogger(audit.NewLogger(db))
			invSvc := invoice.NewService(invoices, clock.Real(), tenant.NewSettingsStore(db))
			invSvc.SetResolver(resolver)
			svc.SetSubscriptionPauser(dunningMovers{svc: subSvc}, invoices)
			svc.SetSubscriptionCanceler(dunningMovers{svc: subSvc})
			svc.SetSubscriptionStateReader(dunningMovers{svc: subSvc})
			svc.SetInvoiceUncollectibleMarker(writeOff{svc: invSvc})
			svc.SetChargeHoldClaimer(invoices)

			// The catchup's dunning phase, on the ctx the catchup binds.
			simCtx := clock.WithSim(ctx, clock.Sim{At: frozen, TestClockID: clockID})
			if _, errs := svc.ProcessDueRunsForClock(simCtx, tenantID, clockID, frozen, 20); len(errs) != 0 {
				t.Fatalf("dunning catchup: %v", errs)
			}

			if len(retrier.calls) != tc.wantRetries {
				t.Fatalf("retries %v, want %d", retrier.calls, tc.wantRetries)
			}
			run, err := runs.GetRun(ctx, tenantID, runID)
			if err != nil {
				t.Fatalf("read run: %v", err)
			}
			if run.State != domain.DunningEscalated {
				t.Fatalf("run state = %s, want escalated", run.State)
			}
			events, err := runs.ListEvents(ctx, tenantID, runID)
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			escalated := 0
			for _, e := range events {
				if e.EventType == domain.DunningEventEscalated {
					escalated++
					wantInstant(t, "escalated event created_at", e.CreatedAt, tc.wantStamp)
				}
			}
			if escalated != 1 {
				t.Errorf("%d escalated events, want 1", escalated)
			}

			got, err := invoices.Get(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("read invoice: %v", err)
			}
			if got.Status != domain.InvoiceUncollectible || got.UncollectibleAt == nil {
				t.Fatalf("invoice status=%s uncollectible_at=%v, want written off", got.Status, got.UncollectibleAt)
			}
			wantInstant(t, "invoice uncollectible_at", *got.UncollectibleAt, tc.wantStamp)

			action, auditAction := domain.SubAuditCollectionPaused, domain.AuditActionUpdate
			if tc.sub == domain.SubActionCancel {
				action, auditAction = "", domain.AuditActionCancel
				s, err := subStore.Get(ctx, tenantID, sub.ID)
				if err != nil {
					t.Fatalf("read subscription: %v", err)
				}
				if s.CanceledAt == nil {
					t.Fatalf("subscription is %s with no canceled_at, want canceled", s.Status)
				}
				wantInstant(t, "subscription canceled_at", *s.CanceledAt, wantSub)
			}
			wantInstant(t, "subscription "+string(auditAction)+" audit row sim_effective_at", subAuditSimAt(t, admin, sub.ID, auditAction, action), wantSub)
		})
	}
}

// subAuditSimAt is the sim_effective_at of the subscription's one audit row
// with this action (and metadata action, when set).
func subAuditSimAt(t *testing.T, admin *sql.DB, subID, action, metaAction string) time.Time {
	t.Helper()
	q := `SELECT sim_effective_at FROM audit_log WHERE resource_type = 'subscription' AND resource_id = $1 AND action = $2`
	args := []any{subID, action}
	if metaAction != "" {
		q += ` AND metadata->>'action' = $3`
		args = append(args, metaAction)
	}
	rows, err := admin.Query(q, args...)
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
		t.Fatalf("subscription %s audit rows %q/%q: %v, want one with a sim_effective_at", subID, action, metaAction, found)
	}
	return found[0].Time
}

func wantInstant(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v, want %v (off by %v)", what, got.UTC(), want, got.Sub(want))
	}
}
