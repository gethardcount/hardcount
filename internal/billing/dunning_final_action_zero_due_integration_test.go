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
	"github.com/sagarsuperuser/velox/internal/email"
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
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// A credit note that leaves a dunned invoice with nothing due marks it paid in
// its own transaction, unless a charge hold is set (G3, loops review
// 2026-10-19). These two tests run it against dunning's final action, on real
// Postgres with the cancel and its relief wired as internal/api/router.go
// wires them: a policy that cancels the subscription and writes the invoice
// off when retries run out.

// An operator waives the invoice of a run whose final action failed and is due
// to be tried again. Before, the note left the invoice open at $0, the next
// tick's exhaust-on-entry fired the final action anyway (its hold claim has no
// amount condition), and the customer's subscription was canceled over an
// invoice that owed nothing. A cancel cannot be undone.
func TestExhaustOnEntry_AfterCreditNoteToZero_NoFinalAction(t *testing.T) {
	r := newZeroDueFinalActionRig(t, "Zero Due Exhaust On Entry")
	runID := r.dueRun(domain.ResolutionActionFailed)

	cn, err := r.cnSvc.Create(r.ctx, r.tenantID, creditnote.CreateInput{
		InvoiceID: r.day1.ID, Reason: "waived",
		Lines: []creditnote.CreditLineInput{{Description: "waive the invoice", Quantity: 1, UnitAmountCents: r.day1.AmountDueCents}},
	})
	if err != nil {
		t.Fatalf("create credit note: %v", err)
	}
	if _, err := r.cnSvc.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
		t.Fatalf("issue credit note: %v", err)
	}
	r.tick()

	if got := r.subscription(); got.Status != domain.SubscriptionActive {
		t.Errorf("subscription %s, want active: the final action ran on an invoice that owed nothing", got.Status)
	}
	if n := r.outboxEvents(domain.EventSubscriptionCanceled, "subscription_id", r.sub.ID); n != 0 {
		t.Errorf("%d subscription.canceled events queued, want none", n)
	}
	if got := r.invoice(); got.Status != domain.InvoicePaid {
		t.Errorf("invoice %s/%s with %d due, want paid (not written off)", got.Status, got.PaymentStatus, got.AmountDueCents)
	}
	if got := r.run(runID); got.State != domain.DunningResolved || got.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("run %s/%q, want resolved as payment_recovered", got.State, got.Resolution)
	}
}

// The real trigger for the hold check. Customer credits paid part of the
// invoice, and dunning's final action cancels the subscription. The cancel's
// relief note, issued under the final action's own hold, takes the invoice to
// $0. It must not mark the invoice paid: the write-off would then fail on a
// paid invoice, the run would go to action_failed and close a day later as
// payment_recovered, and the canceled customer would never get the
// escalation email. Passes before the fix too (nothing marked it paid then).
//
// Mutation seams in invoice.PostgresStore.SettleZeroDueTx: drop the
// ChargeHeldTx check, or let the hold's owner through
// (domain.OwnsChargeHold, as refuseWhileChargeHeld does) → red.
func TestDunningFinalCancel_ReliefToZeroUnderItsHold_WritesOffNotPaid(t *testing.T) {
	r := newZeroDueFinalActionRig(t, "Zero Due Final Cancel")
	if _, err := r.creditSvc.Grant(r.ctx, r.tenantID, credit.GrantInput{CustomerID: r.cust.ID, AmountCents: 4000, Description: "goodwill"}); err != nil {
		t.Fatalf("grant credit: %v", err)
	}
	if applied, err := r.creditSvc.ApplyToInvoice(r.ctx, r.tenantID, r.cust.ID, r.day1.ID, r.day1.AmountDueCents); err != nil || applied != 4000 {
		t.Fatalf("apply credit = %d, %v; want 4000 applied", applied, err)
	}
	runID := r.dueRun("")

	r.tick()

	if got := r.subscription(); got.Status != domain.SubscriptionCanceled {
		t.Fatalf("subscription %s, want canceled by the final action", got.Status)
	}
	inv := r.invoice()
	if inv.Status != domain.InvoiceUncollectible || inv.AmountDueCents != 0 {
		t.Errorf("invoice %s/%s with %d due, want written off with nothing due (the relief cleared it)", inv.Status, inv.PaymentStatus, inv.AmountDueCents)
	}
	if n := r.outboxEvents(domain.EventInvoicePaid, "invoice_id", r.day1.ID); n != 0 {
		t.Errorf("%d invoice.paid events queued, want none: the relief was issued under the final action's hold", n)
	}
	if got := r.run(runID); got.State != domain.DunningEscalated || got.Resolution != domain.ResolutionRetriesExhausted {
		t.Errorf("run %s/%q, want escalated as retries_exhausted", got.State, got.Resolution)
	}
	if n := r.escalationEmails(); n != 1 {
		t.Errorf("%d escalation emails queued, want 1", n)
	}
}

type zeroDueFinalActionRig struct {
	t         *testing.T
	ctx       context.Context
	db        *postgres.DB
	tenantID  string
	cust      domain.Customer
	sub       domain.Subscription
	day1      domain.Invoice
	subStore  *subscription.PostgresStore
	invStore  *invoice.PostgresStore
	creditSvc *credit.Service
	cnSvc     *creditnote.Service
	dstore    *dunning.PostgresStore
	dsvc      *dunning.Service
	policy    domain.DunningPolicy
}

// newZeroDueFinalActionRig is a 49.00 in-advance subscription whose period
// began 15 days ago, its unpaid day-1 invoice, and dunning with a policy of
// one retry, then cancel and write off. Events go to the real outboxes.
func newZeroDueFinalActionRig(t *testing.T, name string) *zeroDueFinalActionRig {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	r := &zeroDueFinalActionRig{t: t, ctx: ctx, db: db, tenantID: testutil.CreateTestTenant(t, db, name)}

	outbox := webhook.NewOutboxStore(db)
	pricingStore := pricing.NewPostgresStore(db)
	r.subStore = subscription.NewPostgresStore(db)
	r.subStore.SetOutboxEnqueuer(outbox)
	r.invStore = invoice.NewPostgresStore(db)
	r.invStore.SetOutboxEnqueuer(outbox)
	settings := tenant.NewSettingsStore(db)
	r.creditSvc = credit.NewService(credit.NewPostgresStore(db))
	r.cnSvc = creditnote.NewService(creditnote.NewPostgresStore(db), r.invStore, nil, &cnGrantAdapter{r.creditSvc})
	r.cnSvc.SetNumberGenerator(settings)

	customers := customer.NewPostgresStore(db)
	var err error
	r.cust, err = customers.Create(ctx, r.tenantID, domain.Customer{
		ExternalID: "cus_zero_due", DisplayName: "Zero Due", Email: "ap@zero-due.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewService(pricingStore).CreatePlan(ctx, r.tenantID, pricing.CreatePlanInput{
		Code: "pro-adv", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// The period began 15 days ago, so a cancel now leaves about half of it
	// unused: the relief is a credit note, not a void.
	periodStart := time.Now().UTC().AddDate(0, 0, -15).Truncate(time.Second)
	periodEnd := periodStart.AddDate(0, 1, 0)
	clk := clock.NewFake(periodStart.Add(time.Minute))
	e := billing.NewEngine(
		&subStoreAdapter{r.subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{r.invStore},
		r.creditSvc, settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, clk,
	)
	e.SetIntervalReader(r.subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetDunningResolver(&testDunningResolver{})
	e.SetCreditGranter(r.creditSvc)
	e.SetInvoiceVoider(invoice.NewService(r.invStore, clock.Real(), settings))
	e.SetCreditNoteAdjuster(r.cnSvc)
	e.SetCreditHeadroomReader(r.cnSvc)

	r.sub, err = r.subStore.Create(ctx, r.tenantID, domain.Subscription{
		Code: "sub-zero-due", DisplayName: "Zero Due", CustomerID: r.cust.ID,
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
	if err != nil || r.day1.Status != domain.InvoiceFinalized || r.day1.PaymentStatus == domain.PaymentSucceeded || r.day1.AmountDueCents != 4900 {
		t.Fatalf("day-1 invoice: %v status=%s payment=%s due=%d, want finalized and unpaid with 4900 due", err, r.day1.Status, r.day1.PaymentStatus, r.day1.AmountDueCents)
	}
	clk.Set(time.Now())

	subSvc := subscription.NewService(r.subStore, clock.Real())
	subSvc.SetBiller(e)
	invSvc := invoice.NewService(r.invStore, clock.Real(), settings)

	r.dstore = dunning.NewPostgresStore(db)
	r.dsvc = dunning.NewService(r.dstore, nil, clock.Real())
	movers := finalActionMovers{svc: subSvc}
	r.dsvc.SetSubscriptionPauser(movers, r.invStore)
	r.dsvc.SetSubscriptionCanceler(movers)
	r.dsvc.SetSubscriptionStateReader(movers)
	r.dsvc.SetInvoiceUncollectibleMarker(finalActionWriteOff{svc: invSvc})
	r.dsvc.SetChargeHoldClaimer(r.invStore)
	r.dsvc.SetEmailNotifier(email.NewOutboxSender(email.NewOutboxStore(db)))
	r.dsvc.SetCustomerEmailFetcher(customerEmails{customers})
	r.policy, err = r.dsvc.UpsertPolicy(ctx, r.tenantID, domain.DunningPolicy{
		Name: "cancel at the end", Enabled: true, RetrySchedule: []string{"72h"}, MaxRetryAttempts: 1,
		FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	return r
}

// dueRun is the day-1 invoice's dunning run with its one retry used and due
// now, so the next tick exhausts it. resolution is its last outcome
// (action_failed for a final action being tried again), or "" for none.
func (r *zeroDueFinalActionRig) dueRun(resolution domain.DunningResolution) string {
	r.t.Helper()
	runID := "run_" + r.day1.ID
	now := time.Now().UTC()
	r.exec(`
		INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, resolution,
			attempt_count, last_attempt_at, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, $5, 'active', 'payment_failed', NULLIF($6, ''), 1, $7, $8, false, $9, $9, false)`,
		runID, r.tenantID, r.day1.ID, r.cust.ID, r.policy.ID, string(resolution), now.Add(-30*time.Minute), now.Add(-10*time.Minute), now.Add(-time.Hour))
	return runID
}

func (r *zeroDueFinalActionRig) tick() {
	r.t.Helper()
	if _, errs := r.dsvc.ProcessDueRuns(r.ctx, r.tenantID, 50); len(errs) != 0 {
		r.t.Fatalf("dunning tick: %v", errs)
	}
}

func (r *zeroDueFinalActionRig) subscription() domain.Subscription {
	r.t.Helper()
	sub, err := r.subStore.Get(r.ctx, r.tenantID, r.sub.ID)
	if err != nil {
		r.t.Fatalf("read subscription: %v", err)
	}
	return sub
}

func (r *zeroDueFinalActionRig) invoice() domain.Invoice {
	r.t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, r.day1.ID)
	if err != nil {
		r.t.Fatalf("read invoice: %v", err)
	}
	return inv
}

func (r *zeroDueFinalActionRig) run(id string) domain.InvoiceDunningRun {
	r.t.Helper()
	run, err := r.dstore.GetRun(r.ctx, r.tenantID, id)
	if err != nil {
		r.t.Fatalf("read run: %v", err)
	}
	return run
}

// outboxEvents counts the queued webhook events of one type whose payload
// names the entity.
func (r *zeroDueFinalActionRig) outboxEvents(eventType, key, id string) int {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND payload->>($3::text) = $4`,
		r.tenantID, eventType, key, id)
}

func (r *zeroDueFinalActionRig) escalationEmails() int {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`, r.tenantID, email.TypeDunningEscalation)
}

func (r *zeroDueFinalActionRig) exec(query string, args ...any) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		r.t.Fatalf("pin livemode GUC: %v", err)
	}
	if _, err := tx.ExecContext(r.ctx, query, args...); err != nil {
		r.t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
}

func (r *zeroDueFinalActionRig) count(query string, args ...any) int {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, query, args...).Scan(&n); err != nil {
		r.t.Fatalf("count: %v", err)
	}
	return n
}

// customerEmails is dunning's customer email reader, as
// internal/api/adapters.go's customerEmailFetcherAdapter gives it.
type customerEmails struct{ store *customer.PostgresStore }

func (c customerEmails) GetCustomerEmail(ctx context.Context, tenantID, customerID string) (string, string, []string, error) {
	cust, err := c.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", nil, err
	}
	return cust.Email, cust.DisplayName, cust.AdditionalEmails, nil
}
