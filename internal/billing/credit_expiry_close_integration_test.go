package billing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/subscription/subscriptiontest"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// ADR-071 §2 (amended 2026-10-19), end to end: a period's invoice draws on the
// credit grants that were live when the period ended, however late it is
// built or collected, and the expiry sweep does not retire such a grant
// first. The real triggers are a close that fails on billing day and waits on
// the ADR-119 retry ladder, and a close whose Stripe Tax call failed so its
// invoice is finalized hours later (#653).

// perInvoiceCharger records the amount of every charge, per invoice, and
// reports success. A charge is what the card pays after credit.
type perInvoiceCharger struct {
	mu  sync.Mutex
	got map[string][]int64
}

func (c *perInvoiceCharger) ChargeInvoice(_ context.Context, _ string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.got == nil {
		c.got = map[string][]int64{}
	}
	c.got[inv.ID] = append(c.got[inv.ID], inv.AmountDueCents)
	return inv, nil
}

func (c *perInvoiceCharger) charges(invoiceID string) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.got[invoiceID]...)
}

// gapInvoices runs beforeClaim when the collector claims an invoice: after
// the close has committed and before the credit draw. That gap is where a
// slow post-commit step (CommitTax is a Stripe call) leaves room for a sweep.
type gapInvoices struct {
	*hookedInvoices
	beforeClaim func(invoiceID string)
}

func (g *gapInvoices) ClaimAutoCharge(ctx context.Context, tenantID, id string) (domain.ChargeHold, error) {
	if g.beforeClaim != nil {
		g.beforeClaim(id)
	}
	return g.hookedInvoices.ClaimAutoCharge(ctx, tenantID, id)
}

// fixedTaxResolver resolves every tenant to one provider.
type fixedTaxResolver struct{ p tax.Provider }

func (r fixedTaxResolver) Resolve(context.Context, domain.TenantSettings) (tax.Provider, error) {
	return r.p, nil
}

// outageTax fails every calculation while down, as Stripe Tax does in an
// outage, and computes no tax once it is back.
type outageTax struct {
	tax.Provider
	down atomic.Bool
}

func (p *outageTax) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	if p.down.Load() {
		return nil, errors.New("tax provider unavailable (test)")
	}
	return p.Provider.Calculate(ctx, req)
}

// expiryEnv is the billing-failure fixture (a real engine on a fake clock at
// two seconds past the March period's end) with the real credit ledger wired
// into the engine, a card on file for every customer, and a recording
// charger.
type expiryEnv struct {
	*billFailureEnv
	credits *credit.Service
	charger *perInvoiceCharger
	gap     *gapInvoices
	eng     *billing.Engine
}

func newExpiryEnv(t *testing.T, name string, taxProvider tax.Provider) *expiryEnv {
	t.Helper()
	env := newBillFailureEnv(t, name)
	x := &expiryEnv{
		billFailureEnv: env,
		credits:        credit.NewService(credit.NewPostgresStore(env.db)),
		charger:        &perInvoiceCharger{},
		gap:            &gapInvoices{hookedInvoices: env.invs},
	}
	var resolver billing.TaxProviderResolver = tax.NewResolver(nil)
	if taxProvider != nil {
		resolver = fixedTaxResolver{taxProvider}
	}
	e := billing.NewEngine(
		&subStoreAdapter{env.subs}, &usageStoreAdapter{usage.NewPostgresStore(env.db)},
		env.pricing, x.gap, x.credits, tenant.NewSettingsStore(env.db),
		testPaymentSetupsReady{}, x.charger, env.clock,
	)
	e.SetIntervalReader(env.subs)
	e.SetTaxProviderResolver(resolver)
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetDunningResolver(&testDunningResolver{})
	x.eng = e
	return x
}

func (x *expiryEnv) customer(t *testing.T, name string) string {
	t.Helper()
	c, err := customer.NewPostgresStore(x.db).Create(x.ctx, x.tenantID, domain.Customer{ExternalID: "cus_" + name, DisplayName: name})
	if err != nil {
		t.Fatalf("create customer %s: %v", name, err)
	}
	return c.ID
}

// subFor creates an active subscription of custID on plan whose March period
// ended at periodEnd and is due now.
func (x *expiryEnv) subFor(t *testing.T, custID, code string, plan domain.Plan) domain.Subscription {
	t.Helper()
	start := x.periodStart
	sub, err := x.subs.Create(x.ctx, x.tenantID, domain.Subscription{
		Code: code, DisplayName: code, CustomerID: custID, Status: domain.SubscriptionActive,
		Items:       []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		BillingTime: domain.BillingTimeCalendar, StartedAt: &start,
		CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: bivPE(start),
	})
	if err != nil {
		t.Fatalf("subscription %s: %v", code, err)
	}
	subscriptiontest.SetBillingCycle(t, x.ctx, x.db, x.tenantID, sub.ID, x.periodStart, x.periodEnd, x.periodEnd, 0)
	return sub
}

// backdatedGrant grants amount to custID and then sets its expires_at to
// expires (Grant refuses a past expiry, so the past stamp is written
// directly). Returns the grant's id.
func (x *expiryEnv) backdatedGrant(t *testing.T, custID string, amount int64, expires time.Time) string {
	t.Helper()
	future := time.Now().UTC().Add(24 * time.Hour)
	g, err := x.credits.Grant(x.ctx, x.tenantID, credit.GrantInput{
		CustomerID: custID, AmountCents: amount, Description: "expiring commit", ExpiresAt: &future,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := x.admin.ExecContext(x.ctx, `UPDATE customer_credit_ledger SET expires_at = $1 WHERE id = $2`, expires, g.ID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}
	return g.ID
}

func (x *expiryEnv) expire(t *testing.T) {
	t.Helper()
	if _, errs := x.credits.ExpireCredits(x.ctx); len(errs) > 0 {
		t.Errorf("expire credits: %v", errs)
	}
}

func (x *expiryEnv) consumed(t *testing.T, grantID string) int64 {
	t.Helper()
	var c int64
	if err := x.admin.QueryRowContext(x.ctx, `SELECT consumed_cents FROM customer_credit_ledger WHERE id = $1`, grantID).Scan(&c); err != nil {
		t.Fatalf("read consumed_cents: %v", err)
	}
	return c
}

// ledger returns the customer's ledger sum and its expiry entries.
func (x *expiryEnv) ledger(t *testing.T, custID string) (sum int64, expiries int, expired int64) {
	t.Helper()
	if err := x.admin.QueryRowContext(x.ctx, `
		SELECT COALESCE(SUM(amount_cents), 0),
		       COUNT(*) FILTER (WHERE entry_type = 'expiry'),
		       COALESCE(-SUM(amount_cents) FILTER (WHERE entry_type = 'expiry'), 0)
		FROM customer_credit_ledger WHERE customer_id = $1`, custID).Scan(&sum, &expiries, &expired); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return sum, expiries, expired
}

// periodInvoice returns the one invoice the subscription's close produced.
func (x *expiryEnv) periodInvoice(t *testing.T, subID string) domain.Invoice {
	t.Helper()
	invs, _, err := x.invoices.List(x.ctx, invoice.ListFilter{TenantID: x.tenantID, SubscriptionID: subID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	if len(invs) != 1 {
		t.Fatalf("subscription %s has %d invoices, want 1", subID, len(invs))
	}
	return x.invoiceByID(t, invs[0].ID)
}

func (x *expiryEnv) invoiceByID(t *testing.T, id string) domain.Invoice {
	t.Helper()
	inv, err := x.invoices.Get(x.ctx, x.tenantID, id)
	if err != nil {
		t.Fatalf("get invoice %s: %v", id, err)
	}
	return inv
}

func equalCharges(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestFailedClose_ThenSuccess_DrawsTheGrantLiveAtThePeriodEnd: billing day,
// and the close fails (a counted failure; or a panic, which G1's drain saves
// as a failure while the tick still goes on to its expiry step). The
// customer's $30.00 commit expires six hours into the retry ladder, and the
// hourly expiry sweep runs. Then the cause is fixed and the operator retries
// billing. The $49.00 period invoice draws the whole grant, the card is
// charged $19.00, and nothing is left to expire.
//
// Fails before the fix: the sweep retired the grant, the close drew nothing,
// and the card was charged $49.00.
func TestFailedClose_ThenSuccess_DrawsTheGrantLiveAtThePeriodEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func() error
	}{
		{name: "counted failure"},
		{name: "panic", fail: func() error { panic("plan read panicked (test)") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newExpiryEnv(t, "Expiry Failed Close "+tc.name, nil)
			sub := x.dueSub(t, "sub-failed-close", x.planBad, x.periodEnd)
			grant := x.backdatedGrant(t, x.custID, 3000, x.periodEnd.Add(6*time.Hour))

			x.pricing.fail = tc.fail
			func() {
				defer func() {
					if r := recover(); r != nil && tc.fail == nil {
						t.Fatalf("unexpected panic: %v", r)
					}
				}()
				x.eng.RunCycle(x.ctx, 50, nil)
			}()
			if bf := x.get(t, sub.ID).BillingFailure; bf == nil || bf.NextAttemptAt == nil {
				t.Fatalf("setup: billing failure = %+v, want a saved failure waiting on the ladder", bf)
			}

			x.expire(t)
			if got := x.consumed(t, grant); got != 0 {
				t.Fatalf("grant consumed=%d after the sweep, want 0: the failed close still draws on it", got)
			}

			x.pricing.fail = nil
			x.pricing.failing.Store(false)
			svc := subscription.NewService(x.subs, x.clock)
			svc.SetDueBiller(x.eng)
			res, err := svc.RetryBilling(x.ctx, x.tenantID, sub.ID)
			if err != nil || res.InvoicesGenerated != 1 {
				t.Fatalf("retry billing: generated=%d err=%v, want 1 invoice", res.InvoicesGenerated, err)
			}

			inv := x.periodInvoice(t, sub.ID)
			if inv.TotalAmountCents != 4900 || inv.CreditsAppliedCents != 3000 {
				t.Errorf("invoice total/credits applied = %d/%d, want 4900/3000 (the grant was live at the period end)", inv.TotalAmountCents, inv.CreditsAppliedCents)
			}
			if got := x.charger.charges(inv.ID); !equalCharges(got, 1900) {
				t.Errorf("card charges = %v, want [1900] (total less the grant)", got)
			}
			x.expire(t)
			if sum, n, expired := x.ledger(t, x.custID); sum != 0 || n != 0 || expired != 0 {
				t.Errorf("ledger after the close: sum=%d expiry entries=%d expired=%d, want 0/0/0 (the grant paid the period; nothing was left to expire)", sum, n, expired)
			}
		})
	}
}

// TestSweepCollect_CycleInvoiceDrawsCreditLiveAtIssuedAt: the collection
// sweep collects a queued period invoice three hours after it was issued
// (its inline collection never ran: a crash or deploy after the close
// commit, or a finalize with no nudge). The customer's grant expired an hour
// ago and was not yet retired. A subscription_cycle and a threshold invoice
// test that expiry at their issued_at, so they draw it; the usage entry is
// still stamped at the collection instant (#1281). A manual invoice issued
// at the same instant tests expiry when it is collected, and so does nothing
// differently; nor does a cycle invoice issued after the grant expired.
//
// Fails before the fix: the sweep drew nothing and charged $50.00 each.
// Mutation: pass `at` instead of issued_at to the drain.
func TestSweepCollect_CycleInvoiceDrawsCreditLiveAtIssuedAt(t *testing.T) {
	x := newExpiryEnv(t, "Expiry Sweep Collect", nil)
	now := time.Now().UTC().Truncate(time.Microsecond)
	x.clock.Set(now)
	expires := now.Add(-time.Hour)

	cases := []struct {
		name   string
		reason domain.InvoiceBillingReason
		issued time.Time
		drawn  int64
	}{
		{"cycle", domain.BillingReasonSubscriptionCycle, now.Add(-3 * time.Hour), 2000},
		{"threshold", domain.BillingReasonThreshold, now.Add(-3 * time.Hour), 2000},
		{"manual", domain.BillingReasonManual, now.Add(-3 * time.Hour), 0},
		{"cycle issued after the expiry", domain.BillingReasonSubscriptionCycle, now.Add(-30 * time.Minute), 0},
	}
	invIDs := make([]string, len(cases))
	for i, tc := range cases {
		cust := x.customer(t, fmt.Sprintf("sweep_%d", i))
		x.backdatedGrant(t, cust, 2000, expires)
		issued := tc.issued
		due := issued.Add(30 * 24 * time.Hour)
		inv, err := x.invoices.Create(x.ctx, x.tenantID, domain.Invoice{
			InvoiceNumber: fmt.Sprintf("SWEEP-%d", i), CustomerID: cust,
			Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: issued.AddDate(0, -1, 0), BillingPeriodEnd: issued,
			IssuedAt: &issued, DueAt: &due, CreatedAt: issued, BillingReason: tc.reason,
		})
		if err != nil || !inv.AutoChargePending {
			t.Fatalf("seed %s invoice: queued=%v err=%v", tc.name, inv.AutoChargePending, err)
		}
		invIDs[i] = inv.ID
	}

	if _, errs := x.eng.RetryPendingCharges(x.ctx, 50, nil); len(errs) > 0 {
		t.Fatalf("collection sweep: %v", errs)
	}

	for i, tc := range cases {
		inv := x.invoiceByID(t, invIDs[i])
		if inv.CreditsAppliedCents != tc.drawn {
			t.Errorf("%s: credits applied = %d, want %d", tc.name, inv.CreditsAppliedCents, tc.drawn)
		}
		if got := x.charger.charges(inv.ID); !equalCharges(got, 5000-tc.drawn) {
			t.Errorf("%s: card charges = %v, want [%d]", tc.name, got, 5000-tc.drawn)
		}
		if tc.drawn == 0 {
			continue
		}
		var drawnAt time.Time
		if err := x.admin.QueryRowContext(x.ctx,
			`SELECT created_at FROM customer_credit_ledger WHERE invoice_id = $1 AND entry_type = 'usage'`, inv.ID).Scan(&drawnAt); err != nil {
			t.Fatalf("%s: read usage entry: %v", tc.name, err)
		}
		if !drawnAt.Equal(now) {
			t.Errorf("%s: usage entry created_at = %v, want the collection instant %v (only the expiry test moves to issued_at)", tc.name, drawnAt, now)
		}
	}
}

// TestTaxDeferredClose_RetryTaxLater_DrawsTheGrant (#653): billing day, and
// Stripe Tax is down, so the close commits the period's invoice as a draft
// and moves the subscription to the next period. The grant expires six hours
// later and the sweep runs. Tax comes back, the tax retry finalizes the draft
// and queues it (nothing nudges collection), and the sweep runs again: an
// operator's Retry tax can land after a tick's collection step and before
// its expiry step. A day after the period ended, the collection sweep draws
// the grant.
//
// Fails before the fix. Needs both halves: the hold (the draft, then the
// queued invoice) and the draw at issued_at; remove either and the card is
// charged $49.00.
func TestTaxDeferredClose_RetryTaxLater_DrawsTheGrant(t *testing.T) {
	taxp := &outageTax{Provider: tax.NewNoneProvider()}
	taxp.down.Store(true)
	x := newExpiryEnv(t, "Expiry Tax Deferred Close", taxp)
	sub := x.dueSub(t, "sub-tax-deferred", x.planOK, x.periodEnd)
	grant := x.backdatedGrant(t, x.custID, 3000, x.periodEnd.Add(6*time.Hour))

	if n, errs := x.eng.RunCycle(x.ctx, 50, nil); n != 1 || len(errs) > 0 {
		t.Fatalf("close: generated=%d errs=%v, want 1 invoice", n, errs)
	}
	draft := x.periodInvoice(t, sub.ID)
	if draft.Status != domain.InvoiceDraft || draft.TaxStatus != domain.InvoiceTaxPending {
		t.Fatalf("setup: invoice status/tax = %s/%s, want draft/pending", draft.Status, draft.TaxStatus)
	}
	x.expire(t)
	if got := x.consumed(t, grant); got != 0 {
		t.Fatalf("grant consumed=%d while the period's draft waits for tax, want 0", got)
	}

	taxp.down.Store(false)
	invSvc := invoice.NewService(x.invoices, x.clock, tenant.NewSettingsStore(x.db))
	invSvc.SetTaxRetrier(x.eng)
	fin, err := invSvc.RetryTax(x.ctx, x.tenantID, draft.ID)
	if err != nil || fin.Status != domain.InvoiceFinalized || !fin.AutoChargePending {
		t.Fatalf("retry tax: status=%s queued=%v err=%v, want finalized and queued", fin.Status, fin.AutoChargePending, err)
	}
	x.expire(t)
	if got := x.consumed(t, grant); got != 0 {
		t.Fatalf("grant consumed=%d while the finalized invoice waits for collection, want 0", got)
	}

	x.clock.Advance(24 * time.Hour)
	if _, errs := x.eng.RetryPendingCharges(x.ctx, 50, nil); len(errs) > 0 {
		t.Fatalf("collection sweep: %v", errs)
	}
	inv := x.invoiceByID(t, draft.ID)
	if inv.CreditsAppliedCents != 3000 {
		t.Errorf("credits applied = %d, want 3000 (the grant was live at the period end)", inv.CreditsAppliedCents)
	}
	if got := x.charger.charges(inv.ID); !equalCharges(got, 1900) {
		t.Errorf("card charges = %v, want [1900]", got)
	}
}

// TestExpiryVsCloseCollect_NoGapInTheHandoff: the expiry sweep runs while a
// due period is closed and collected, at every point of the hand-off: freely
// concurrent, inside the close transaction (before it commits), and after the
// close commit but before the collector's credit draw. In every round the
// grant pays the period up to its size, and only what is left over expires.
//
// Fails before the fix. Mutation: drop `auto_charge_pending` from the hold;
// a sweep between the close commit and the draw then retires the grant and
// the card is charged in full.
func TestExpiryVsCloseCollect_NoGapInTheHandoff(t *testing.T) {
	x := newExpiryEnv(t, "Expiry Close Handoff", nil)
	const total = int64(4900)
	modes := []string{"concurrent", "inside the close", "between the close and the draw"}
	for round := 0; round < 6; round++ {
		mode := round % len(modes)
		grantAmt := int64(3000)
		if round%2 == 1 {
			grantAmt = 6000
		}
		label := fmt.Sprintf("round %d (%s, grant %d)", round, modes[mode], grantAmt)
		cust := x.customer(t, fmt.Sprintf("handoff_%d", round))
		sub := x.subFor(t, cust, fmt.Sprintf("sub-handoff-%d", round), x.planOK)
		x.backdatedGrant(t, cust, grantAmt, x.periodEnd.Add(6*time.Hour))

		var once sync.Once
		expire := func() { once.Do(func() { x.expire(t) }) }
		switch mode {
		case 1:
			x.invs.onCreateTx = func(inv domain.Invoice) error {
				if inv.CustomerID == cust {
					expire()
				}
				return nil
			}
		case 2:
			x.gap.beforeClaim = func(string) { expire() }
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := x.eng.BillDueNow(x.ctx, x.tenantID, sub.ID); err != nil {
				t.Errorf("%s: bill: %v", label, err)
			}
		}()
		if mode == 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				expire()
			}()
		}
		close(start)
		wg.Wait()
		x.invs.onCreateTx, x.gap.beforeClaim = nil, nil

		inv := x.periodInvoice(t, sub.ID)
		want := min(grantAmt, total)
		if inv.CreditsAppliedCents != want {
			t.Errorf("%s: credits applied = %d, want %d", label, inv.CreditsAppliedCents, want)
		}
		var wantCharges []int64
		if total > want {
			wantCharges = []int64{total - want}
		}
		if got := x.charger.charges(inv.ID); !equalCharges(got, wantCharges...) {
			t.Errorf("%s: card charges = %v, want %v", label, got, wantCharges)
		}

		// The period is paid; a later sweep retires only what is left.
		x.expire(t)
		sum, n, expired := x.ledger(t, cust)
		if sum != 0 {
			t.Errorf("%s: ledger sum = %d after the sweep, want 0", label, sum)
		}
		if left := grantAmt - want; (left > 0 && (n != 1 || expired != left)) || (left == 0 && n != 0) {
			t.Errorf("%s: expiry entries=%d expired=%d, want the remainder %d only", label, n, expired, left)
		}
	}
}
