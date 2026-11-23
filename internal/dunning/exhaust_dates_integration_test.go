package dunning_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

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

// TestExhaust_FinalActionsDatedWhenTheyRun: when a dunning run runs out of
// retries, its final actions (the subscription's canceled_at, the invoice's
// uncollectible_at) are dated at the tick that runs them, and the escalated
// event at the escalation's instant (exhaustStampAt). Each arm ticks when its
// run is due. The two dates agree except after an interrupted exhaustion.
//
// Arms: the final retry declines and exhausts inline (both at the retry
// slot); exhaust-on-entry after an interrupted exhaustion (next_action_at is
// the lease: the actions at the lease, the escalation at last_attempt_at);
// and a lowered max_retry (both at the run's due slot).
func TestExhaust_FinalActionsDatedWhenTheyRun(t *testing.T) {
	slot := time.Date(2027, 4, 10, 6, 0, 0, 0, time.UTC) // the run's due instant
	last := slot.Add(-72 * time.Hour)

	cases := []struct {
		name         string
		attemptsUsed int
		lastAttempt  time.Time
		nextAction   time.Time // the step that runs the job, and the final actions' date
		wantStamp    time.Time // the escalated event's date
		wantRetries  int
	}{
		{"inline after the final retry", 1, last, slot, slot, 1},
		// The final retry ran at slot and wrote the lease; the process died
		// before the final actions. The tick at the lease re-drives them.
		{"exhaust on entry after an interrupted exhaustion", 2, slot, slot.Add(15 * time.Minute), slot, 0},
		// The policy's max_retry was lowered under a run already at it.
		{"exhaust on entry after max_retry was lowered", 2, last, slot, slot, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			ctx := postgres.WithLivemode(context.Background(), false)
			tenantID := testutil.CreateTestTenant(t, db, "Exhaust Dates")
			wall := clock.NewFake(tc.nextAction)

			cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
				ExternalID: "cus_exhaust_dates", DisplayName: "Exhaust Dates",
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
			// The current period runs past the step, so the cancel finds no
			// ended period to bill first. The invoice in dunning bills it.
			periodStart := slot.Add(-10 * 24 * time.Hour)
			periodEnd := periodStart.AddDate(0, 1, 0)
			invStart, invEnd := periodStart, periodEnd
			subStore := subscription.NewPostgresStore(db)
			sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
				Code: "sub-exhaust-dates", DisplayName: "Exhaust Dates", CustomerID: cust.ID,
				Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
			})
			if err != nil {
				t.Fatalf("create subscription: %v", err)
			}
			subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)

			invoices := invoice.NewPostgresStore(db)
			inv, err := invoices.CreateWithLineItems(ctx, tenantID, domain.Invoice{
				CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: "INV-EXHAUST-DATES",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
				SubtotalCents: 4900, TotalAmountCents: 4900, AmountDueCents: 4900,
				BillingPeriodStart: invStart, BillingPeriodEnd: invEnd, IssuedAt: &invStart,
			}, nil)
			if err != nil {
				t.Fatalf("create invoice: %v", err)
			}

			retrier := &declinedRetry{invoices: invoices}
			runs := dunning.NewPostgresStore(db)
			svc := dunning.NewService(runs, retrier, wall)
			policy, err := svc.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
				Name: "two retries", Enabled: true, RetryDays: []int{3, 3},
				FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
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

			subSvc := subscription.NewService(subStore, wall)
			subSvc.SetAuditLogger(audit.NewLogger(db))
			invSvc := invoice.NewService(invoices, wall, tenant.NewSettingsStore(db))
			svc.SetSubscriptionPauser(dunningMovers{svc: subSvc}, invoices)
			svc.SetSubscriptionCanceler(dunningMovers{svc: subSvc})
			svc.SetSubscriptionStateReader(dunningMovers{svc: subSvc})
			svc.SetInvoiceUncollectibleMarker(writeOff{svc: invSvc})
			svc.SetChargeHoldClaimer(invoices)

			tick := leadertest.Token(t, testutil.AdminPool(t), ctx, leader.RoleDunning)
			if _, errs := svc.ProcessDueRuns(tick, tenantID, 20); len(errs) != 0 {
				t.Fatalf("dunning tick: %v", errs)
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
			wantInstant(t, "invoice uncollectible_at", *got.UncollectibleAt, tc.nextAction)

			s, err := subStore.Get(ctx, tenantID, sub.ID)
			if err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			if s.CanceledAt == nil {
				t.Fatalf("subscription is %s with no canceled_at, want canceled", s.Status)
			}
			wantInstant(t, "subscription canceled_at", *s.CanceledAt, tc.nextAction)
		})
	}
}

func wantInstant(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v, want %v (off by %v)", what, got.UTC(), want, got.Sub(want))
	}
}
