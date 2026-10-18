package dunning_test

import (
	"context"
	"errors"
	"testing"
	"time"

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
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Dunning's final action must never fire while a payment on the invoice may
// still take money (#976 PR 2, ADR-112 amendment 2026-10-19). Canceling a
// subscription cannot be undone, and canceling one whose payment then succeeds
// is the outcome this pins against.
//
// Everything here is real Postgres and real services: the dunning store, the
// invoice store and service, and subscription.Service canceling or pausing a
// real subscription. Only the card charge is faked, because the question is
// what dunning does with the invoice the charge leaves behind.
//
// The payment goes in flight between the final retry and its final action:
// the stale-precondition gap the late re-check exists for (playbook class D,
// C4). The pickers already skip an invoice that is processing or unknown, so
// these are the two ways it can reach the final action today: inline after the
// final retry, and exhaust-on-entry after the listing.

// dunningMovers bridges a real subscription.Service to dunning's three
// subscription ports, the same calls internal/api/adapters.go makes.
type dunningMovers struct{ svc *subscription.Service }

func (m dunningMovers) PauseCollection(ctx context.Context, tenantID, id string) error {
	_, err := m.svc.PauseCollection(ctx, tenantID, id, subscription.PauseCollectionInput{
		Behavior: domain.PauseCollectionKeepAsDraft, TriggeredBy: "dunning",
	})
	return err
}

func (m dunningMovers) Cancel(ctx context.Context, tenantID, id string) error {
	_, _, err := m.svc.Cancel(subscription.WithCancelOrigin(ctx, "dunning"), tenantID, id)
	return err
}

func (m dunningMovers) GetTerminalState(ctx context.Context, tenantID, id string) (dunning.SubscriptionTerminalState, error) {
	sub, err := m.svc.Get(ctx, tenantID, id)
	if err != nil {
		return dunning.SubscriptionTerminalState{}, err
	}
	return dunning.SubscriptionTerminalState{
		Terminated: sub.Status == domain.SubscriptionCanceled || sub.Status == domain.SubscriptionArchived,
	}, nil
}

// writeOff bridges invoice.Service to dunning's write-off port.
type writeOff struct{ svc *invoice.Service }

func (w writeOff) MarkUncollectible(ctx context.Context, tenantID, id string) error {
	_, err := w.svc.MarkUncollectible(ctx, tenantID, id)
	return err
}

// paymentShape puts the invoice into one payment state through the real
// outcome writer, at the invoice's current attempt counter.
type paymentShape struct {
	name     string
	inFlight bool
	apply    func(t *testing.T, ctx context.Context, f *exhaustFixture)
}

func stamp(status domain.InvoicePaymentStatus, pi, lastErr string) func(*testing.T, context.Context, *exhaustFixture) {
	return func(t *testing.T, ctx context.Context, f *exhaustFixture) {
		t.Helper()
		inv, err := f.invoices.Get(ctx, f.tenantID, f.invoiceID)
		if err != nil {
			t.Fatalf("read invoice: %v", err)
		}
		if _, applied, err := f.invoices.StampChargeOutcome(ctx, f.tenantID, f.invoiceID, inv.ChargeAttemptSeq, status, pi, lastErr); err != nil || !applied {
			t.Fatalf("stamp %s: applied=%v err=%v", status, applied, err)
		}
	}
}

var paymentShapes = []paymentShape{
	{name: "processing", inFlight: true, apply: stamp(domain.PaymentProcessing, "pi_racing", "")},
	{name: "unknown with a PaymentIntent", inFlight: true, apply: stamp(domain.PaymentUnknown, "pi_racing", "stripe: 500")},
	{name: "parked", inFlight: true, apply: stamp(domain.PaymentUnknown, "", "stripe: request timed out")},
	// The control: the payment definitely failed, so the final action fires.
	{name: "failed (control)", inFlight: false, apply: func(*testing.T, context.Context, *exhaustFixture) {}},
}

type finalActions struct {
	name string
	sub  domain.DunningSubscriptionAction
	inv  domain.DunningInvoiceAction
}

var policyActions = []finalActions{
	{name: "cancel and write off", sub: domain.SubActionCancel, inv: domain.InvActionMarkUncollectible},
	{name: "pause", sub: domain.SubActionPause, inv: domain.InvActionNone},
}

type exhaustFixture struct {
	db        *postgres.DB
	ctx       context.Context
	clk       *clock.Fake
	tenantID  string
	subID     string
	invoiceID string
	runID     string
	invoices  *invoice.PostgresStore
	invSvc    *invoice.Service
	subs      *subscription.Service
	runs      *dunning.PostgresStore
	svc       *dunning.Service
}

// newExhaustFixture builds one tenant with a real active subscription, its
// failed invoice, and a dunning run one retry from the end (attemptsUsed=1 of
// 2) or already out of retries (attemptsUsed=2), due an hour ago.
func newExhaustFixture(t *testing.T, db *postgres.DB, name string, acts finalActions, attemptsUsed int, store func(dunning.Store) dunning.Store, retrier dunning.PaymentRetrier) *exhaustFixture {
	t.Helper()
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, name)
	now := time.Now().UTC().Truncate(time.Second)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_inflight", DisplayName: "In Flight"})
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
	// The current period runs well past now, so neither mover finds an
	// ended period to bill first.
	periodStart := now.Add(-24 * time.Hour)
	periodEnd := periodStart.AddDate(0, 1, 0)
	subStore := subscription.NewPostgresStore(db)
	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-inflight", DisplayName: "In Flight", CustomerID: cust.ID,
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
		CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: "INV-INFLIGHT",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 4900, TotalAmountCents: 4900, AmountDueCents: 4900,
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd, IssuedAt: &periodStart,
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	clk := clock.NewFake(now)
	runs := dunning.NewPostgresStore(db)
	var st dunning.Store = runs
	if store != nil {
		st = store(runs)
	}
	svc := dunning.NewService(st, retrier, clk)
	policy, err := svc.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "two retries", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 2,
		FinalSubscriptionAction: acts.sub, FinalInvoiceAction: acts.inv, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	runID := "run_" + inv.ID
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	// Trigger 0021 sets livemode from the GUC; pin it to test mode.
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("pin livemode GUC: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason,
			attempt_count, last_attempt_at, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', $6, $7, $8, false, $9, $9, false)`,
		runID, tenantID, inv.ID, cust.ID, policy.ID, attemptsUsed,
		now.Add(-2*time.Hour), now.Add(-time.Hour), now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit run: %v", err)
	}

	subSvc := subscription.NewService(subStore, clock.Real())
	invSvc := invoice.NewService(invoices, clock.Real(), tenant.NewSettingsStore(db))
	svc.SetSubscriptionPauser(dunningMovers{svc: subSvc}, invoices)
	svc.SetSubscriptionCanceler(dunningMovers{svc: subSvc})
	svc.SetSubscriptionStateReader(dunningMovers{svc: subSvc})
	svc.SetInvoiceUncollectibleMarker(writeOff{svc: invSvc})

	return &exhaustFixture{
		db: db, ctx: ctx, clk: clk, tenantID: tenantID, subID: sub.ID, invoiceID: inv.ID, runID: runID,
		invoices: invoices, invSvc: invSvc, subs: subSvc, runs: runs, svc: svc,
	}
}

func (f *exhaustFixture) tick(t *testing.T) {
	t.Helper()
	if _, errs := f.svc.ProcessDueRuns(f.ctx, f.tenantID, 50); len(errs) != 0 {
		t.Fatalf("dunning tick: %v", errs)
	}
}

// assertHeld: the final action did not fire, and the run is still active and
// due again later.
func (f *exhaustFixture) assertHeld(t *testing.T) {
	t.Helper()
	sub, err := f.subs.Get(f.ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if sub.Status != domain.SubscriptionActive || sub.PauseCollection != nil {
		t.Errorf("subscription is %s (paused=%v) — dunning's final action fired while a payment on the invoice may still take money",
			sub.Status, sub.PauseCollection != nil)
	}
	inv, err := f.invoices.Get(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if inv.Status != domain.InvoiceFinalized {
		t.Errorf("invoice is %s, want finalized — written off while its payment may still go through", inv.Status)
	}
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != domain.DunningActive || run.Resolution != "" {
		t.Errorf("run state=%s resolution=%q, want active with no resolution: it must wait and be re-checked", run.State, run.Resolution)
	}
	if run.NextActionAt == nil || !run.NextActionAt.After(f.clk.Now(f.ctx)) {
		t.Errorf("run next_action_at=%v, want the lease, after now (%v): a dunning tick must pick it up again later",
			run.NextActionAt, f.clk.Now(f.ctx))
	}
	events, err := f.runs.ListEvents(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("list run events: %v", err)
	}
	for _, e := range events {
		if e.EventType == domain.DunningEventEscalated {
			t.Errorf("an escalated event was written while the run was held: %+v", e)
		}
	}
}

// assertActed: the policy's final actions fired and the run escalated.
func (f *exhaustFixture) assertActed(t *testing.T, acts finalActions) {
	t.Helper()
	sub, err := f.subs.Get(f.ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	switch acts.sub {
	case domain.SubActionCancel:
		if sub.Status != domain.SubscriptionCanceled {
			t.Errorf("subscription is %s, want canceled: the payment definitely failed, so the final action must fire", sub.Status)
		}
	case domain.SubActionPause:
		if sub.PauseCollection == nil {
			t.Errorf("subscription collection is not paused: the payment definitely failed, so the final action must fire")
		}
	}
	inv, err := f.invoices.Get(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	wantStatus := domain.InvoiceFinalized
	if acts.inv == domain.InvActionMarkUncollectible {
		wantStatus = domain.InvoiceUncollectible
	}
	if inv.Status != wantStatus {
		t.Errorf("invoice is %s, want %s", inv.Status, wantStatus)
	}
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != domain.DunningEscalated || run.Resolution != domain.ResolutionRetriesExhausted {
		t.Errorf("run state=%s resolution=%q, want escalated / retries_exhausted", run.State, run.Resolution)
	}
}

// finalRetry is the card charge on the final retry: declined. raceIn, when
// set, is a payment that reaches the invoice while that retry runs.
type finalRetry struct {
	t      *testing.T
	f      **exhaustFixture
	raceIn func(*testing.T, context.Context, *exhaustFixture)
	calls  int
}

func (r *finalRetry) RetryPayment(ctx context.Context, _, _, _ string) error {
	r.calls++
	f := *r.f
	// The retry's own decline, recorded as the charge path records one with
	// no PaymentIntent.
	stamp(domain.PaymentFailed, "", "card_declined")(r.t, ctx, f)
	if r.raceIn != nil {
		r.raceIn(r.t, ctx, f)
	}
	return errors.New("card_declined: your card was declined")
}

// TestExhaust_FinalRetry_PaymentInFlight_HoldsTheFinalAction is the inline
// way in: the final retry is declined, and by the time dunning reaches the
// final action the invoice's payment is processing, unknown or parked. Neither
// the subscription action nor the write-off fires; the run stays active and is
// re-checked. The control, a payment that definitely failed, still gets both.
func TestExhaust_FinalRetry_PaymentInFlight_HoldsTheFinalAction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, acts := range policyActions {
		for _, shape := range paymentShapes {
			t.Run(acts.name+"/"+shape.name, func(t *testing.T) {
				var f *exhaustFixture
				retry := &finalRetry{t: t, f: &f, raceIn: shape.apply}
				f = newExhaustFixture(t, db, "Inline "+acts.name+" "+shape.name, acts, 1, nil, retry)

				f.tick(t)

				if retry.calls != 1 {
					t.Fatalf("final retry charged %d times, want 1", retry.calls)
				}
				run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
				if err != nil {
					t.Fatalf("read run: %v", err)
				}
				if run.AttemptCount != 2 {
					t.Fatalf("attempt_count=%d, want 2: the final retry was declined and counts", run.AttemptCount)
				}
				if shape.inFlight {
					f.assertHeld(t)
				} else {
					f.assertActed(t, acts)
				}
			})
		}
	}
}

// claimThenPay is the real dunning store, except that a payment reaches the
// invoice right after this processor wins the exhaustion claim: after the
// listing read the invoice as failed, before the final action reads it.
type claimThenPay struct {
	dunning.Store
	t   *testing.T
	f   **exhaustFixture
	pay func(*testing.T, context.Context, *exhaustFixture)
}

func (s claimThenPay) ClaimExhaustion(ctx context.Context, tenantID, runID string, expectedAttempts int, expectedNext, lease time.Time) (bool, error) {
	won, err := s.Store.ClaimExhaustion(ctx, tenantID, runID, expectedAttempts, expectedNext, lease)
	if won && err == nil {
		s.pay(s.t, ctx, *s.f)
	}
	return won, err
}

// noRetry fails the test if dunning charges a run that is out of retries.
type noRetry struct{ t *testing.T }

func (r noRetry) RetryPayment(context.Context, string, string, string) error {
	r.t.Error("RetryPayment called on a run that is out of retries")
	return errors.New("unexpected retry")
}

// TestExhaust_OnEntry_PaymentInFlight_HoldsTheFinalAction is the other way
// in: a run already out of retries (an exhaustion whose lease expired, or an
// action_failed re-attempt) is listed, and its invoice's payment goes in
// flight before the final action reads it.
func TestExhaust_OnEntry_PaymentInFlight_HoldsTheFinalAction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, acts := range policyActions {
		for _, shape := range paymentShapes {
			t.Run(acts.name+"/"+shape.name, func(t *testing.T) {
				var f *exhaustFixture
				wrap := func(s dunning.Store) dunning.Store {
					return claimThenPay{Store: s, t: t, f: &f, pay: shape.apply}
				}
				f = newExhaustFixture(t, db, "OnEntry "+acts.name+" "+shape.name, acts, 2, wrap, noRetry{t: t})

				f.tick(t)

				if shape.inFlight {
					f.assertHeld(t)
				} else {
					f.assertActed(t, acts)
				}
			})
		}
	}
}

// TestExhaust_HeldRun_ReachesAnEnd follows a held run to each way out, so the
// hold can never strand it: the payment fails (the final actions fire), the
// payment succeeds (the run resolves as recovered, the subscription untouched),
// or a parked invoice is written off by an operator (the run closes as not
// collectible, without the subscription action — the runbook line).
func TestExhaust_HeldRun_ReachesAnEnd(t *testing.T) {
	db := testutil.SetupTestDB(t)
	acts := policyActions[0] // cancel and write off: the irreversible one

	hold := func(t *testing.T, name string, shape func(*testing.T, context.Context, *exhaustFixture)) *exhaustFixture {
		t.Helper()
		var f *exhaustFixture
		retry := &finalRetry{t: t, f: &f, raceIn: shape}
		f = newExhaustFixture(t, db, name, acts, 1, nil, retry)
		f.tick(t)
		f.assertHeld(t)
		// While the lease runs the run is not due; the next tick changes nothing.
		f.tick(t)
		f.assertHeld(t)
		f.clk.Advance(16 * time.Minute) // past the 15-minute exhaustion lease
		return f
	}

	t.Run("payment fails, then the final actions fire", func(t *testing.T) {
		f := hold(t, "Held then failed", stamp(domain.PaymentProcessing, "pi_racing", ""))
		// Still in flight past the lease: the pickers skip it, nothing fires.
		f.tick(t)
		f.assertHeld(t)

		stamp(domain.PaymentFailed, "", "card_declined")(t, f.ctx, f)
		f.tick(t)
		f.assertActed(t, acts)
	})

	t.Run("payment succeeds, then the run resolves as recovered", func(t *testing.T) {
		f := hold(t, "Held then paid", stamp(domain.PaymentProcessing, "pi_racing", ""))
		if _, err := f.invoices.MarkPaid(f.ctx, f.tenantID, f.invoiceID, "pi_racing", f.clk.Now(f.ctx)); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		f.tick(t)

		run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
			t.Errorf("run state=%s resolution=%q, want resolved / payment_recovered", run.State, run.Resolution)
		}
		sub, err := f.subs.Get(f.ctx, f.tenantID, f.subID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.Status != domain.SubscriptionActive {
			t.Errorf("subscription is %s, want active: the customer paid", sub.Status)
		}
	})

	t.Run("parked, written off by an operator, the run closes without the subscription action", func(t *testing.T) {
		f := hold(t, "Held then written off", stamp(domain.PaymentUnknown, "", "stripe: request timed out"))
		if _, err := f.invSvc.MarkUncollectible(f.ctx, f.tenantID, f.invoiceID); err != nil {
			t.Fatalf("operator write-off of a parked invoice: %v", err)
		}
		f.tick(t)

		run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionInvoiceNotCollectible {
			t.Errorf("run state=%s resolution=%q, want resolved / invoice_not_collectible", run.State, run.Resolution)
		}
		sub, err := f.subs.Get(f.ctx, f.tenantID, f.subID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.Status != domain.SubscriptionActive {
			t.Errorf("subscription is %s, want active: after a write-off the subscription action is the operator's call (runbook)", sub.Status)
		}
	})
}
