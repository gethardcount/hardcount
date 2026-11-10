package billing_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// prebillVoidRig is a flat in_advance subscription whose period began an hour
// ago, with its unpaid day-1 prebill and a dunning run on it, on real
// Postgres. A cancel or plan swap now leaves the period wholly unused, so its
// relief voids the prebill (ADR-118). The engine, the subscription service,
// the invoice service and dunning are wired as internal/api/router.go wires
// them; only Stripe is a recording fake (the collection stopper), until
// declinedAtStripe puts the real one in its place.
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
	invSvc   *invoice.Service
	cn       *creditnote.Service
	dstore   *dunning.PostgresStore
	dsvc     *dunning.Service
	stopper  *recordingCollectionStopper
	engine   *billing.Engine
	// pricing is the engine's plan reader. A test sets its onGetPlan to act
	// while the engine reads a plan, as a cancel does while it prices.
	pricing *hookedPricing
}

// hookedPricing is a pricing adapter that runs onGetPlan, when set, before
// each plan read.
type hookedPricing struct {
	*pricingStoreAdapter
	onGetPlan func()
}

func (p *hookedPricing) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	if p.onGetPlan != nil {
		p.onGetPlan()
	}
	return p.pricingStoreAdapter.GetPlan(ctx, tenantID, id)
}

// recordingCollectionStopper is the Stripe leg of a void (expire the Checkout
// session, cancel the PaymentIntent), recorded.
type recordingCollectionStopper struct{ stopped []string }

func (r *recordingCollectionStopper) StopCollection(_ context.Context, _ string, inv domain.Invoice) {
	r.stopped = append(r.stopped, inv.ID)
}

// ExpireCheckoutSessions is a write-off's leg; these tests void.
func (*recordingCollectionStopper) ExpireCheckoutSessions(context.Context, string, domain.Invoice) {}

func newPrebillVoidRig(t *testing.T, name string, policy domain.DunningPolicy) *prebillVoidRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	r := &prebillVoidRig{t: t, ctx: ctx, db: db, tenantID: testutil.CreateTestTenant(t, db, name)}

	pricingStore := pricing.NewPostgresStore(db)
	r.subStore = subscription.NewPostgresStore(db)
	r.invStore = invoice.NewPostgresStore(db)
	settings := tenant.NewSettingsStore(db)
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	r.cn = creditnote.NewService(creditnote.NewPostgresStore(db), r.invStore, nil, &cnGrantAdapter{creditSvc})
	r.cn.SetNumberGenerator(settings)

	cust, err := customer.NewPostgresStore(db).Create(ctx, r.tenantID, domain.Customer{
		ExternalID: "cus_prebill_void", DisplayName: "Prebill Void", Email: "ap@prebill-void.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plans := pricing.NewService(pricingStore)
	plan, _, err := plans.CreatePlan(ctx, r.tenantID, pricing.CreatePlanInput{
		Code: "pro-adv", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 4900, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	r.yearly, _, err = plans.CreatePlan(ctx, r.tenantID, pricing.CreatePlanInput{
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
	r.dstore.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	r.dsvc = dunning.NewService(r.dstore, nil, clock.Real())

	// One invoice service, as router.go has: the engine's relief void, the
	// write-off, and every void's post-commit legs.
	r.stopper = &recordingCollectionStopper{}
	invSvc := invoice.NewService(r.invStore, clock.Real(), settings)
	invSvc.SetCollectionStopper(r.stopper)
	invSvc.SetDunningResolver(r.dsvc)
	r.invSvc = invSvc

	r.pricing = &hookedPricing{pricingStoreAdapter: &pricingStoreAdapter{pricingStore}}
	e := billing.NewEngine(
		&subStoreAdapter{r.subStore}, &usageStoreAdapter{usage.NewPostgresStore(db)},
		r.pricing, &invoiceStoreAdapter{r.invStore},
		creditSvc, settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(periodStart.Add(time.Minute)),
	)
	r.engine = e
	e.SetIntervalReader(r.subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetStripeReadiness(testStripeConnected{})
	e.SetInvoiceEmailer(&testInvoiceEmailer{})
	e.SetDunningResolver(&testDunningResolver{})
	e.SetInvoiceVoider(invSvc)
	e.SetCreditNoteAdjuster(r.cn)
	e.SetCreditHeadroomReader(r.cn)

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
	r.insertRun(saved.ID, policy.Retries())
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

// invoice reads one of the rig's invoices.
func (r *prebillVoidRig) invoice(id string) domain.Invoice {
	r.t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, id)
	if err != nil {
		r.t.Fatalf("read invoice %s: %v", id, err)
	}
	return inv
}

// makeRunDueNow moves the run's next action to a minute ago, so the next tick
// takes it.
func (r *prebillVoidRig) makeRunDueNow() {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `UPDATE invoice_dunning_runs SET next_action_at = $1 WHERE id = $2`,
		time.Now().UTC().Add(-time.Minute), r.runID()); err != nil {
		r.t.Fatalf("make the run due: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
}

// dunningEvents counts the queued webhook events of eventType about the run,
// read under the tenant's row-level security.
func (r *prebillVoidRig) dunningEvents(eventType string) int {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx,
		`SELECT count(*) FROM webhook_outbox WHERE event_type = $1 AND payload->'dunning_run'->>'id' = $2`,
		eventType, r.runID()).Scan(&n); err != nil {
		r.t.Fatalf("count %s outbox rows: %v", eventType, err)
	}
	return n
}

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
	if n := r.dunningEvents(domain.EventDunningResolved); n != 1 {
		r.t.Errorf("dunning.resolved sent %d times, want 1", n)
	}
	if n := r.dunningEvents(domain.EventDunningEscalated); n != 0 {
		r.t.Errorf("dunning.escalated sent %d times, want 0", n)
	}
	if len(r.stopper.stopped) != 1 || r.stopper.stopped[0] != r.day1.ID {
		r.t.Errorf("StopCollection calls = %v, want one for %s: a voided invoice must not stay payable on a Checkout session", r.stopper.stopped, r.day1.ID)
	}
}

// stripeKeys holds one Stripe secret key per (tenant, livemode). Any other
// lookup finds no key, as for a tenant that has not connected Stripe in that
// mode, so a lookup with the wrong tenant or mode sends nothing.
type stripeKeys map[stripeKeyFor]string

type stripeKeyFor struct {
	tenantID string
	livemode bool
}

func (k stripeKeys) GetPlaintext(_ context.Context, tenantID string, livemode bool) (tenantstripe.PlaintextSecrets, error) {
	key, ok := k[stripeKeyFor{tenantID, livemode}]
	if !ok {
		return tenantstripe.PlaintextSecrets{}, errs.ErrNotFound
	}
	return tenantstripe.PlaintextSecrets{SecretKey: key}, nil
}

// cancelingStripe answers every request as Stripe answers a PaymentIntent
// cancel, and records each request as "METHOD path Authorization".
type cancelingStripe struct {
	mu       sync.Mutex
	requests []string
}

func (s *cancelingStripe) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req.Method+" "+req.URL.Path+" "+req.Header.Get("Authorization"))
	s.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"pi_day1_declined","object":"payment_intent","status":"canceled"}`)),
		Request:    req,
	}, nil
}

func (s *cancelingStripe) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// rigStripeKey is the rig tenant's test-mode Stripe key.
const rigStripeKey = "sk_test_prebill_void"

// declinedAtStripe makes the day-1 prebill what a declined auto-charge leaves:
// payment_status failed, with the declined PaymentIntent recorded on it. It
// then puts router.go's collection stopper in place of the recording one: the
// real payment.Stripe and Stripe client, holding the tenant's test-mode key
// and sending through the returned stub.
func (r *prebillVoidRig) declinedAtStripe() *cancelingStripe {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `
		UPDATE invoices SET payment_status = 'failed', stripe_payment_intent_id = 'pi_day1_declined',
			last_payment_error = 'Your card was declined.'
		WHERE id = $1`, r.day1.ID); err != nil {
		r.t.Fatalf("stamp the decline: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}

	api := &cancelingStripe{}
	keys := stripeKeys{{r.tenantID, false}: rigStripeKey}
	client := payment.NewLiveStripeClient(payment.NewStripeClientsWithHTTPClient(keys, &http.Client{Transport: api}))
	r.invSvc.SetCollectionStopper(payment.NewStripe(client, r.invStore, payment.NewPostgresWebhookStore(r.db), nil))
	return api
}

// requireVoidedAndPaymentIntentCanceled: the prebill is voided, and Stripe
// received one request, the cancel of its declined PaymentIntent, sent with
// the tenant's key.
func (r *prebillVoidRig) requireVoidedAndPaymentIntentCanceled(api *cancelingStripe) {
	r.t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, r.day1.ID)
	if err != nil {
		r.t.Fatalf("read invoice: %v", err)
	}
	if inv.Status != domain.InvoiceVoided {
		r.t.Fatalf("day-1 invoice is %s, want voided: nothing of the period was used and nothing paid", inv.Status)
	}
	want := "POST /v1/payment_intents/pi_day1_declined/cancel Bearer " + rigStripeKey
	if got := api.seen(); len(got) != 1 || got[0] != want {
		r.t.Errorf("Stripe requests = %q, want one %q: the voided invoice's declined PaymentIntent must be canceled with its tenant's key", got, want)
	}
}

// waitingPolicy retries every 72 hours, three times: a run inserted with one
// attempt is not due for days.
func waitingPolicy() domain.DunningPolicy {
	return domain.DunningPolicy{
		Name: "three retries", Enabled: true, RetryDays: []int{3, 3, 3},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible,
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
				Name: "cancel at the end", Enabled: true, RetryDays: []int{3},
				FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: action,
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

// A void run by dunning or by the billing run cancels, at Stripe, the
// declined PaymentIntent recorded on the invoice, with the invoice's tenant's
// key. Both run on a ctx that holds livemode and no tenant, as r.ctx does. A
// test-clock catch-up does too: its ctx is context.Background() plus
// livemode. The Stripe client finds the key by the tenant on ctx, so
// payment.Stripe.StopCollection names the void's tenant itself.
//
// Mutation: delete the auth.WithTenantID line in
// payment.Stripe.StopCollection → both subtests see no Stripe request.
func TestBackgroundVoid_CancelsTheDeclinedPaymentIntentAtStripe(t *testing.T) {
	t.Run("dunning final cancel", func(t *testing.T) {
		r := newPrebillVoidRig(t, "Background Void PI Cancel: dunning", domain.DunningPolicy{
			Name: "cancel at the end", Enabled: true, RetryDays: []int{3},
			FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
		})
		api := r.declinedAtStripe()
		if _, tickErrs := r.dsvc.ProcessDueRuns(r.ctx, r.tenantID, 50); len(tickErrs) != 0 {
			t.Fatalf("dunning tick: %v", tickErrs)
		}
		r.requireVoidedAndPaymentIntentCanceled(api)
	})

	t.Run("scheduled cancel", func(t *testing.T) {
		r := newPrebillVoidRig(t, "Background Void PI Cancel: schedule", waitingPolicy())
		api := r.declinedAtStripe()
		// Due a minute ago, on the period's first day, so the relief voids the
		// wholly unused prebill.
		at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
		tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(r.ctx, `UPDATE subscriptions SET cancel_at = $1 WHERE id = $2`, at, r.sub.ID); err != nil {
			t.Fatalf("schedule cancel: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// The call the billing run makes for a due scheduled cancel.
		if err := r.subSvc.FireDueScheduledCancel(r.ctx, r.tenantID, r.sub.ID, at); err != nil {
			t.Fatalf("fire scheduled cancel: %v", err)
		}
		r.requireVoidedAndPaymentIntentCanceled(api)
	})
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
		Terminated: sub.Status == domain.SubscriptionCanceled,
	}
	if sub.CurrentBillingPeriodStart != nil {
		st.PeriodStart = *sub.CurrentBillingPeriodStart
	}
	return st, nil
}

// finalActionWriteOff bridges invoice.Service to dunning's write-off port, the
// same call internal/api/adapters.go makes: then runs in the write-off's
// transaction.
type finalActionWriteOff struct{ svc *invoice.Service }

func (w finalActionWriteOff) MarkUncollectible(ctx context.Context, tenantID, id string, then func(*sql.Tx) error) error {
	_, err := w.svc.MarkUncollectibleThen(ctx, tenantID, id, then)
	return err
}
