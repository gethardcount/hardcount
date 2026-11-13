package billing_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// P1-23 (ADR-139), on real Postgres: an immediate cancel, a scheduled
// mid-period cancel and dunning's cancel price their final invoice (usage
// reads and the tax call, a network call for a Stripe Tax tenant) and plan
// their relief BEFORE their transaction. The transaction's first statement is
// a compare-and-set on the period the plan was priced from; then the items and
// the billing-interval rows are compared with the ones planned, and the
// threshold watermark is read again. Anything that moved answers
// ErrWatermarkMoved with nothing written, and the cancel plans once more.
//
// Before, the pricing ran after the cancel's UPDATE: a slow tax call held the
// subscription row and a pool connection for its whole length, and about 20
// cancels at once (DB_MAX_OPEN_CONNS) held every connection while each waited
// for a second one.

// calcProbe serves the tenant's real tax provider, wrapped so that each
// Calculate first runs probe with the call's context and its call number
// (1-based), and records the request.
type calcProbe struct {
	inner *tax.Resolver
	probe func(ctx context.Context, call int)

	mu    sync.Mutex
	calls int
	reqs  []tax.Request
}

func newCalcProbe(probe func(ctx context.Context, call int)) *calcProbe {
	return &calcProbe{inner: tax.NewResolver(nil), probe: probe}
}

func (p *calcProbe) Resolve(ctx context.Context, ts domain.TenantSettings) (tax.Provider, error) {
	prov, err := p.inner.Resolve(ctx, ts)
	if err != nil {
		return nil, err
	}
	return calcProbeProvider{Provider: prov, p: p}, nil
}

func (p *calcProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *calcProbe) requests() []tax.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tax.Request(nil), p.reqs...)
}

type calcProbeProvider struct {
	tax.Provider
	p *calcProbe
}

func (c calcProbeProvider) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	c.p.mu.Lock()
	c.p.calls++
	n := c.p.calls
	c.p.reqs = append(c.p.reqs, req)
	probe := c.p.probe
	c.p.mu.Unlock()
	if probe != nil {
		probe(ctx, n)
	}
	return c.Provider.Calculate(ctx, req)
}

// raceTimeout bounds every change a probe makes. A change that waits on the
// cancel's own lock (the shape before P1-23) gives up after it, so the test
// fails instead of hanging.
const raceTimeout = 4 * time.Second

// probeErrs collects what a probe saw go wrong; it runs on the cancel's
// goroutine, where t.Fatal must not be called.
type probeErrs struct {
	mu   sync.Mutex
	errs []string
}

func (p *probeErrs) add(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, fmt.Sprintf(format, args...))
}

func (p *probeErrs) check(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.errs {
		t.Error(e)
	}
}

// evidenced gives a relief rig a subscription service whose store queues
// webhook events and whose cancels write their audit row, as router.go wires
// them, so a test can count subscription.canceled and the cancel audit rows.
func evidenced(r *reliefCancelRig) {
	store := subscription.NewPostgresStore(r.db)
	store.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
	svc := subscription.NewService(store, clock.NewFake(r.cancelAt))
	svc.SetBiller(r.engine)
	svc.SetAuditLogger(audit.NewLogger(r.db))
	r.subSvc = svc
}

// countTenantRows counts rows of query (one $1 parameter) under the tenant's
// row-level security.
func countTenantRows(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, query string, arg any) int {
	t.Helper()
	var n int
	if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, query, arg).Scan(&n)
	}); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// cancelEvidence counts the canceled subscription's subscription.canceled
// events and cancel audit rows.
func cancelEvidence(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, subID string) (events, audits int) {
	t.Helper()
	events = countTenantRows(t, ctx, db, tenantID, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1`, domain.EventSubscriptionCanceled)
	audits = countTenantRows(t, ctx, db, tenantID, `SELECT count(*) FROM audit_log WHERE action = 'cancel' AND resource_id = $1`, subID)
	return events, audits
}

func requireOneCancel(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, subID string) {
	t.Helper()
	if ev, au := cancelEvidence(t, ctx, db, tenantID, subID); ev != 1 || au != 1 {
		t.Errorf("subscription.canceled events = %d, cancel audit rows = %d; want one of each", ev, au)
	}
}

// The tax call holds nothing: while it runs, the subscription row is free
// (another writer could lock it at once) and no session of the app role is
// idle in a transaction.
//
// Mutations: in Service.cancel, plan inside the store's callback; in
// PlanCancelBill, leave the tax call to the writer → the row is held during
// the call and a session is idle in transaction.
func TestCancel_TaxCallHoldsNoLockAndNoTransaction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)

	for _, c := range []struct {
		name string
		run  func(*reliefCancelRig, *testing.T)
	}{
		{"immediate cancel", (*reliefCancelRig).cancelImmediately},
		{"scheduled cancel", (*reliefCancelRig).fireScheduledCancel},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newReliefCancelRig(t, db, "P1-23 tax call holds nothing "+c.name, reliefRigOpts{usageUnits: 3000})
			type seen struct {
				rowErr   error
				idleInTx int
			}
			var got []seen
			probe := newCalcProbe(func(context.Context, int) {
				var s seen
				tx, err := admin.BeginTx(r.ctx, nil)
				if err != nil {
					t.Errorf("probe: begin: %v", err)
					return
				}
				_, s.rowErr = tx.ExecContext(r.ctx, `SELECT 1 FROM subscriptions WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, r.subID)
				_ = tx.Rollback() // the probe only asked for the lock
				if err := admin.QueryRowContext(r.ctx, `
					SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND usename <> current_user
					  AND state LIKE 'idle in transaction%'`).Scan(&s.idleInTx); err != nil {
					t.Errorf("probe: read pg_stat_activity: %v", err)
				}
				got = append(got, s)
			})
			r.engine.SetTaxProviderResolver(probe)

			c.run(r, t)

			if len(got) != 1 {
				t.Fatalf("tax calculations during the cancel = %d, want 1 (the final invoice's)", len(got))
			}
			if got[0].rowErr != nil {
				t.Errorf("during the tax call the subscription row was held: %v", got[0].rowErr)
			}
			if got[0].idleInTx != 0 {
				t.Errorf("during the tax call %d app sessions were idle in a transaction, want 0", got[0].idleInTx)
			}
			if r.finalInvoice(t).TotalAmountCents != 3000 {
				t.Error("the final invoice is not the 3000c of usage")
			}
		})
	}
}

// offTxTracer records every statement that runs on another connection while
// the cancel's transaction is open: from its UPDATE that cancels the row
// until that connection commits or rolls back.
type offTxTracer struct {
	mu    sync.Mutex
	coord *pgx.Conn
	open  bool
	off   []string
}

func (tr *offTxTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	stmt := strings.Join(strings.Fields(data.SQL), " ")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	switch {
	case !tr.open && strings.Contains(stmt, "SET status = 'canceled'"):
		tr.open, tr.coord = true, conn
	case tr.open && conn == tr.coord:
		if l := strings.ToLower(stmt); l == "commit" || l == "rollback" {
			tr.open = false
		}
	case tr.open:
		if len(stmt) > 160 {
			stmt = stmt[:160] + "…"
		}
		tr.off = append(tr.off, stmt)
	}
	return ctx
}

func (tr *offTxTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *offTxTracer) offTx() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.off...)
}

// customerResolver resolves a customer's time domain by reading the customer
// row, as the engine's resolver does in production (Engine.SimForCustomer).
type customerResolver struct {
	customers *customer.PostgresStore
	at        time.Time
}

func (r customerResolver) SimForCustomer(ctx context.Context, tenantID, customerID string) (clock.Sim, error) {
	if _, err := r.customers.Get(ctx, tenantID, customerID); err != nil {
		return clock.Sim{}, err
	}
	return clock.Sim{At: r.at}, nil
}

func (r customerResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: r.at}, nil
}

func (r customerResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: r.at}, nil
}

// The cancel's transaction asks for no second connection: once it has locked
// the row, every statement until its commit runs on its own connection. A
// read on another connection there holds the cancel's locks while it waits
// for a free connection, the pool stall P1-23 removed.
//
// Mutations: plan inside the store's callback (M1); read the relief's
// funding invoices on the transaction (M12); read the invoice off the
// transaction in creditnote create (M10, paid prebill); bind the customer's
// clock in credit.ReverseForInvoiceTx (M11, the void case) → a statement on
// another connection is listed.
func TestCancel_TransactionReadsOnlyItsOwnConnection(t *testing.T) {
	testutil.SetupTestDB(t) // migrate and clean; each case opens its own traced pool

	type tracedCase struct {
		name string
		run  func(t *testing.T, db *postgres.DB)
	}
	cases := []tracedCase{
		{"paid prebill: the relief is a credit-note draft", func(t *testing.T, db *postgres.DB) {
			r := newReliefCancelRig(t, db, "P1-23 traced paid prebill", reliefRigOpts{usageUnits: 3000})
			r.cancelImmediately(t)
		}},
		{"first-day unpaid prebill with credit applied: the relief voids it and returns the credit", func(t *testing.T, db *postgres.DB) {
			r := newReliefCancelRig(t, db, "P1-23 traced voided prebill", reliefRigOpts{usageUnits: 500, sameDay: true, day1Credit: 1000})
			// As router.go wires them: the void hands the applied credit back,
			// and the credit service resolves the customer's time domain.
			voider := invoice.NewService(r.invStore, clock.Real(), tenant.NewSettingsStore(db))
			voider.SetCreditReverser(r.credit)
			r.engine.SetInvoiceVoider(voider)
			r.credit.SetResolver(customerResolver{customers: customer.NewPostgresStore(db), at: r.cancelAt})
			r.cancelImmediately(t)
			if d := r.invoice(t, r.day1ID); d.Status != domain.InvoiceVoided {
				t.Fatalf("day-1 invoice is %s, want voided by the relief", d.Status)
			}
		}},
		{"in arrears: no relief", func(t *testing.T, db *postgres.DB) {
			r := newArrearsCancelRig(t, db, "P1-23 traced in arrears", 1, 0)
			if _, _, err := r.cancel(0); err != nil {
				t.Fatalf("cancel: %v", err)
			}
		}},
		{"scheduled cancel, paid prebill", func(t *testing.T, db *postgres.DB) {
			r := newReliefCancelRig(t, db, "P1-23 traced scheduled", reliefRigOpts{usageUnits: 3000})
			r.fireScheduledCancel(t)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &offTxTracer{}
			c.run(t, testutil.OpenTracedDB(t, tr))
			if off := tr.offTx(); len(off) != 0 {
				t.Errorf("%d statements ran on another connection while the cancel's transaction was open:\n  %s",
					len(off), strings.Join(off, "\n  "))
			}
		})
	}
}

// arrearsCancelRig is one tenant with an in-arrears monthly plan (3000c base,
// 1c a call) and n active subscriptions, each with its own customer and 1000
// calls on 2026-06-03, in the period [2026-06-01, 2026-07-01). The cancel
// lands on 2026-06-16: each final invoice bills 1500c of base (15 of 30
// days) and 1000c of usage. No card is on file, so no final invoice is
// charged.
type arrearsCancelRig struct {
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	subIDs   []string
	invStore *invoice.PostgresStore
	engine   *billing.Engine
	subSvc   *subscription.Service
}

const (
	arrearsBaseCents  = 3000
	arrearsUsageCents = 1000
	arrearsFinalCents = arrearsBaseCents/2 + arrearsUsageCents // 15 of 30 days, plus usage
)

// newArrearsCancelRig seeds the rig. taxRate, when above zero, is the
// tenant's manual tax rate in percent.
func newArrearsCancelRig(t *testing.T, db *postgres.DB, name string, n int, taxRate float64) *arrearsCancelRig {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	settings := tenant.NewSettingsStore(db)
	if taxRate > 0 {
		if _, err := settings.Update(ctx, tenantID, func(ts *domain.TenantSettings) error {
			ts.TaxProvider, ts.TaxRate, ts.TaxName = "manual", taxRate, "Sales Tax"
			return nil
		}); err != nil {
			t.Fatalf("update settings: %v", err)
		}
	}

	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	meter, err := pricingSvc.CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: "calls", Name: "Calls", Unit: "calls", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	plan, _, err := pricingSvc.CreatePlan(ctx, tenantID, pricing.CreatePlanInput{
		Code: "arrears", Name: "Arrears", Currency: "USD", BillingInterval: domain.BillingMonthly,
		BaseAmountCents: arrearsBaseCents, BaseBillTiming: domain.BillInArrears,
		Prices: []pricing.PriceInput{{MeterID: meter.ID, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}}},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	e := billing.NewEngine(
		&subStoreAdapter{subStore}, &usageStoreAdapter{usageStore},
		&pricingStoreAdapter{pricingStore}, &invoiceStoreAdapter{invStore},
		nil, settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(reliefCancelAt),
	)
	e.SetIntervalReader(subStore)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetStripeReadiness(testStripeConnected{})
	e.SetInvoiceEmailer(&testInvoiceEmailer{})
	e.SetDunningResolver(&testDunningResolver{})

	r := &arrearsCancelRig{ctx: ctx, db: db, tenantID: tenantID, invStore: invStore, engine: e}
	periodStart, periodEnd := reliefPeriodStart, reliefPeriodEnd
	usageAt := reliefPeriodStart.AddDate(0, 0, 2)
	custStore := customer.NewPostgresStore(db)
	for i := range n {
		cust, err := custStore.Create(ctx, tenantID, domain.Customer{
			ExternalID: fmt.Sprintf("cus_arrears_%d", i), DisplayName: fmt.Sprintf("Arrears %d", i),
		})
		if err != nil {
			t.Fatalf("create customer %d: %v", i, err)
		}
		sub, err := subStore.Create(ctx, tenantID, domain.Subscription{
			Code: fmt.Sprintf("sub-arrears-%d", i), DisplayName: "Arrears", CustomerID: cust.ID,
			Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
			Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
			StartedAt: &periodStart, CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
		})
		if err != nil {
			t.Fatalf("create subscription %d: %v", i, err)
		}
		subscriptiontest.SetBillingCycle(t, ctx, db, tenantID, sub.ID, periodStart, periodEnd, periodEnd, 0)
		if _, err := usage.NewService(usageStore).Ingest(ctx, tenantID, usage.IngestInput{
			CustomerID: cust.ID, MeterID: meter.ID, Quantity: decimal.NewFromInt(arrearsUsageCents), Timestamp: &usageAt,
		}); err != nil {
			t.Fatalf("ingest usage %d: %v", i, err)
		}
		r.subIDs = append(r.subIDs, sub.ID)
	}
	r.subSvc = subscription.NewService(subStore, clock.NewFake(reliefCancelAt))
	r.subSvc.SetBiller(e)
	return r
}

// cancel is the immediate cancel of subscription i at reliefCancelAt.
func (r *arrearsCancelRig) cancel(i int) (domain.Subscription, int64, error) {
	return r.subSvc.Cancel(clock.WithEffectiveNow(r.ctx, reliefCancelAt), r.tenantID, r.subIDs[i])
}

// finalInvoices returns the tenant's final invoices, keyed by subscription.
func (r *arrearsCancelRig) finalInvoices(t *testing.T) map[string]domain.Invoice {
	t.Helper()
	invs, _, err := r.invStore.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID, Limit: 100})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	out := map[string]domain.Invoice{}
	for _, inv := range invs {
		if inv.BillingReason == domain.BillingReasonSubscriptionCancel {
			if _, dup := out[inv.SubscriptionID]; dup {
				t.Fatalf("subscription %s has two final invoices", inv.SubscriptionID)
			}
			out[inv.SubscriptionID] = inv
		}
	}
	return out
}

// 25 cancels at once against a pool of 20 connections, each waiting in its
// tax call until all 25 are in theirs. The tax call holds no connection, so
// all 25 get there, and all 25 finish.
//
// Before, each cancel held a connection in its transaction from its UPDATE
// and asked for a second one to price: 20 held every connection, none could
// price, and all waited until their deadline.
//
// Mutation: plan inside the store's callback (M1) → the barrier is never
// reached and the cancels time out.
func TestCancel_25ConcurrentAgainstPoolOf20_AllFinish(t *testing.T) {
	const cancels, poolSize = 25, 20
	db := testutil.SetupTestDB(t)
	r := newArrearsCancelRig(t, db, "P1-23 25 cancels, pool of 20", cancels, 0)
	db.Pool.SetMaxOpenConns(poolSize)

	var entered atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	var barrierMissed atomic.Bool
	r.engine.SetTaxProviderResolver(newCalcProbe(func(context.Context, int) {
		if entered.Add(1) == cancels {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(15 * time.Second):
			barrierMissed.Store(true)
			once.Do(func() { close(release) })
		}
	}))

	errs := make([]error, cancels)
	var wg sync.WaitGroup
	for i := range cancels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(clock.WithEffectiveNow(r.ctx, reliefCancelAt), 30*time.Second)
			defer cancel()
			_, _, errs[i] = r.subSvc.Cancel(ctx, r.tenantID, r.subIDs[i])
		}()
	}
	wg.Wait()

	if barrierMissed.Load() {
		t.Errorf("only %d of %d cancels reached their tax call together: the others could not get a connection", entered.Load(), cancels)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("cancel %d: %v", i, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	finals := r.finalInvoices(t)
	if len(finals) != cancels {
		t.Fatalf("%d final invoices, want %d", len(finals), cancels)
	}
	var nums []int
	for _, inv := range finals {
		num := inv.InvoiceNumber
		i := len(num)
		for i > 0 && num[i-1] >= '0' && num[i-1] <= '9' {
			i--
		}
		n, err := strconv.Atoi(num[i:])
		if err != nil {
			t.Fatalf("invoice number %q: %v", inv.InvoiceNumber, err)
		}
		nums = append(nums, n)
		if inv.TotalAmountCents != arrearsFinalCents {
			t.Errorf("final invoice %s = %dc, want %dc", inv.InvoiceNumber, inv.TotalAmountCents, arrearsFinalCents)
		}
	}
	sort.Ints(nums)
	for i := 1; i < len(nums); i++ {
		if nums[i] != nums[i-1]+1 {
			t.Fatalf("final invoice numbers %v are not consecutive", nums)
		}
	}
}

// A change that commits while the cancel is priced is seen on the cancel's
// transaction, and the cancel plans once more from the new row. Each case
// changes the subscription inside the first tax call, except the trial case,
// whose first plan makes none: it changes it at the plan's read of the
// billing intervals. On main each change
// waited for the cancel's own row lock, which the cancel held across that
// call: it timed out (raceTimeout) and the cancel committed what it had
// priced.
func TestCancel_SnapshotMoved_ReplansOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)

	// changeDuringFirstCall runs change, bounded by raceTimeout, inside the
	// rig's first tax call.
	changeDuringFirstCall := func(r *reliefCancelRig, pe *probeErrs, what string, change func(ctx context.Context) error) *calcProbe {
		p := newCalcProbe(func(ctx context.Context, call int) {
			if call != 1 {
				return
			}
			cctx, cancel := context.WithTimeout(ctx, raceTimeout)
			defer cancel()
			if err := change(cctx); err != nil {
				pe.add("%s while the cancel was priced: %v (it waited on the cancel's lock)", what, err)
			}
		})
		r.engine.SetTaxProviderResolver(p)
		return p
	}
	item := func(t *testing.T, r *reliefCancelRig) domain.SubscriptionItem {
		t.Helper()
		sub, err := subscription.NewPostgresStore(r.db).Get(r.ctx, r.tenantID, r.subID)
		if err != nil || len(sub.Items) != 1 {
			t.Fatalf("read subscription: %v items=%d", err, len(sub.Items))
		}
		return sub.Items[0]
	}
	quantity := func(n int64) subscription.UpdateItemInput {
		return subscription.UpdateItemInput{Quantity: &n, Immediate: true}
	}

	// (a) A quantity change: the items read on the transaction are not the
	// planned ones (sameItems). Mutation M4 drops that check → one plan.
	t.Run("quantity changed", func(t *testing.T) {
		r := newReliefCancelRig(t, db, "P1-23 replan quantity", reliefRigOpts{usageUnits: 3000})
		evidenced(r)
		it := item(t, r)
		var pe probeErrs
		p := changeDuringFirstCall(r, &pe, "quantity change", func(ctx context.Context) error {
			_, err := r.subSvc.UpdateItem(ctx, r.tenantID, r.subID, it.ID, quantity(2))
			return err
		})
		// Planned again from two seats, the relief is two seats' unused half,
		// 4900c, which is all the paid day-1 invoice can be credited.
		r.wantCredited = reliefBaseCents
		r.cancelImmediately(t)
		pe.check(t)
		if n := p.count(); n != 2 {
			t.Errorf("tax calculations = %d, want 2: the cancel must plan again from the changed items", n)
		}
		if got := item(t, r); got.Quantity != 2 {
			t.Errorf("item quantity = %d, want 2 (the change committed)", got.Quantity)
		}
		requireOneCancel(t, r.ctx, r.db, r.tenantID, r.subID)
	})

	// An item removed: its billing interval only gets an end, which the
	// interval check leaves out, so only the items read on the transaction
	// show it. Mutation M4 (drop sameItems) → one plan.
	t.Run("item removed", func(t *testing.T) {
		r := newReliefCancelRig(t, db, "P1-23 replan removal", reliefRigOpts{usageUnits: 3000})
		evidenced(r)
		addOn, _, err := pricing.NewService(pricing.NewPostgresStore(r.db)).CreatePlan(r.ctx, r.tenantID, pricing.CreatePlanInput{
			Code: "add-on", Name: "Add-on", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 1000, BaseBillTiming: domain.BillInArrears,
		})
		if err != nil {
			t.Fatalf("create add-on plan: %v", err)
		}
		added, err := r.subSvc.AddItem(clock.WithEffectiveNow(r.ctx, reliefPeriodStart), r.tenantID, r.subID, subscription.AddItemInput{PlanID: addOn.ID, Quantity: 1})
		if err != nil {
			t.Fatalf("add the add-on: %v", err)
		}
		var pe probeErrs
		p := changeDuringFirstCall(r, &pe, "remove an item", func(ctx context.Context) error {
			return r.subSvc.RemoveItem(ctx, r.tenantID, r.subID, added.ID)
		})
		r.cancelImmediately(t)
		pe.check(t)
		if n := p.count(); n != 2 {
			t.Errorf("tax calculations = %d, want 2: the cancel must plan again without the removed item", n)
		}
		requireOneCancel(t, r.ctx, r.db, r.tenantID, r.subID)
	})

	// The billing run closes the period while the cancel is priced: the
	// period moves and no item does. The cancel's instant is bound before the
	// boundary, as dunning's is, so the next attempt finds a period that
	// starts after it and does not plan: the cancel returns ErrWatermarkMoved
	// with nothing written, and June's usage is billed once, on the period's
	// own invoice. Mutation M3 (drop the period predicates from the cancel's
	// compare-and-set) → the cancel commits a final invoice for the closed
	// period and bills June's usage twice.
	t.Run("period closed by the billing run", func(t *testing.T) {
		r := newReliefCancelRig(t, db, "P1-23 close during pricing", reliefRigOpts{usageUnits: 3000})
		evidenced(r)
		var pe probeErrs
		changeDuringFirstCall(r, &pe, "the period close", func(ctx context.Context) error {
			closeCtx := clock.WithEffectiveNow(context.WithoutCancel(ctx), reliefPeriodEnd.Add(time.Hour))
			cctx, cancel := context.WithTimeout(closeCtx, raceTimeout)
			defer cancel()
			if n, failures := r.engine.RunCycleForTenant(cctx, r.tenantID, 50); n != 1 || len(failures) != 0 {
				return fmt.Errorf("closed %d periods, failures %v", n, failures)
			}
			return nil
		})
		beforeBoundary := clock.WithEffectiveNow(r.ctx, reliefPeriodEnd.Add(-time.Hour))
		if _, _, err := r.subSvc.Cancel(beforeBoundary, r.tenantID, r.subID); !errors.Is(err, subscription.ErrWatermarkMoved) {
			t.Errorf("cancel: %v, want ErrWatermarkMoved: its instant is before the period the close opened", err)
		}
		pe.check(t)
		sub, err := subscription.NewPostgresStore(r.db).Get(r.ctx, r.tenantID, r.subID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.Status != domain.SubscriptionActive || sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(reliefPeriodEnd) {
			t.Errorf("subscription status=%s period start=%v, want active in the period from %v", sub.Status, sub.CurrentBillingPeriodStart, reliefPeriodEnd)
		}
		invs, _, err := r.invStore.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID, Limit: 50})
		if err != nil {
			t.Fatalf("list invoices: %v", err)
		}
		for _, inv := range invs {
			if inv.BillingReason == domain.BillingReasonSubscriptionCancel {
				t.Errorf("a final cancel invoice was written (%dc): June's usage is on the period's own invoice", inv.TotalAmountCents)
			}
		}
		if ev, au := cancelEvidence(t, r.ctx, r.db, r.tenantID, r.subID); ev != 0 || au != 0 {
			t.Errorf("subscription.canceled events = %d, cancel audit rows = %d; want none", ev, au)
		}
	})

	// The activation scan converts a trial while the cancel is planned. The
	// cancel lands an hour after the trial ended, before the scan ran, and
	// reads the row trialing and never activated, so its plan bills no final
	// invoice (a trial is free, ADR-069). The scan then flips the row to
	// active and bills the first period in advance. The conversion moves
	// neither the period nor the items. It opens the items' billing
	// intervals, but here it commits before the plan reads them, so the plan
	// and the transaction read the same rows. Only the status in the
	// compare-and-set shows the change. The cancel plans again from the
	// active row: it bills the hour of usage since the trial ended, and voids
	// the unused, unpaid first-period invoice. Mutation M18 (drop
	// `AND status = $3` from the cancel's compare-and-set) → the cancel
	// commits the trial's plan, with no final invoice: the usage is never
	// billed.
	t.Run("trial activated while planned", func(t *testing.T) {
		r := newReliefCancelRig(t, db, "P1-23 trial activated during the plan", reliefRigOpts{usageUnits: 3000, sameDay: true, trial: true})
		store := subscription.NewPostgresStore(r.db)
		store.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
		scan := subscription.NewService(store, clock.NewFake(r.cancelAt))
		scan.SetBiller(r.engine)
		hooked := &readHookStore{PostgresStore: store}
		svc := subscription.NewService(hooked, clock.NewFake(r.cancelAt))
		svc.SetBiller(r.engine)
		svc.SetAuditLogger(audit.NewLogger(r.db))
		r.subSvc = svc

		// The probe counts the tax calls after the conversion: the cancel's.
		p := newCalcProbe(nil)
		var pe probeErrs
		var once sync.Once
		hooked.onListIntervals = func(ctx context.Context) {
			once.Do(func() {
				sctx, cancel := context.WithTimeout(clock.WithEffectiveNow(context.WithoutCancel(ctx), r.cancelAt), raceTimeout)
				defer cancel()
				if n, errs := scan.ProcessExpiredTrials(sctx, 50); n != 1 || len(errs) != 0 {
					pe.add("trial activation while the cancel was planned: activated %d, errors %v", n, errs)
				}
				r.engine.SetTaxProviderResolver(p)
			})
		}

		r.cancelImmediately(t)
		pe.check(t)
		if n := p.count(); n != 1 {
			t.Errorf("tax calculations by the cancel = %d, want 1: the plan from the trialing row prices nothing, the plan from the active row prices the final invoice", n)
		}
		sub, err := store.Get(r.ctx, r.tenantID, r.subID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.ActivatedAt == nil {
			t.Fatalf("activated_at is not set: the trial was not converted while the cancel was planned")
		}
		if final := r.finalInvoice(t); final.TotalAmountCents != 3000 {
			t.Errorf("final invoice = %dc, want the 3000c of usage since the trial ended", final.TotalAmountCents)
		}
		invs, _, err := r.invStore.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID, Limit: 50})
		if err != nil {
			t.Fatalf("list invoices: %v", err)
		}
		var first []domain.Invoice
		for _, inv := range invs {
			if inv.BillingReason == domain.BillingReasonSubscriptionCreate {
				first = append(first, inv)
			}
		}
		if len(first) != 1 || first[0].Status != domain.InvoiceVoided {
			t.Errorf("first-period invoices = %+v, want one, voided by the cancel's relief (unpaid, and none of it used)", first)
		}
		requireOneCancel(t, r.ctx, r.db, r.tenantID, r.subID)
	})

	// (d) A collection pause: it moves nothing the plan priced, so there is no
	// second plan, and the final invoice follows the locked row's pause: a
	// draft. Mutation M14 (status from the pause read before the
	// transaction) → the invoice is finalized.
	t.Run("collection paused", func(t *testing.T) {
		r := newReliefCancelRig(t, db, "P1-23 pause during pricing", reliefRigOpts{usageUnits: 3000})
		evidenced(r)
		var pe probeErrs
		p := changeDuringFirstCall(r, &pe, "pause", func(ctx context.Context) error {
			_, err := r.subSvc.PauseCollection(ctx, r.tenantID, r.subID, subscription.PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft})
			return err
		})
		r.cancelImmediately(t)
		pe.check(t)
		if n := p.count(); n != 1 {
			t.Errorf("tax calculations = %d, want 1: a pause changes nothing the plan priced", n)
		}
		if final := r.finalInvoice(t); final.Status != domain.InvoiceDraft {
			t.Errorf("final invoice status = %s, want draft: the subscription's collection was paused before the cancel committed", final.Status)
		}
		requireOneCancel(t, r.ctx, r.db, r.tenantID, r.subID)
	})

	// (e) Changes that leave the live items as they were: an item added then
	// removed, a quantity changed and changed back. sameItems passes; the
	// billing-interval rows do not. Mutation M4 (drop sameIntervals) → one
	// plan.
	t.Run("item added then removed", func(t *testing.T) {
		r := newReliefCancelRig(t, db, "P1-23 replan add-remove", reliefRigOpts{usageUnits: 3000})
		evidenced(r)
		addOn, _, err := pricing.NewService(pricing.NewPostgresStore(r.db)).CreatePlan(r.ctx, r.tenantID, pricing.CreatePlanInput{
			Code: "add-on", Name: "Add-on", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 1000, BaseBillTiming: domain.BillInAdvance,
		})
		if err != nil {
			t.Fatalf("create add-on plan: %v", err)
		}
		var pe probeErrs
		p := changeDuringFirstCall(r, &pe, "add then remove an item", func(ctx context.Context) error {
			added, err := r.subSvc.AddItem(ctx, r.tenantID, r.subID, subscription.AddItemInput{PlanID: addOn.ID, Quantity: 1})
			if err != nil {
				return err
			}
			return r.subSvc.RemoveItem(ctx, r.tenantID, r.subID, added.ID)
		})
		r.cancelImmediately(t)
		pe.check(t)
		if n := p.count(); n != 2 {
			t.Errorf("tax calculations = %d, want 2: an item that came and went changes the billing intervals", n)
		}
		requireOneCancel(t, r.ctx, r.db, r.tenantID, r.subID)
	})
	t.Run("quantity changed and changed back", func(t *testing.T) {
		r := newReliefCancelRig(t, db, "P1-23 replan quantity back", reliefRigOpts{usageUnits: 3000})
		evidenced(r)
		it := item(t, r)
		var pe probeErrs
		p := changeDuringFirstCall(r, &pe, "quantity change and back", func(ctx context.Context) error {
			if _, err := r.subSvc.UpdateItem(ctx, r.tenantID, r.subID, it.ID, quantity(2)); err != nil {
				return err
			}
			_, err := r.subSvc.UpdateItem(ctx, r.tenantID, r.subID, it.ID, quantity(1))
			return err
		})
		r.cancelImmediately(t)
		pe.check(t)
		if n := p.count(); n != 2 {
			t.Errorf("tax calculations = %d, want 2: a quantity changed and back changes the billing intervals", n)
		}
		requireOneCancel(t, r.ctx, r.db, r.tenantID, r.subID)
	})

	// (f) An operator credit note on the paid day-1 invoice, written while the
	// final invoice is priced. The relief is planned after the tax call, so it
	// reads the room the note left and fits it: no second plan, and no cap
	// refusal. Mutation M17 (plan the relief before pricing) → the relief is
	// sized to the room before the note, and its draft is refused under the
	// invoice's credit-note lock.
	t.Run("operator credit note on the funding invoice", func(t *testing.T) {
		const noteCents = 3000
		r := newReliefCancelRig(t, db, "P1-23 relief after the tax call", reliefRigOpts{usageUnits: 3000})
		evidenced(r)
		var pe probeErrs
		p := changeDuringFirstCall(r, &pe, "operator credit note", func(ctx context.Context) error {
			_, err := r.cn.Create(ctx, r.tenantID, creditnote.CreateInput{
				InvoiceID: r.day1ID, Reason: "goodwill",
				Lines: []creditnote.CreditLineInput{{Description: "goodwill", Quantity: 1, UnitAmountCents: noteCents}},
			})
			return err
		})
		r.wantCredited = reliefBaseCents - noteCents // the room the note left
		r.cancelImmediately(t)
		pe.check(t)
		if n := p.count(); n != 1 {
			t.Errorf("tax calculations = %d, want 1: a credit note moves nothing the cancel's snapshot holds", n)
		}
		var relief []int64
		for _, n := range creditNotesOn(t, r.ctx, r.db, r.tenantID, r.day1ID) {
			if n.TotalCents != noteCents {
				relief = append(relief, n.TotalCents)
			}
		}
		if len(relief) != 1 || relief[0] != reliefBaseCents-noteCents {
			t.Errorf("relief notes on day-1 = %v, want one of %dc: the room the operator's note left", relief, reliefBaseCents-noteCents)
		}
		requireOneCancel(t, r.ctx, r.db, r.tenantID, r.subID)
	})
}

// (b) of the same test, on the threshold rig: a spend-threshold invoice fires
// while the cancel is priced. The cancel's transaction reads the watermark
// again, sees the fire, and plans once more: the final invoice bills only the
// usage after the fire. Mutation M5 (drop the watermark check in
// writePlanned) → the final invoice bills the fire's window a second time.
func TestCancel_SnapshotMoved_ReplansOnce_ThresholdFire(t *testing.T) {
	r := newS1Rig(t, "P1-23 fire during the cancel's pricing", 200)
	svc := subscription.NewService(r.f.subStore, nil)
	svc.SetBiller(r.closer.engine)

	var pe probeErrs
	p := newCalcProbe(func(_ context.Context, call int) {
		if call != 1 {
			return
		}
		ctx, cancel := context.WithTimeout(r.at(r.t1), raceTimeout)
		defer cancel()
		fired, errs := r.fire.engine.ScanThresholds(ctx, 50)
		if len(errs) != 0 || fired != 1 {
			pe.add("threshold fire while the cancel was priced: fired=%d errs=%v (it waited on the cancel's lock)", fired, errs)
		}
	})
	r.closer.engine.SetTaxProviderResolver(p)

	if _, _, err := svc.Cancel(r.at(r.tc), r.f.tenantID, r.f.subID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	pe.check(t)
	if n := p.count(); n != 2 {
		t.Errorf("tax calculations = %d, want 2: the cancel must plan again after the fire", n)
	}
	_, threshold, other := r.liveInvoices(t)
	if len(threshold) != 1 || len(other) != 1 {
		t.Fatalf("invoices: threshold=%d other=%d, want the fire's and the final invoice", len(threshold), len(other))
	}
	var billed int64
	for _, l := range r.usageLines(t, other[0].ID) {
		billed += l.AmountCents
	}
	if billed != 100 {
		t.Errorf("final invoice usage = %dc, want 100c: only [t1, tc), after the fire's window", billed)
	}
	assertUsageBilledOnce(t, r.base)
}

// (c) of the same test: an in-advance cross-interval swap re-anchors the
// period while the cancel is priced, at an instant after the cancel's first
// one. The cancel (the API's: its context is not bound) plans again at a new
// instant, at or after the new period's start: the final invoice is priced
// from the new period, and the swap's yearly prebill is relieved (voided:
// nothing of it was used or paid). Mutations M3 (drop the period predicates
// from the cancel's compare-and-set) and M7 (bind the instant once, outside
// the attempts) → the cancel commits on the old period, or on the new one at
// the old instant, and the yearly prebill stands.
func TestCancel_SnapshotMoved_ReplansOnce_SwapReanchorsThePeriod(t *testing.T) {
	r := newPrebillVoidRig(t, "P1-23 swap during the cancel's pricing", waitingPolicy())
	r.subStore.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
	r.subSvc.SetResolver(wallClockResolver{})

	var pe probeErrs
	var swapAt time.Time
	var hooked atomic.Bool
	r.pricing.onGetPlan = func() {
		if !hooked.CompareAndSwap(false, true) {
			return
		}
		swapAt = time.Now().UTC().Add(time.Millisecond).Truncate(time.Microsecond)
		ctx, cancel := context.WithTimeout(clock.WithEffectiveNow(r.ctx, swapAt), raceTimeout)
		defer cancel()
		res, err := swapToYearly(ctx, r)
		if err != nil {
			pe.add("cross-interval swap while the cancel was priced: %v (it waited on the cancel's lock)", err)
			return
		}
		r.subSvc.FinalizeCrossIntervalSwap(ctx, r.tenantID, r.sub, res)
		// The cancel's next attempt reads the wall clock: let it pass the
		// swap's instant.
		time.Sleep(time.Until(swapAt))
	}

	canceled, _, err := r.subSvc.Cancel(r.ctx, r.tenantID, r.sub.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	pe.check(t)
	if t.Failed() {
		t.FailNow()
	}
	if canceled.CurrentBillingPeriodStart == nil || !canceled.CurrentBillingPeriodStart.Equal(swapAt) {
		t.Fatalf("canceled period start = %v, want the swap's %v", canceled.CurrentBillingPeriodStart, swapAt)
	}
	if canceled.CanceledAt == nil || canceled.CanceledAt.Before(swapAt) {
		t.Errorf("canceled_at = %v, want at or after the new period's start %v", canceled.CanceledAt, swapAt)
	}
	invs, _, err := r.invStore.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID, Limit: 50})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var yearly []domain.Invoice
	for _, inv := range invs {
		if inv.ID != r.day1.ID && inv.BillingReason != domain.BillingReasonSubscriptionCancel {
			yearly = append(yearly, inv)
		}
	}
	if len(yearly) != 1 || yearly[0].Status != domain.InvoiceVoided {
		t.Errorf("swap invoices = %+v, want the yearly prebill, voided by the cancel's relief", yearly)
	}
	if n := countTenantRows(t, r.ctx, r.db, r.tenantID, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1`, domain.EventSubscriptionCanceled); n != 1 {
		t.Errorf("subscription.canceled events = %d, want 1", n)
	}
}

// wallClockResolver resolves every entity as a wall-clock one, as the engine
// does for an entity on no test clock: to the bound instant on a bound
// context, and to the wall clock's now otherwise. So a second bind on a bound
// context gives the first instant back, and a bind on the API's unbound
// context gives a new one.
type wallClockResolver struct{}

func (wallClockResolver) sim(ctx context.Context) clock.Sim {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return clock.Sim{At: at}
	}
	return clock.Sim{At: time.Now().UTC()}
}

func (r wallClockResolver) SimForSubscription(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx), nil
}

func (r wallClockResolver) SimForCustomer(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx), nil
}

func (r wallClockResolver) SimForInvoice(ctx context.Context, _, _ string) (clock.Sim, error) {
	return r.sim(ctx), nil
}

// swapToYearly runs the monthly-to-yearly swap of the prebill rig's item on
// its own transaction, as the item handler does.
func swapToYearly(ctx context.Context, r *prebillVoidRig) (subscription.ItemChangeResult, error) {
	var res subscription.ItemChangeResult
	err := r.db.WithTenantTx(ctx, r.tenantID, func(tx *sql.Tx) error {
		var err error
		res, err = r.subSvc.UpdateItemTx(ctx, tx, r.tenantID, r.sub.ID, r.sub.Items[0].ID, subscription.UpdateItemInput{
			NewPlanID: r.yearly.ID, Immediate: true,
		})
		if err == nil && !res.OrchestratedCrossAxis {
			err = errors.New("the monthly-to-yearly swap did not take the atomic cross-interval path")
		}
		return err
	})
	return res, err
}

// A scheduled cancel fired by the billing run, whose subscription changes
// while it is priced: the fire answers ErrWatermarkMoved with nothing
// written, billSubscription reads the row again and fires again, and the
// subscription is canceled in the same run with no billing failure saved.
//
// Item writers refuse a subscription whose scheduled cancel is due (it is
// due for billing), so the writers that can race the fire are the ones that
// do not change items: a void of the period's threshold invoice, and a
// period writer. The period-move case leaves the lines unchanged, and the
// cancel instant is the contracted one, so its second tax call sends the
// same request: the same Stripe Tax key.
//
// Mutations: drop the ErrWatermarkMoved branch at the cancel arm of
// billSubscription (M9) → the run saves a billing failure and the
// subscription stays active; drop the period predicates from the scheduled
// compare-and-set (M16) → the period-move case prices once.
func TestScheduledCancel_SnapshotMoved_BillingRunReplans(t *testing.T) {
	// requireCanceledInTheRun checks the run's outcome for one subscription.
	requireCanceledInTheRun := func(t *testing.T, ctx context.Context, store *subscription.PostgresStore, tenantID, subID string, errs []error) {
		t.Helper()
		if len(errs) != 0 {
			t.Errorf("billing run: %v", errs)
		}
		sub, err := store.Get(ctx, tenantID, subID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.Status != domain.SubscriptionCanceled {
			t.Fatalf("subscription is %s after the run, want canceled in the same run", sub.Status)
		}
		if sub.BillingFailure != nil {
			t.Errorf("billing failure saved: %+v, want none: a moved snapshot is not a failure", sub.BillingFailure)
		}
	}

	t.Run("threshold invoice voided", func(t *testing.T) {
		r := newS1Rig(t, "P1-23 scheduled replan: void", 200)
		admin := testutil.AdminPool(t)
		if fired := r.runFire(t); fired != 1 {
			t.Fatalf("fire reported %d, want 1", fired)
		}
		_, threshold, _ := r.liveInvoices(t)
		if len(threshold) != 1 {
			t.Fatalf("%d threshold invoices after the fire, want 1", len(threshold))
		}
		if err := r.f.db.WithTenantTx(r.base, r.f.tenantID, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.base, `UPDATE subscriptions SET cancel_at = $1 WHERE id = $2`, r.tc, r.f.subID)
			return err
		}); err != nil {
			t.Fatalf("schedule cancel: %v", err)
		}
		svc := subscription.NewService(r.f.subStore, nil)
		svc.SetBiller(r.closer.engine)
		r.closer.engine.SetScheduledCancelExecutor(svc)
		invSvc := invoice.NewService(r.f.invStore, clock.Real(), r.f.settings)
		var pe probeErrs
		p := newCalcProbe(func(_ context.Context, call int) {
			if call != 1 {
				return
			}
			ctx, cancel := context.WithTimeout(r.at(r.tc), raceTimeout)
			defer cancel()
			if _, err := invSvc.Void(ctx, r.f.tenantID, threshold[0].ID); err != nil {
				pe.add("void of the threshold invoice while the scheduled cancel was priced: %v (it waited on the cancel's key share)", err)
			}
		})
		r.closer.engine.SetTaxProviderResolver(p)

		runCtx := leadertest.Token(t, admin, r.at(r.tc.Add(time.Hour)), leader.RoleBilling)
		_, errs := r.closer.engine.RunCycle(runCtx, 50, nil)
		pe.check(t)
		requireCanceledInTheRun(t, r.base, r.f.subStore, r.f.tenantID, r.f.subID, errs)
		if n := p.count(); n != 2 {
			t.Errorf("tax calculations = %d, want 2: the run must fire the cancel again", n)
		}
		_, _, other := r.liveInvoices(t)
		if len(other) != 1 {
			t.Fatalf("final invoices = %d, want 1", len(other))
		}
		var billed int64
		for _, l := range r.usageLines(t, other[0].ID) {
			billed += l.AmountCents
		}
		if billed != 400 {
			t.Errorf("final invoice usage = %dc, want 400c: the fire was voided first, so [P0, tc) is billed here", billed)
		}
	})

	t.Run("period moved", func(t *testing.T) {
		db := testutil.SetupTestDB(t)
		admin := testutil.AdminPool(t)
		r := newReliefCancelRig(t, db, "P1-23 scheduled replan: period", reliefRigOpts{usageUnits: 3000})
		r.scheduleCancel(t)
		r.engine.SetScheduledCancelExecutor(r.subSvc)
		var pe probeErrs
		p := newCalcProbe(func(ctx context.Context, call int) {
			if call != 1 {
				return
			}
			cctx, cancel := context.WithTimeout(ctx, raceTimeout)
			defer cancel()
			// A period writer re-stamps the period: next_billing_at moves by
			// an hour, the start and the lines stay.
			if err := r.db.WithTenantTx(cctx, r.tenantID, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(cctx, `UPDATE subscriptions SET next_billing_at = next_billing_at + interval '1 hour' WHERE id = $1`, r.subID)
				return err
			}); err != nil {
				pe.add("period write while the scheduled cancel was priced: %v (it waited on the cancel's lock)", err)
			}
		})
		r.engine.SetTaxProviderResolver(p)

		runCtx := leadertest.Token(t, admin, clock.WithEffectiveNow(r.ctx, r.cancelAt.Add(time.Hour)), leader.RoleBilling)
		_, errs := r.engine.RunCycle(runCtx, 50, nil)
		pe.check(t)
		requireCanceledInTheRun(t, r.ctx, subscription.NewPostgresStore(r.db), r.tenantID, r.subID, errs)
		reqs := p.requests()
		if len(reqs) != 2 {
			t.Fatalf("tax calculations = %d, want 2: the run must fire the cancel again", len(reqs))
		}
		if !reflect.DeepEqual(reqs[0], reqs[1]) {
			t.Errorf("the second tax request differs from the first:\n first %+v\nsecond %+v\nwant the same request (the same Stripe Tax key: the scope is the contracted cancel_at)", reqs[0], reqs[1])
		}
		if final := r.finalInvoice(t); final.TotalAmountCents != 3000 {
			t.Errorf("final invoice = %dc, want the 3000c of usage", final.TotalAmountCents)
		}
	})
}

// An immediate cancel of an in-arrears subscription on a tenant with a 10%
// tax rate: the final invoice's total and amount due include the tax.
//
// Mutation: drop `+ taxApp.TaxAmountCents` from the final invoice's total in
// writeFinalOnCancel → the total and amount due are the subtotal.
func TestCancel_InArrearsWithTax_FinalInvoiceTotalIncludesTax(t *testing.T) {
	db := testutil.SetupTestDB(t)
	r := newArrearsCancelRig(t, db, "P1-23 cancel total includes tax", 1, 10)
	if _, _, err := r.cancel(0); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	final, ok := r.finalInvoices(t)[r.subIDs[0]]
	if !ok {
		t.Fatal("the cancel wrote no final invoice")
	}
	const wantTax = arrearsFinalCents / 10
	if final.SubtotalCents != arrearsFinalCents || final.TaxAmountCents != wantTax {
		t.Fatalf("final invoice subtotal=%dc tax=%dc, want %dc and %dc (10%%)", final.SubtotalCents, final.TaxAmountCents, arrearsFinalCents, wantTax)
	}
	if final.TotalAmountCents != arrearsFinalCents+wantTax || final.AmountDueCents != arrearsFinalCents+wantTax {
		t.Errorf("final invoice total=%dc amount_due=%dc, want both %dc: the subtotal plus its tax",
			final.TotalAmountCents, final.AmountDueCents, arrearsFinalCents+wantTax)
	}
}

// The same check for the day-1 in-advance invoice, on a tenant with a 10% tax
// rate: its total and amount due include the tax. buildOnCreateInvoice builds
// it for a create, a trial's conversion and a cross-interval swap, so this
// covers the swap's invoice total too.
//
// Mutation: drop `+ taxApp.TaxAmountCents` from totalWithTax in
// buildOnCreateInvoice → the total and amount due are the subtotal.
func TestBillOnCreate_WithTax_InvoiceTotalIncludesTax(t *testing.T) {
	db := testutil.SetupTestDB(t)
	// sameDay leaves the day-1 invoice unpaid, so its amount due is its total.
	r := newReliefCancelRig(t, db, "P1-23 day-1 total includes tax", reliefRigOpts{sameDay: true, taxRate: 10})
	day1 := r.invoice(t, r.day1ID)
	const wantTax = reliefBaseCents / 10
	if day1.SubtotalCents != reliefBaseCents || day1.TaxAmountCents != wantTax {
		t.Fatalf("day-1 invoice subtotal=%dc tax=%dc, want %dc and %dc (10%%)", day1.SubtotalCents, day1.TaxAmountCents, reliefBaseCents, wantTax)
	}
	if day1.TotalAmountCents != reliefBaseCents+wantTax || day1.AmountDueCents != reliefBaseCents+wantTax {
		t.Errorf("day-1 invoice total=%dc amount_due=%dc, want both %dc: the subtotal plus its tax",
			day1.TotalAmountCents, day1.AmountDueCents, reliefBaseCents+wantTax)
	}
}

// readHookStore is the subscription store with onGet, when set, run before
// each read of a subscription, and onListIntervals, when set, before each
// read of its billing intervals off a transaction.
type readHookStore struct {
	*subscription.PostgresStore
	onGet           func(ctx context.Context)
	onListIntervals func(ctx context.Context)
}

func (s *readHookStore) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	if s.onGet != nil {
		s.onGet(ctx)
	}
	return s.PostgresStore.Get(ctx, tenantID, id)
}

func (s *readHookStore) ListItemIntervals(ctx context.Context, tenantID, subscriptionID string) ([]domain.ItemInterval, error) {
	if s.onListIntervals != nil {
		s.onListIntervals(ctx)
	}
	return s.PostgresStore.ListItemIntervals(ctx, tenantID, subscriptionID)
}

// cancelAtTheEnd is a dunning policy whose one retry is exhausted at once and
// whose final action cancels the subscription.
func cancelAtTheEnd() domain.DunningPolicy {
	return domain.DunningPolicy{
		Name: "cancel at the end", Enabled: true, RetryDays: []int{3},
		FinalSubscriptionAction: domain.SubActionCancel, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	}
}

// Dunning's final cancel, whose subscription changes while dunning cancels
// it. Dunning's context is bound to its instant, so both attempts of the
// cancel cancel at that instant (ADR-139).
//   - A quantity change while the cancel is priced: the cancel plans again
//     within dunning's attempt, and the run ends as after any cancel, not
//     action_failed. Mutation M6 (one attempt) → action_failed.
//   - The period re-anchored to start after dunning's instant, between
//     dunning's instant and the cancel's read: the cancel does not plan from
//     that row, on either attempt, and returns the error. The run is
//     labelled action_failed and kept for a re-attempt; the subscription is
//     still active, its prebill untouched, and no subscription.canceled is
//     queued. The re-attempt, at an instant in the new period, cancels.
//     Mutation M8 (drop the period-start rule) → the first attempt cancels
//     at dunning's instant with the new period unbilled and unrelieved.
//     The re-anchor is a write of the period columns in a store hook, the
//     effect of a cross-interval swap on the row: a real swap voids the run's
//     prebill, which resolves the run and leaves no re-attempt to watch.
func TestDunningFinalCancel_SnapshotMoved(t *testing.T) {
	t.Run("quantity changed while priced", func(t *testing.T) {
		r := newPrebillVoidRig(t, "P1-23 dunning cancel: quantity", cancelAtTheEnd())
		var pe probeErrs
		var hooked atomic.Bool
		r.pricing.onGetPlan = func() {
			if !hooked.CompareAndSwap(false, true) {
				return
			}
			ctx, cancel := context.WithTimeout(r.ctx, raceTimeout)
			defer cancel()
			q := int64(2)
			if _, err := r.subSvc.UpdateItem(ctx, r.tenantID, r.sub.ID, r.sub.Items[0].ID, subscription.UpdateItemInput{Quantity: &q, Immediate: true}); err != nil {
				pe.add("quantity change while dunning's cancel was priced: %v (it waited on the cancel's lock)", err)
			}
		}
		if _, errs := r.dsvc.ProcessDueRuns(r.ctx, r.tenantID, 50); len(errs) != 0 {
			t.Fatalf("dunning tick: %v", errs)
		}
		pe.check(t)
		sub, err := r.subStore.Get(r.ctx, r.tenantID, r.sub.ID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.Status != domain.SubscriptionCanceled || len(sub.Items) != 1 || sub.Items[0].Quantity != 2 {
			t.Fatalf("subscription status=%s items=%+v, want canceled with the changed quantity 2", sub.Status, sub.Items)
		}
		run, err := r.dstore.GetRun(r.ctx, r.tenantID, r.runID())
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		if run.Resolution == domain.ResolutionActionFailed {
			t.Errorf("run labelled action_failed: the cancel must plan again within dunning's attempt")
		}
	})

	t.Run("period re-anchored past dunning's instant", func(t *testing.T) {
		r := newPrebillVoidRig(t, "P1-23 dunning cancel: period moved", cancelAtTheEnd())
		r.subStore.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
		store := &readHookStore{PostgresStore: r.subStore}
		svc := subscription.NewService(store, clock.Real())
		svc.SetBiller(r.engine)
		r.dsvc.SetSubscriptionCanceler(finalActionMovers{svc: svc})

		var pe probeErrs
		var newStart time.Time
		var hooked atomic.Bool
		store.onGet = func(ctx context.Context) {
			if subscription.CancelOrigin(ctx) != "dunning" || !hooked.CompareAndSwap(false, true) {
				return
			}
			// Two seconds after dunning's instant: the period now starts after
			// the instant the cancel would cancel at.
			newStart = clock.Now(ctx).Add(2 * time.Second).Truncate(time.Microsecond)
			if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(r.ctx, `
					UPDATE subscriptions SET current_billing_period_start = $1,
					       current_billing_period_end = $1::timestamptz + interval '1 year',
					       next_billing_at = $1::timestamptz + interval '1 year'
					WHERE id = $2`, newStart, r.sub.ID)
				return err
			}); err != nil {
				pe.add("re-anchor the period: %v", err)
			}
		}
		if _, errs := r.dsvc.ProcessDueRuns(r.ctx, r.tenantID, 50); len(errs) != 0 {
			t.Fatalf("dunning tick: %v", errs)
		}
		pe.check(t)
		if !hooked.Load() {
			t.Fatal("dunning's cancel never read the subscription")
		}
		sub, err := r.subStore.Get(r.ctx, r.tenantID, r.sub.ID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.Status != domain.SubscriptionActive {
			t.Fatalf("subscription is %s, want still active: dunning's instant is before the period's start, and a cancel planned from that row bills and relieves nothing of it", sub.Status)
		}
		if day1 := r.invoice(r.day1.ID); day1.Status != domain.InvoiceFinalized {
			t.Errorf("day-1 prebill is %s, want untouched (finalized)", day1.Status)
		}
		if n := countTenantRows(t, r.ctx, r.db, r.tenantID, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1`, domain.EventSubscriptionCanceled); n != 0 {
			t.Errorf("subscription.canceled events = %d, want 0", n)
		}
		run, err := r.dstore.GetRun(r.ctx, r.tenantID, r.runID())
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		if run.State != domain.DunningActive || run.Resolution != domain.ResolutionActionFailed {
			t.Fatalf("run state=%s resolution=%q, want active/action_failed (kept for a re-attempt)", run.State, run.Resolution)
		}

		// The re-attempt, once the new period has begun: the run is due again
		// now rather than in 24 hours.
		time.Sleep(time.Until(newStart.Add(10 * time.Millisecond)))
		r.makeRunDueNow()
		if _, errs := r.dsvc.ProcessDueRuns(r.ctx, r.tenantID, 50); len(errs) != 0 {
			t.Fatalf("dunning re-attempt: %v", errs)
		}
		sub, err = r.subStore.Get(r.ctx, r.tenantID, r.sub.ID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if sub.Status != domain.SubscriptionCanceled || sub.CanceledAt == nil || sub.CanceledAt.Before(newStart) {
			t.Errorf("after the re-attempt: status=%s canceled_at=%v, want canceled at or after the period's start %v", sub.Status, sub.CanceledAt, newStart)
		}
	})
}
