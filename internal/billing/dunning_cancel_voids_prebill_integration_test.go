package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// prebillVoidRig is a flat in_advance subscription whose period began an hour
// ago, with its unpaid day-1 prebill and a dunning run on it, on real
// Postgres. A cancel or plan swap now leaves the period wholly unused, so its
// relief voids the prebill (ADR-118). The engine, the subscription service,
// the invoice service and dunning are wired as internal/api/router.go wires
// them; only Stripe is a recording fake (the collection stopper).
type prebillVoidRig struct {
	t        *testing.T
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	sub      domain.Subscription
	day1     domain.Invoice
	yearly   domain.Plan
	subStore *subscription.PostgresStore
	subSvc   *subscription.Service
	invStore *invoice.PostgresStore
	dstore   *dunning.PostgresStore
	dsvc     *dunning.Service
	stopper  *recordingCollectionStopper
	events   *recordingDunningEvents
}

// recordingCollectionStopper is the Stripe leg of a void (expire the Checkout
// session, cancel the PaymentIntent), recorded.
type recordingCollectionStopper struct{ stopped []string }

func (r *recordingCollectionStopper) StopCollection(_ context.Context, _ string, inv domain.Invoice) {
	r.stopped = append(r.stopped, inv.ID)
}

// ExpireCheckoutSessions is a write-off's leg; these tests void.
func (*recordingCollectionStopper) ExpireCheckoutSessions(context.Context, string, domain.Invoice) {}

// recordingDunningEvents records the webhook events dunning sends.
type recordingDunningEvents struct{ types []string }

func (r *recordingDunningEvents) Dispatch(_ context.Context, _, eventType string, _ map[string]any) error {
	r.types = append(r.types, eventType)
	return nil
}

func (r *recordingDunningEvents) count(eventType string) int {
	n := 0
	for _, t := range r.types {
		if t == eventType {
			n++
		}
	}
	return n
}

func newPrebillVoidRig(t *testing.T, name string, policy domain.DunningPolicy) *prebillVoidRig {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	r := &prebillVoidRig{t: t, ctx: ctx, db: db, tenantID: testutil.CreateTestTenant(t, db, name)}

	pricingStore := pricing.NewPostgresStore(db)
	r.subStore = subscription.NewPostgresStore(db)
	r.invStore = invoice.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	cnSvc := creditnote.NewService(creditnote.NewPostgresStore(db), r.invStore, nil, &cnGrantAdapter{creditSvc})
	cnSvc.SetNumberGenerator(settings)

	cust, err := customer.NewPostgresStore(db).Create(ctx, r.tenantID, domain.Customer{
		ExternalID: "cus_prebill_void", DisplayName: "Prebill Void", Email: "ap@prebill-void.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plans := pricing.NewService(pricingStore)
	plan, err := plans.CreatePlan(ctx, r.tenantID, pricing.CreatePlanInput{
		Code: "pro-adv", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	r.yearly, err = plans.CreatePlan(ctx, r.tenantID, pricing.CreatePlanInput{
		Code: "pro-adv-yearly", Name: "Pro yearly", Currency: "USD", BillingInterval: domain.BillingYearly,
		BaseAmountCents: 49000, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create yearly plan: %v", err)
	}

	// The period began an hour ago, so a cancel or swap now leaves it wholly
	// unused.
	periodStart := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	periodEnd := periodStart.AddDate(0, 1, 0)

	// Dunning first: the invoice service's void resolves its runs.
	r.dstore = dunning.NewPostgresStore(db)
	r.dsvc = dunning.NewService(r.dstore, nil, clock.Real())
	r.events = &recordingDunningEvents{}
	r.dsvc.SetEventDispatcher(r.events)

	// One invoice service, as router.go has: the engine's relief void, the
	// write-off, and every void's post-commit legs.
	r.stopper = &recordingCollectionStopper{}
	invSvc := invoice.NewService(r.invStore, clock.Real(), settings)
	invSvc.SetCollectionStopper(r.stopper)
	invSvc.SetDunningResolver(r.dsvc)

	e := billing.NewEngine(
		&subStoreAdapter{r.subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{r.invStore},
		creditSvc, settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(periodStart.Add(time.Minute)),
	)
	e.SetIntervalReader(r.subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetDunningResolver(&testDunningResolver{})
	e.SetCreditGranter(creditSvc)
	e.SetInvoiceVoider(invSvc)
	e.SetCreditNoteAdjuster(cnSvc)
	e.SetCreditHeadroomReader(cnSvc)

	r.sub, err = r.subStore.Create(ctx, r.tenantID, domain.Subscription{
		Code: "sub-prebill-void", DisplayName: "Prebill Void", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, r.tenantID, r.sub.ID, periodStart, periodEnd, periodEnd, 0)
	r.sub.CurrentBillingPeriodStart = &periodStart
	r.sub.CurrentBillingPeriodEnd = &periodEnd
	r.day1, err = e.BillOnCreate(ctx, r.sub)
	if err != nil || r.day1.Status != domain.InvoiceFinalized || r.day1.PaymentStatus == domain.PaymentSucceeded {
		t.Fatalf("day-1 invoice: %v status=%s payment=%s, want finalized and unpaid", err, r.day1.Status, r.day1.PaymentStatus)
	}
	if r.sub, err = r.subStore.Get(ctx, r.tenantID, r.sub.ID); err != nil {
		t.Fatalf("re-read subscription: %v", err)
	}

	r.subSvc = subscription.NewService(r.subStore, clock.Real())
	r.subSvc.SetBiller(e)
	r.subSvc.SetPlanReader(pricingStore)

	movers := finalActionMovers{svc: r.subSvc}
	r.dsvc.SetSubscriptionPauser(movers, r.invStore)
	r.dsvc.SetSubscriptionCanceler(movers)
	r.dsvc.SetSubscriptionStateReader(movers)
	r.dsvc.SetInvoiceUncollectibleMarker(finalActionWriteOff{svc: invSvc})
	r.dsvc.SetChargeHoldClaimer(r.invStore)
	saved, err := r.dsvc.UpsertPolicy(ctx, r.tenantID, policy)
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	r.insertRun(saved.ID, policy.MaxRetryAttempts)
	return r
}

// insertRun writes the day-1 invoice's active dunning run. With attempts at
// the policy's maximum it is due now, so the next tick exhausts it; with
// fewer its next retry is 72 hours away, so no tick touches it before then.
func (r *prebillVoidRig) insertRun(policyID string, attempts int) {
	r.t.Helper()
	now := time.Now().UTC()
	attempt, next := 1, now.Add(72*time.Hour)
	if attempts <= 1 {
		next = now.Add(-10 * time.Minute)
	}
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		r.t.Fatalf("pin livemode GUC: %v", err)
	}
	if _, err := tx.ExecContext(r.ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason,
			attempt_count, last_attempt_at, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', $6, $7, $8, false, $9, $9, false)`,
		r.runID(), r.tenantID, r.day1.ID, r.sub.CustomerID, policyID, attempt, now.Add(-30*time.Minute), next, now.Add(-time.Hour)); err != nil {
		r.t.Fatalf("insert run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit run: %v", err)
	}
}

func (r *prebillVoidRig) runID() string { return "run_" + r.day1.ID }

// requireVoidedAndRunClosed: the prebill is voided, its run is resolved as
// invoice_voided with one resolved timeline row and one dunning.resolved, it
// never escalated, and collection was stopped at Stripe once for it.
func (r *prebillVoidRig) requireVoidedAndRunClosed() {
	r.t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, r.day1.ID)
	if err != nil {
		r.t.Fatalf("read invoice: %v", err)
	}
	if inv.Status != domain.InvoiceVoided {
		r.t.Fatalf("day-1 invoice is %s, want voided: nothing of the period was used and nothing paid", inv.Status)
	}
	run, err := r.dstore.GetRun(r.ctx, r.tenantID, r.runID())
	if err != nil {
		r.t.Fatalf("read run: %v", err)
	}
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionInvoiceVoided {
		r.t.Errorf("run state=%s resolution=%q, want resolved/invoice_voided: the run of a voided invoice must close when it is voided, not at its next due tick", run.State, run.Resolution)
	}
	evs, err := r.dstore.ListEvents(r.ctx, r.tenantID, r.runID())
	if err != nil {
		r.t.Fatalf("list run events: %v", err)
	}
	resolved, escalated := 0, 0
	for _, e := range evs {
		switch e.EventType {
		case domain.DunningEventResolved:
			resolved++
		case domain.DunningEventEscalated:
			escalated++
		}
	}
	if resolved != 1 || escalated != 0 {
		r.t.Errorf("run events: %d resolved, %d escalated; want 1 resolved and no escalation", resolved, escalated)
	}
	if n := r.events.count(domain.EventDunningResolved); n != 1 {
		r.t.Errorf("dunning.resolved sent %d times, want 1", n)
	}
	if n := r.events.count(domain.EventDunningEscalated); n != 0 {
		r.t.Errorf("dunning.escalated sent %d times, want 0", n)
	}
	if len(r.stopper.stopped) != 1 || r.stopper.stopped[0] != r.day1.ID {
		r.t.Errorf("StopCollection calls = %v, want one for %s: a voided invoice must not stay payable on a Checkout session", r.stopper.stopped, r.day1.ID)
	}
}

// waitingPolicy retries every 72 hours, three times: a run inserted with one
// attempt is not due for days.
func waitingPolicy() domain.DunningPolicy {
	return domain.DunningPolicy{
		Name: "three retries", Enabled: true, RetrySchedule: []string{"72h", "72h"}, MaxRetryAttempts: 3,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	}
}

// The real trigger: a flat in_advance renewal, the day-1 charge fails, dunning
// starts, and the operator cancels the same day. The cancel's relief voids the
// prebill inside the cancel's transaction (VoidUnchangedTx). Before, that void
// ran only the tax reversal and the webhook after commit: the run stayed
// active until its next due tick (days away) and a live Checkout session
// stayed payable. Every void now runs the same post-commit legs (AfterVoid).
//
// Mutation: drop ResolveByInvoice or StopCollection from AfterVoid.
func TestCancel_VoidsUnusedPrebill_ClosesItsDunningRunAndStopsCollection(t *testing.T) {
	t.Run("immediate cancel", func(t *testing.T) {
		r := newPrebillVoidRig(t, "Cancel Voids Prebill Run", waitingPolicy())
		if _, _, err := r.subSvc.Cancel(r.ctx, r.tenantID, r.sub.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		r.requireVoidedAndRunClosed()
	})

	t.Run("same-day cross-interval plan swap", func(t *testing.T) {
		r := newPrebillVoidRig(t, "Swap Voids Prebill Run", waitingPolicy())
		tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			t.Fatalf("begin swap tx: %v", err)
		}
		defer postgres.Rollback(tx)
		res, err := r.subSvc.UpdateItemTx(r.ctx, tx, r.tenantID, r.sub.ID, r.sub.Items[0].ID, subscription.UpdateItemInput{
			NewPlanID: r.yearly.ID, Immediate: true,
		})
		if err != nil {
			t.Fatalf("swap: %v", err)
		}
		if !res.OrchestratedCrossAxis {
			t.Fatal("the monthly-to-yearly swap did not take the atomic cross-interval path")
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit swap: %v", err)
		}
		// What the handler runs once the swap has committed.
		r.subSvc.FinalizeCrossIntervalSwap(r.ctx, r.tenantID, r.sub, res)
		r.requireVoidedAndRunClosed()
	})
}

// Dunning's final action cancels a subscription whose only invoice is its
// unpaid, wholly unused day-1 prebill (ADR-118: the cancel voids it).
//
// The final action takes the invoice's charge hold before the cancel
// (ADR-127), and the relief's void (VoidUnchangedTx) refuses while a hold is
// set unless the caller owns it. exhaustRun runs the cancel as the hold's
// owner (domain.WithChargeHoldOwner). Without that, the void refused its own
// owner's hold, the cancel rolled back, and the run failed its final action
// every 24 hours, forever.
//
// The void now resolves the run itself (AfterVoid), so the run ends
// resolved as invoice_voided under either final invoice action, with no
// escalation: with mark_uncollectible there is nothing left to write off;
// with none the run used to escalate over a voided invoice (ADR-118
// amendment 2026-10-22).
//
// Mutation seam: in dunning.Service.exhaustRun, pass ctx instead of subCtx to
// applySubscriptionAction → the subscription stays active and the run is
// kept for a re-attempt.
func TestDunningFinalCancel_VoidsTheUnusedPrebillUnderItsOwnHold(t *testing.T) {
	for _, action := range []domain.DunningInvoiceAction{domain.InvActionMarkUncollectible, domain.InvActionNone} {
		t.Run(string(action), func(t *testing.T) {
			r := newPrebillVoidRig(t, "Dunning Cancel Voids Prebill "+string(action), domain.DunningPolicy{
				Name: "cancel at the end", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 1,
				FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: action, GracePeriodDays: 3,
			})
			if _, errs := r.dsvc.ProcessDueRuns(r.ctx, r.tenantID, 50); len(errs) != 0 {
				t.Fatalf("dunning tick: %v", errs)
			}

			gotSub, err := r.subStore.Get(r.ctx, r.tenantID, r.sub.ID)
			if err != nil {
				t.Fatalf("read subscription: %v", err)
			}
			if gotSub.Status != domain.SubscriptionCanceled {
				t.Errorf("subscription is %s, want canceled: the final action's cancel was refused (its relief void met the final action's own hold?)", gotSub.Status)
			}
			r.requireVoidedAndRunClosed()

			var held bool
			check, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer postgres.Rollback(check)
			if err := check.QueryRowContext(r.ctx, `SELECT auto_charge_claimed_until IS NOT NULL FROM invoices WHERE id = $1`, r.day1.ID).Scan(&held); err != nil {
				t.Fatalf("read hold: %v", err)
			}
			if held {
				t.Error("the final action's charge hold was left set")
			}
		})
	}
}

// finalActionMovers is dunning's view of the subscription service, as the
// adapters in internal/api/adapters.go give it.
type finalActionMovers struct{ svc *subscription.Service }

func (m finalActionMovers) PauseCollection(ctx context.Context, tenantID, subID, invoiceID string) (bool, error) {
	return m.svc.PauseCollectionForDunning(ctx, tenantID, subID, invoiceID)
}

func (m finalActionMovers) Cancel(ctx context.Context, tenantID, id string) error {
	_, _, err := m.svc.Cancel(subscription.WithCancelOrigin(ctx, "dunning"), tenantID, id)
	return err
}

func (m finalActionMovers) GetTerminalState(ctx context.Context, tenantID, id string) (dunning.SubscriptionTerminalState, error) {
	sub, err := m.svc.Get(ctx, tenantID, id)
	if err != nil {
		return dunning.SubscriptionTerminalState{}, err
	}
	st := dunning.SubscriptionTerminalState{
		Terminated: sub.Status == domain.SubscriptionCanceled || sub.Status == domain.SubscriptionArchived,
	}
	if sub.CurrentBillingPeriodStart != nil {
		st.PeriodStart = *sub.CurrentBillingPeriodStart
	}
	return st, nil
}

type finalActionWriteOff struct{ svc *invoice.Service }

func (w finalActionWriteOff) MarkUncollectible(ctx context.Context, tenantID, id string) error {
	_, err := w.svc.MarkUncollectible(ctx, tenantID, id)
	return err
}
