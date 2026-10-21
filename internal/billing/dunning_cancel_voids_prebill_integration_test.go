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

// Dunning's final action cancels a subscription whose only invoice is its
// unpaid, wholly unused day-1 prebill (ADR-118: the cancel voids it), on real
// Postgres with the cancel and its relief wired as internal/api/router.go
// wires them.
//
// The final action takes the invoice's charge hold before the cancel
// (ADR-127), and the relief's void (VoidUnchangedTx) refuses while a hold is
// set unless the caller owns it. exhaustRun runs the cancel as the hold's
// owner (domain.WithChargeHoldOwner). Without that, the void refused its own
// owner's hold, the cancel rolled back, and the run failed its final action
// every 24 hours, forever. No other test ran the real cancel under the hold:
// the dunning fixtures build a subscription service with no relief.
//
// Mutation seam: in dunning.Service.exhaustRun, pass ctx instead of subCtx to
// applySubscriptionAction → the subscription stays active and the run is
// kept for a re-attempt.
func TestDunningFinalCancel_VoidsTheUnusedPrebillUnderItsOwnHold(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, "Dunning Cancel Voids Prebill")

	pricingStore := pricing.NewPostgresStore(db)
	subStore := subscription.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	cnSvc := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil, &cnGrantAdapter{creditSvc})
	cnSvc.SetNumberGenerator(settings)

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_dunning_void", DisplayName: "Dunning Void", Email: "ap@dunning-void.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewService(pricingStore).CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "pro-adv", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// The period began an hour ago, so a cancel now leaves it wholly unused.
	periodStart := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	periodEnd := periodStart.AddDate(0, 1, 0)

	// The engine, with the cancel's relief wired as router.go wires it.
	e := billing.NewEngine(
		&subStoreAdapter{subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invStore},
		creditSvc, settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(periodStart.Add(time.Minute)),
	)
	e.SetIntervalReader(subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetDunningResolver(&testDunningResolver{})
	e.SetCreditGranter(creditSvc)
	e.SetInvoiceVoider(invoice.NewService(invStore, clock.Real(), settings))
	e.SetCreditNoteAdjuster(cnSvc)
	e.SetCreditHeadroomReader(cnSvc)

	sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
		Code: "sub-dunning-void", DisplayName: "Dunning Void", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
	sub.CurrentBillingPeriodStart = &periodStart
	sub.CurrentBillingPeriodEnd = &periodEnd
	day1, err := e.BillOnCreate(ctx, sub)
	if err != nil || day1.Status != domain.InvoiceFinalized || day1.PaymentStatus == domain.PaymentSucceeded {
		t.Fatalf("day-1 invoice: %v status=%s payment=%s, want finalized and unpaid", err, day1.Status, day1.PaymentStatus)
	}

	subSvc := subscription.NewService(subStore, clock.Real())
	subSvc.SetBiller(e)
	invSvc := invoice.NewService(invStore, clock.Real(), settings)

	// Dunning, wired as router.go wires it: the cancel through the
	// subscription service, the final action taking the charge hold.
	dstore := dunning.NewPostgresStore(db)
	dsvc := dunning.NewService(dstore, nil, clock.Real())
	movers := finalActionMovers{svc: subSvc}
	dsvc.SetSubscriptionPauser(movers, invStore)
	dsvc.SetSubscriptionCanceler(movers)
	dsvc.SetSubscriptionStateReader(movers)
	dsvc.SetInvoiceUncollectibleMarker(finalActionWriteOff{svc: invSvc})
	dsvc.SetChargeHoldClaimer(invStore)
	policy, err := dsvc.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "cancel at the end", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 1,
		FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	// The run has used its one retry and is due: the next tick exhausts it.
	runID := "run_" + day1.ID
	now := time.Now().UTC()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("pin livemode GUC: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason,
			attempt_count, last_attempt_at, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', 1, $6, $7, false, $8, $8, false)`,
		runID, tenantID, day1.ID, cust.ID, policy.ID, now.Add(-30*time.Minute), now.Add(-10*time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit run: %v", err)
	}

	if _, errs := dsvc.ProcessDueRuns(ctx, tenantID, 50); len(errs) != 0 {
		t.Fatalf("dunning tick: %v", errs)
	}

	gotSub, err := subStore.Get(ctx, tenantID, sub.ID)
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if gotSub.Status != domain.SubscriptionCanceled {
		t.Errorf("subscription is %s, want canceled: the final action's cancel was refused (its relief void met the final action's own hold?)", gotSub.Status)
	}
	gotInv, err := invStore.Get(ctx, tenantID, day1.ID)
	if err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if gotInv.Status != domain.InvoiceVoided {
		t.Errorf("day-1 invoice is %s, want voided: nothing of the period was used and nothing paid", gotInv.Status)
	}
	run, err := dstore.GetRun(ctx, tenantID, runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State == domain.DunningActive || run.Resolution != domain.ResolutionInvoiceVoided {
		t.Errorf("run state=%s resolution=%q, want closed as invoice_voided (not kept for a re-attempt 24 hours later)", run.State, run.Resolution)
	}
	var held bool
	check, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(check)
	if err := check.QueryRowContext(ctx, `SELECT auto_charge_claimed_until IS NOT NULL FROM invoices WHERE id = $1`, day1.ID).Scan(&held); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if held {
		t.Error("the final action's charge hold was left set")
	}
}

// finalActionMovers is dunning's view of the subscription service, as the
// adapters in internal/api/adapters.go give it.
type finalActionMovers struct{ svc *subscription.Service }

func (m finalActionMovers) PauseCollection(ctx context.Context, tenantID, id string) error {
	_, err := m.svc.PauseCollection(ctx, tenantID, id, subscription.PauseCollectionInput{
		Behavior: domain.PauseCollectionKeepAsDraft, TriggeredBy: "dunning",
	})
	return err
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
