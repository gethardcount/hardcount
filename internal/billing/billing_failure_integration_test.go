package billing_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
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

// faultyPricing fails GetPlan for one plan with a plain error, and counts how
// often that plan was asked for. The rows in the database stay well-formed;
// only the read is refused. An unchanged count across a run proves the
// subscription on that plan was not attempted.
type faultyPricing struct {
	*pricingStoreAdapter
	badPlanID string
	failing   atomic.Bool
	calls     atomic.Int64
	// onBad, if set, runs on each refused read before the error is returned.
	onBad func()
	// fail, if set, replaces the refused read's error (or panics).
	fail func() error
}

func (p *faultyPricing) GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error) {
	if id == p.badPlanID {
		p.calls.Add(1)
		if p.failing.Load() {
			if p.onBad != nil {
				p.onBad()
			}
			if p.fail != nil {
				return domain.Plan{}, p.fail()
			}
			return domain.Plan{}, errors.New("plan lookup refused (test)")
		}
	}
	return p.pricingStoreAdapter.GetPlan(ctx, tenantID, id)
}

// billFailureEnv is the shared fixture of the billing-failure tests: one
// tenant, a plan whose reads can be refused, a healthy plan, and a real engine
// on a fake clock.
type billFailureEnv struct {
	db       *postgres.DB
	admin    *sql.DB
	ctx      context.Context
	tenantID string
	custID   string
	planBad  domain.Plan
	planOK   domain.Plan
	subs     *subscription.PostgresStore
	invoices *invoice.PostgresStore
	pricing  *faultyPricing
	invs     *hookedInvoices
	clock    *clock.Fake
	engine   *billing.Engine

	periodStart, periodEnd time.Time
}

func newBillFailureEnv(t *testing.T, name string) *billFailureEnv {
	t.Helper()
	db := testutil.SetupTestDB(t) // skips under -short
	// One owner-role pool per test: AdminPool opens a new pool on every call.
	admin := testutil.AdminPool(t)
	ctx := leadertest.Token(t, admin, postgres.WithLivemode(context.Background(), false), leader.RoleBilling)
	env := &billFailureEnv{
		db: db, ctx: ctx, admin: admin,
		subs:        subscription.NewPostgresStore(db),
		invoices:    invoice.NewPostgresStore(db),
		periodStart: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		periodEnd:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	}
	env.tenantID = testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, env.tenantID, domain.Customer{ExternalID: "cus_billfail", DisplayName: "Bill Failure"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	env.custID = cust.ID
	pricingStore := pricing.NewPostgresStore(db)
	for code, dst := range map[string]*domain.Plan{"plan-bad": &env.planBad, "plan-ok": &env.planOK} {
		p, err := pricingStore.CreatePlan(ctx, env.tenantID, domain.Plan{
			Code: code, Name: code, Currency: "USD",
			BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 4900,
		})
		if err != nil {
			t.Fatalf("plan %s: %v", code, err)
		}
		*dst = p
	}
	env.pricing = &faultyPricing{pricingStoreAdapter: &pricingStoreAdapter{pricingStore}, badPlanID: env.planBad.ID}
	env.pricing.failing.Store(true)
	env.invs = &hookedInvoices{invoiceStoreAdapter: &invoiceStoreAdapter{env.invoices}}
	// T0: two seconds past the period end, so a subscription due at the end
	// and one due a second later are both due.
	env.clock = clock.NewFake(env.periodEnd.Add(2 * time.Second))
	env.engine = env.newEngine(&subStoreAdapter{env.subs}, env.pricing)
	return env
}

// newEngine builds an engine over the same stores and clock; tests use a
// second one for a concurrent or a later run, with its own subscription reader
// (to observe or fail the failure write) and pricing reader (to fail or not).
func (env *billFailureEnv) newEngine(subs billing.SubscriptionReader, pr billing.PricingReader) *billing.Engine {
	e := billing.NewEngine(
		subs, &usageStoreAdapter{usage.NewPostgresStore(env.db)},
		pr, env.invs,
		nil, tenant.NewSettingsStore(env.db), testPaymentSetupsNoPM{}, testChargerSentinel{}, env.clock,
	)
	e.SetIntervalReader(env.subs)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	return e
}

// dueSub creates an active wall-clock subscription on plan whose March period
// has ended and whose next_billing_at is `next`.
func (env *billFailureEnv) dueSub(t *testing.T, code string, plan domain.Plan, next time.Time) domain.Subscription {
	t.Helper()
	return env.subWith(t, domain.Subscription{Code: code, Status: domain.SubscriptionActive}, plan, env.periodStart, env.periodEnd, next)
}

// subWith creates a wall-clock subscription from tmpl on plan, with the
// period [start, end) and next_billing_at = next.
func (env *billFailureEnv) subWith(t *testing.T, tmpl domain.Subscription, plan domain.Plan, start, end, next time.Time) domain.Subscription {
	t.Helper()
	tmpl.DisplayName, tmpl.CustomerID = tmpl.Code, env.custID
	tmpl.Items = []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}}
	tmpl.BillingTime = domain.BillingTimeCalendar
	tmpl.StartedAt = &start
	tmpl.CurrentBillingPeriodStart, tmpl.CurrentBillingPeriodEnd = &start, bivPE(start)
	sub, err := env.subs.Create(env.ctx, env.tenantID, tmpl)
	if err != nil {
		t.Fatalf("subscription %s: %v", tmpl.Code, err)
	}
	subscriptiontest.SetBillingCycle(t, env.ctx, env.db, env.tenantID, sub.ID, start, end, next, 0)
	return sub
}

// hookedInvoices fails the invoice insert inside the closer transaction when
// onCreateTx returns an error. That is the write after the period CAS, so a
// failure here proves the CAS (and with it the void of a failure record)
// rolled back.
type hookedInvoices struct {
	*invoiceStoreAdapter
	onCreateTx func(inv domain.Invoice) error
}

func (a *hookedInvoices) CreateInvoiceWithLineItemsTx(ctx context.Context, tx *sql.Tx, tenantID string, inv domain.Invoice, items []domain.InvoiceLineItem) (domain.Invoice, error) {
	if a.onCreateTx != nil {
		if err := a.onCreateTx(inv); err != nil {
			return domain.Invoice{}, err
		}
	}
	return a.invoiceStoreAdapter.CreateInvoiceWithLineItemsTx(ctx, tx, tenantID, inv, items)
}

// observedSubs wraps the real store adapter to watch, fail, or interleave
// with the engine's calls: the concurrent-resolver seam of these tests.
type observedSubs struct {
	*subStoreAdapter
	// recordErr, if set, fails RecordBillingFailure without touching the row.
	recordErr error
	// afterFetch, if set, runs once after GetDueBilling has returned a
	// non-empty page and released its row locks, before the engine sees it.
	afterFetch func()
	fetched    sync.Once
	// beforeFetch, if set, runs before each GetDueBilling with its 1-based
	// call number.
	beforeFetch func(call int)
	fetches     atomic.Int32

	mu      sync.Mutex
	scanned []string // every id GetDueBilling returned, in order
	results []bool   // what each RecordBillingFailure call reported
	// writes counts every RecordBillingFailure call, including failed ones.
	writes atomic.Int32
}

func (a *observedSubs) GetDueBilling(ctx context.Context, before time.Time, exclude []string, limit int) ([]domain.Subscription, error) {
	if n := int(a.fetches.Add(1)); a.beforeFetch != nil {
		a.beforeFetch(n)
	}
	subs, err := a.subStoreAdapter.GetDueBilling(ctx, before, exclude, limit)
	a.mu.Lock()
	for _, sub := range subs {
		a.scanned = append(a.scanned, sub.ID)
	}
	a.mu.Unlock()
	if err == nil && len(subs) > 0 && a.afterFetch != nil {
		a.fetched.Do(a.afterFetch)
	}
	return subs, err
}

func (a *observedSubs) RecordBillingFailure(ctx context.Context, tenantID, id string, f subscription.BillingFailureWrite) (bool, error) {
	a.writes.Add(1)
	if a.recordErr != nil {
		return false, a.recordErr
	}
	ok, err := a.subStoreAdapter.RecordBillingFailure(ctx, tenantID, id, f)
	a.mu.Lock()
	a.results = append(a.results, ok)
	a.mu.Unlock()
	return ok, err
}

func (a *observedSubs) recorded() (landed, refused int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ok := range a.results {
		if ok {
			landed++
		} else {
			refused++
		}
	}
	return landed, refused
}

const blankBillingColumns = "-|0|-|-|-"

// captureLogs points the default logger at a buffer for the rest of the test
// and restores it afterwards. Restoring takes three steps, not one: installing
// a non-default slog handler also redirects the log package, and setting the
// previous (default) logger back does not undo that. Without the last two
// lines every later test in the binary would log into this test's buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &logs
}

// mustFail runs the scheduler's billing run and asserts that subID failed.
func (env *billFailureEnv) mustFail(t *testing.T, subID string) {
	t.Helper()
	_, errs := env.engine.RunCycle(env.ctx, 50, nil)
	onlyFailure(t, errs, subID)
}

func (env *billFailureEnv) get(t *testing.T, id string) domain.Subscription {
	t.Helper()
	sub, err := env.subs.Get(env.ctx, env.tenantID, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return sub
}

func (env *billFailureEnv) invoiceCount(t *testing.T, subID string) int {
	t.Helper()
	_, total, err := env.invoices.List(env.ctx, invoice.ListFilter{TenantID: env.tenantID, SubscriptionID: subID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	return total
}

// billingColumns reads the five saved columns as text, straight from the row,
// so a test can prove a run left them byte-identical. The timestamps are
// rendered in UTC with no offset, whatever the session's TimeZone is.
func (env *billFailureEnv) billingColumns(t *testing.T, subID string) string {
	t.Helper()
	var out string
	err := env.admin.QueryRowContext(env.ctx, `
		SELECT concat_ws('|', COALESCE((billing_failed_for AT TIME ZONE 'UTC')::text,'-'), billing_failure_count::text,
		       COALESCE((billing_failed_at AT TIME ZONE 'UTC')::text,'-'),
		       COALESCE((billing_next_attempt_at AT TIME ZONE 'UTC')::text,'-'), COALESCE(billing_last_error,'-'))
		  FROM subscriptions WHERE id = $1`, subID).Scan(&out)
	if err != nil {
		t.Fatalf("read billing columns of %s: %v", subID, err)
	}
	return out
}

// onlyFailure asserts errs is exactly one error naming subID.
func onlyFailure(t *testing.T, errs []error, subID string) {
	t.Helper()
	if len(errs) != 1 {
		t.Fatalf("run returned %d errors, want exactly 1 (for %s): %v", len(errs), subID, errs)
	}
	var sbe billing.SubBillError
	if !errors.As(errs[0], &sbe) || sbe.SubscriptionID != subID {
		t.Fatalf("run error does not name %s: %v", subID, errs[0])
	}
}

// TestBillingFailure_FailingSubIsSavedWaitsAndIsSkipped is the headline test
// of issue #960. Subscription A fails to bill on every attempt and sits at the
// head of the oldest-first due queue; B is healthy and due just after it.
//
//  1. The first run saves A's failure (attempt 1, next attempt in 15 minutes,
//     a generic error text) and still bills B.
//  2. A second run at the same instant does not attempt A at all.
//  3. Once the wait has passed A is attempted again and the count and wait grow.
//  4. Once the cause is fixed A bills, and the record is gone in the same read.
//
// Mutation checks: drop the gate clause from GetDueBilling → step 2 fails
// (the scan returns A).
func TestBillingFailure_FailingSubIsSavedWaitsAndIsSkipped(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Saved")
	a := env.dueSub(t, "sub-a-failing", env.planBad, env.periodEnd)
	b := env.dueSub(t, "sub-b-healthy", env.planOK, env.periodEnd.Add(time.Second))
	t0 := env.clock.Now(env.ctx)

	// Run 1.
	_, errs := env.engine.RunCycle(env.ctx, 50, nil)
	onlyFailure(t, errs, a.ID)
	if n := env.invoiceCount(t, b.ID); n != 1 {
		t.Fatalf("healthy subscription behind the failing one has %d invoices after run 1, want 1", n)
	}
	bf := env.get(t, a.ID).BillingFailure
	if bf == nil {
		t.Fatal("run 1 saved no billing failure on the failing subscription: Get().BillingFailure is nil")
	}
	if bf.Attempts != 1 || bf.MaxAttempts != domain.MaxBillingAttempts || bf.Parked {
		t.Fatalf("after run 1: attempts=%d max=%d parked=%v, want 1/%d/false", bf.Attempts, bf.MaxAttempts, bf.Parked, domain.MaxBillingAttempts)
	}
	if !bf.FailedAt.Equal(t0) {
		t.Fatalf("failed_at = %s, want the run's clock instant %s", bf.FailedAt, t0)
	}
	if bf.NextAttemptAt == nil || !bf.NextAttemptAt.Equal(t0.Add(15*time.Minute)) {
		t.Fatalf("next_attempt_at = %v, want exactly %s (first rung, 15 minutes)", bf.NextAttemptAt, t0.Add(15*time.Minute))
	}
	if !strings.Contains(bf.LastError, a.ID) || strings.Contains(bf.LastError, "plan lookup refused") {
		t.Fatalf("last_error = %q, want the generic reference naming %s and no raw error text", bf.LastError, a.ID)
	}

	// Run 2, same instant: A is waiting, so it is not attempted. The scan
	// itself must leave it out: the attempt-time re-check would also stop the
	// attempt, but a waiting subscription that the scan still returns keeps
	// filling the head of every oldest-first page.
	watched := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	callsBefore, colsBefore := env.pricing.calls.Load(), env.billingColumns(t, a.ID)
	gen, errs := env.newEngine(watched, env.pricing).RunCycle(env.ctx, 50, nil)
	if len(errs) != 0 || gen != 0 {
		t.Fatalf("run 2 at the same instant: generated=%d errors=%v, want 0 and none (A is waiting, B is billed)", gen, errs)
	}
	if len(watched.scanned) != 0 {
		t.Fatalf("run 2's due scan returned %v: a waiting subscription must be skipped by the scan", watched.scanned)
	}
	if got := env.pricing.calls.Load(); got != callsBefore {
		t.Fatalf("run 2 attempted the waiting subscription: plan reads went %d → %d", callsBefore, got)
	}
	if got := env.billingColumns(t, a.ID); got != colsBefore {
		t.Fatalf("run 2 changed the saved record:\n before %s\n after  %s", colsBefore, got)
	}

	// Run 3, the wait has passed: attempted again, attempt 2, one-hour wait.
	env.clock.Advance(15 * time.Minute)
	t1 := env.clock.Now(env.ctx)
	_, errs = env.engine.RunCycle(env.ctx, 50, nil)
	onlyFailure(t, errs, a.ID)
	bf = env.get(t, a.ID).BillingFailure
	if bf == nil || bf.Attempts != 2 || bf.NextAttemptAt == nil || !bf.NextAttemptAt.Equal(t1.Add(time.Hour)) {
		t.Fatalf("after run 3: %+v, want attempts 2 and next attempt at %s", bf, t1.Add(time.Hour))
	}

	// Run 4, the cause is fixed and the wait has passed: A bills, record gone.
	env.pricing.failing.Store(false)
	env.clock.Advance(time.Hour)
	_, errs = env.engine.RunCycle(env.ctx, 50, nil)
	if len(errs) != 0 {
		t.Fatalf("run 4 after the fix: %v", errs)
	}
	if n := env.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("fixed subscription has %d invoices, want exactly 1", n)
	}
	if bf := env.get(t, a.ID).BillingFailure; bf != nil {
		t.Fatalf("a clean bill left the failure record readable: %+v", bf)
	}
}

// billLadder is the wait after each counted failure, written out here so the
// test names the rungs independently of the engine's table.
var billLadder = []time.Duration{15 * time.Minute, time.Hour, 4 * time.Hour, 12 * time.Hour, 24 * time.Hour, 24 * time.Hour, 24 * time.Hour}

// TestBillingFailure_ParksAtTheEighthFailure steps the clock through the whole
// ladder. Each wait equals its rung exactly; the eighth counted failure writes
// no next attempt, and from then on neither the scheduler's run nor the
// operator's POST /v1/billing/run attempts the subscription. The park is
// logged once.
func TestBillingFailure_ParksAtTheEighthFailure(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Parks")
	a := env.dueSub(t, "sub-a-failing", env.planBad, env.periodEnd)
	b := env.dueSub(t, "sub-b-healthy", env.planOK, env.periodEnd.Add(time.Second))

	logs := captureLogs(t)

	first := env.clock.Now(env.ctx)
	for n := 1; n <= domain.MaxBillingAttempts; n++ {
		now := env.clock.Now(env.ctx)
		env.mustFail(t, a.ID)
		bf := env.get(t, a.ID).BillingFailure
		if bf == nil || bf.Attempts != n || !bf.FailedAt.Equal(now) {
			t.Fatalf("after failure %d: %+v", n, bf)
		}
		if n == domain.MaxBillingAttempts {
			if !bf.Parked || bf.NextAttemptAt != nil {
				t.Fatalf("after failure %d: %+v, want parked with no next attempt", n, bf)
			}
			break
		}
		if bf.Parked || bf.NextAttemptAt == nil || bf.NextAttemptAt.Sub(now) != billLadder[n-1] {
			t.Fatalf("after failure %d: next attempt %v, want exactly now + %s", n, bf.NextAttemptAt, billLadder[n-1])
		}
		// One nanosecond short of the wait: still skipped.
		env.clock.Advance(billLadder[n-1] - time.Nanosecond)
		calls := env.pricing.calls.Load()
		if _, errs := env.engine.RunCycle(env.ctx, 50, nil); len(errs) != 0 || env.pricing.calls.Load() != calls {
			t.Fatalf("a run before rung %d elapsed attempted the subscription (errors %v)", n, errs)
		}
		env.clock.Advance(time.Nanosecond)
	}
	if got, want := env.clock.Now(env.ctx).Sub(first), 3*24*time.Hour+17*time.Hour+15*time.Minute; got != want {
		t.Fatalf("time from the first failure to the park = %s, want %s", got, want)
	}
	if n := env.invoiceCount(t, b.ID); n != 1 {
		t.Fatalf("the healthy subscription has %d invoices, want 1", n)
	}

	// Parked: no run attempts it, however long it waits.
	env.clock.Advance(48 * time.Hour)
	calls, cols := env.pricing.calls.Load(), env.billingColumns(t, a.ID)
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 0 || len(errs) != 0 {
		t.Fatalf("scheduler run on a parked subscription: generated=%d errors=%v", gen, errs)
	}
	if gen, failures := env.engine.RunCycleForTenant(env.ctx, env.tenantID, 50); gen != 0 || len(failures) != 0 {
		t.Fatalf("manual run on a parked subscription: generated=%d failures=%v", gen, failures)
	}
	if env.pricing.calls.Load() != calls || env.billingColumns(t, a.ID) != cols {
		t.Fatal("a run attempted or rewrote a parked subscription")
	}
	if n := strings.Count(logs.String(), "subscription billing parked"); n != 1 {
		t.Fatalf("the park was logged %d times, want once", n)
	}
}

// TestBillingFailure_CleanBillVoidsWithTheClose proves the record needs no
// clear: the period advance that commits the invoice is what voids it, and a
// close that rolls back leaves it standing.
func TestBillingFailure_CleanBillVoidsWithTheClose(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Voids")
	a := env.dueSub(t, "sub-a-failing", env.planBad, env.periodEnd)
	for n := 1; n <= 3; n++ {
		env.mustFail(t, a.ID)
		env.clock.Advance(billLadder[n-1])
	}
	if bf := env.get(t, a.ID).BillingFailure; bf == nil || bf.Attempts != 3 {
		t.Fatalf("setup: %+v, want attempts 3", bf)
	}

	// Shared fate: the plan read is fixed, but the invoice insert fails inside
	// the closer transaction, after the period CAS. The advance rolls back, so
	// the due instant did not move and the record is still live. This attempt
	// failed too, so it counts: 4, not a restart at 1.
	env.pricing.failing.Store(false)
	env.invs.onCreateTx = func(domain.Invoice) error { return errors.New("injected: invoice insert failed inside the closer tx") }
	env.mustFail(t, a.ID)
	sub := env.get(t, a.ID)
	if sub.NextBillingAt == nil || !sub.NextBillingAt.Equal(env.periodEnd) {
		t.Fatalf("the period moved on a rolled-back close: next_billing_at=%v", sub.NextBillingAt)
	}
	if sub.BillingFailure == nil || sub.BillingFailure.Attempts != 4 {
		t.Fatalf("after a rolled-back close: %+v, want the record live at attempts 4", sub.BillingFailure)
	}
	if n := env.invoiceCount(t, a.ID); n != 0 {
		t.Fatalf("%d invoices survived the rolled-back close", n)
	}

	// The clean bill: one invoice, and the record is gone in the same read.
	env.invs.onCreateTx = nil
	env.clock.Advance(billLadder[3])
	if _, errs := env.engine.RunCycle(env.ctx, 50, nil); len(errs) != 0 {
		t.Fatalf("clean run: %v", errs)
	}
	sub = env.get(t, a.ID)
	if n := env.invoiceCount(t, a.ID); n != 1 || sub.BillingFailure != nil {
		t.Fatalf("after the clean bill: invoices=%d failure=%+v, want 1 and nil", n, sub.BillingFailure)
	}
	// Nothing cleared the columns; they are stale and read as nothing.
	if cols := env.billingColumns(t, a.ID); cols == blankBillingColumns {
		t.Fatal("the columns were cleared: the design voids by the due instant, not by a clear")
	}
}

// TestBillingFailure_MultiPeriodCatchupRecordsTheFailingPeriod: three periods
// are overdue and the second fails. The first invoice stays, and the record is
// keyed to the second period's due instant, not to the one the page showed.
func TestBillingFailure_MultiPeriodCatchupRecordsTheFailingPeriod(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Catchup")
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feb, mar := jan.AddDate(0, 1, 0), jan.AddDate(0, 2, 0)
	a := env.subWith(t, domain.Subscription{Code: "sub-three-periods", Status: domain.SubscriptionActive}, env.planOK, jan, feb, feb)

	inserts := 0
	env.invs.onCreateTx = func(domain.Invoice) error {
		inserts++
		if inserts == 2 {
			return errors.New("injected: second period's invoice insert failed")
		}
		return nil
	}
	env.mustFail(t, a.ID)
	if n := env.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices, want the first period's 1", n)
	}
	sub := env.get(t, a.ID)
	if sub.BillingFailure == nil || sub.BillingFailure.Attempts != 1 {
		t.Fatalf("failure = %+v, want attempts 1", sub.BillingFailure)
	}
	if sub.NextBillingAt == nil || !sub.NextBillingAt.Equal(mar) {
		t.Fatalf("next_billing_at = %v, want the second period's close %s", sub.NextBillingAt, mar)
	}
	if cols := env.billingColumns(t, a.ID); !strings.HasPrefix(cols, "2026-03-01 00:00:00|1|") {
		t.Fatalf("record = %s, want it keyed to the second period's due instant 2026-03-01", cols)
	}

	// Fixed: the next attempt bills the two periods still owed.
	env.clock.Advance(billLadder[0])
	if _, errs := env.engine.RunCycle(env.ctx, 50, nil); len(errs) != 0 {
		t.Fatalf("catch-up run: %v", errs)
	}
	if n := env.invoiceCount(t, a.ID); n != 3 {
		t.Fatalf("%d invoices after the catch-up, want 3 (one per period)", n)
	}
}

// TestBillingFailure_TrialFlipCommitsThenCloseFails_RecordedSameRun: the trial
// flip commits in its own transaction and the same call then fails the cycle
// close. The row was read as trialing and is now active; the due instant did
// not move, so the failure is recorded on that same run.
func TestBillingFailure_TrialFlipCommitsThenCloseFails_RecordedSameRun(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Trial Flip")
	trialStart, trialEnd := env.periodStart, env.periodStart.AddDate(0, 0, 14)
	a := env.subWith(t, domain.Subscription{
		Code: "sub-trial", Status: domain.SubscriptionTrialing, TrialStartAt: &trialStart, TrialEndAt: &trialEnd,
	}, env.planOK, env.periodStart, env.periodEnd, env.periodEnd)

	env.invs.onCreateTx = func(domain.Invoice) error { return errors.New("injected: cycle close failed after the trial flip") }
	env.mustFail(t, a.ID)
	sub := env.get(t, a.ID)
	if sub.Status != domain.SubscriptionActive {
		t.Fatalf("status = %s, want active: the trial flip commits before the close", sub.Status)
	}
	if sub.BillingFailure == nil || sub.BillingFailure.Attempts != 1 {
		t.Fatalf("failure = %+v, want it recorded with attempts 1 in the run that flipped the trial", sub.BillingFailure)
	}
}

// TestBillingFailure_MidPeriodCancelFails_ClearingTheCancelVoids: the failing
// work is a mid-period cancel at cancel_at, not the close at next_billing_at.
// Un-scheduling the cancel removes that work, so the record is void and the
// close bills when it comes due. A record keyed on next_billing_at alone would
// still gate that close.
func TestBillingFailure_MidPeriodCancelFails_ClearingTheCancelVoids(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Mid-period Cancel")
	apr, may := env.periodEnd, env.periodEnd.AddDate(0, 1, 0)
	// The clock sits in the April period; the close at May 1 is not due.
	env.clock.Set(apr.AddDate(0, 0, 10))
	a := env.subWith(t, domain.Subscription{Code: "sub-cancel", Status: domain.SubscriptionActive}, env.planOK, apr, may, may)
	cancelAt := apr.AddDate(0, 0, 5)
	if _, err := env.subs.ScheduleCancellation(env.ctx, env.tenantID, a.ID, &cancelAt, false, domain.SubscriptionActive); err != nil {
		t.Fatalf("schedule cancel: %v", err)
	}
	// This engine has no ScheduledCancelExecutor wired, so the due cancel
	// fails loud on every attempt: a deterministic failure of the cancel.
	for n := 1; n <= domain.MaxBillingAttempts; n++ {
		env.mustFail(t, a.ID)
		if n < domain.MaxBillingAttempts {
			env.clock.Advance(billLadder[n-1])
		}
	}
	bf := env.get(t, a.ID).BillingFailure
	if bf == nil || !bf.Parked {
		t.Fatalf("setup: %+v, want the failing cancel parked", bf)
	}
	if cols := env.billingColumns(t, a.ID); !strings.HasPrefix(cols, cancelAt.UTC().Format("2006-01-02 15:04:05")+"|8|") {
		t.Fatalf("record = %s, want it keyed to cancel_at %s", cols, cancelAt)
	}

	if _, err := env.subs.ClearScheduledCancellation(env.ctx, env.tenantID, a.ID); err != nil {
		t.Fatalf("clear cancel: %v", err)
	}
	if bf := env.get(t, a.ID).BillingFailure; bf != nil {
		t.Fatalf("clearing the failing cancel left the record live: %+v", bf)
	}
	env.clock.Set(may.Add(time.Second))
	if _, errs := env.engine.RunCycle(env.ctx, 50, nil); len(errs) != 0 {
		t.Fatalf("close after the cancel was cleared: %v", errs)
	}
	if n := env.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices for the April period, want 1: a park for the cancel must not gate the close", n)
	}
}

// TestBillingFailure_UncountedFailuresSaveNothing: an interrupted run and a
// database error that took the connection or the transaction away say nothing
// about the subscription. Nothing is saved, no wait starts, and the very next
// run attempts it. A statement timeout on a live run does count.
//
// "Saves nothing" is asserted as "the failure write is not even attempted". A
// write on a cancelled ctx would fail anyway, so the row alone cannot tell a
// classifier that skipped the write from one that tried it and logged an
// error on every shutdown.
//
// Mutation seams: make classifyBillFailure ignore ctx.Err() → the two
// "interrupted" legs fail; make postgres.IsTransient return false → the two
// database legs fail.
func TestBillingFailure_UncountedFailuresSaveNothing(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Uncounted")
	a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)
	watched := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	env.engine = env.newEngine(watched, env.pricing)

	attempted := func(t *testing.T, run func()) {
		t.Helper()
		before := env.pricing.calls.Load()
		run()
		if got := env.pricing.calls.Load(); got != before+1 {
			t.Fatalf("plan reads went %d → %d, want exactly one attempt (no wait from the previous leg)", before, got)
		}
	}
	saveNothing := func(t *testing.T) {
		t.Helper()
		if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
			t.Fatalf("an uncounted failure was saved: %s", cols)
		}
		if bf := env.get(t, a.ID).BillingFailure; bf != nil {
			t.Fatalf("an uncounted failure reads as a record: %+v", bf)
		}
		if n := watched.writes.Load(); n != 0 {
			t.Fatalf("the failure write was attempted %d times for an uncounted failure", n)
		}
	}

	t.Run("interrupted: run ctx cancelled mid-bill", func(t *testing.T) {
		ctx, cancel := context.WithCancel(env.ctx)
		defer cancel()
		env.pricing.onBad = cancel
		attempted(t, func() {
			if _, errs := env.engine.RunCycle(ctx, 50, nil); len(errs) == 0 {
				t.Fatal("an interrupted run returned no error")
			}
		})
		saveNothing(t)
	})
	t.Run("interrupted: lease lost mid-bill", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(env.ctx)
		defer cancel(nil)
		env.pricing.onBad = func() { cancel(leader.ErrLeaseLost) }
		attempted(t, func() {
			if _, errs := env.engine.RunCycle(ctx, 50, nil); len(errs) == 0 {
				t.Fatal("a run that lost its lease returned no error")
			}
		})
		saveNothing(t)
	})
	env.pricing.onBad = nil
	for _, code := range []string{"08006", "40P01"} {
		t.Run("transient: SQLSTATE "+code, func(t *testing.T) {
			env.pricing.fail = func() error { return fmt.Errorf("get plan: %w", &pgconn.PgError{Code: code, Message: "injected"}) }
			attempted(t, func() { env.mustFail(t, a.ID) })
			saveNothing(t)
		})
	}
	t.Run("counted: statement timeout on a live run", func(t *testing.T) {
		env.pricing.fail = func() error {
			return fmt.Errorf("get plan: %w", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"})
		}
		attempted(t, func() { env.mustFail(t, a.ID) })
		if bf := env.get(t, a.ID).BillingFailure; bf == nil || bf.Attempts != 1 {
			t.Fatalf("a statement timeout was not counted: %+v", bf)
		}
		if n := watched.writes.Load(); n != 1 {
			t.Fatalf("%d failure writes for one counted failure, want 1", n)
		}
	})
}

// TestBillingFailure_LateFailureCannotLandOnABilledPeriod is the
// concurrent-resolver test for the due-instant guard. While engine 1 is inside
// its failing attempt, engine 2 bills the same period. Engine 1 then returns
// its error. The failure must not be saved: the work it failed on is done, and
// a record written now would sit on the NEXT period, which never failed.
//
// Mutation seam: drop `<due> IS NOT DISTINCT FROM $2` from RecordBillingFailure
// → the write lands and the subscription reads as failing.
func TestBillingFailure_LateFailureCannotLandOnABilledPeriod(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Late")
	a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)

	watched := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	loser := env.newEngine(watched, env.pricing)
	winner := env.newEngine(&subStoreAdapter{env.subs}, &pricingStoreAdapter{pricing.NewPostgresStore(env.db)})
	var once sync.Once
	env.pricing.onBad = func() {
		once.Do(func() {
			if gen, failures := winner.RunCycleForTenant(env.ctx, env.tenantID, 50); gen != 1 || len(failures) != 0 {
				t.Errorf("the winning run: generated=%d failures=%v, want 1 and none", gen, failures)
			}
		})
	}
	_, errs := loser.RunCycle(env.ctx, 50, nil)
	onlyFailure(t, errs, a.ID)

	if n := env.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices, want exactly 1", n)
	}
	if landed, refused := watched.recorded(); landed != 0 || refused != 1 {
		t.Fatalf("failure writes: %d landed, %d refused; want the one late write refused", landed, refused)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("a failure landed on a period that was already billed: %s", cols)
	}
	if bf := env.get(t, a.ID).BillingFailure; bf != nil {
		t.Fatalf("the next period reads as failing though it never failed: %+v", bf)
	}
}

// TestBillingFailure_TwoRunsFailTogether_CountOnce: the scheduler's run and a
// manual run fail the same attempt of the same subscription. Both read
// attempts 0 before either writes. One write lands; the other is refused by
// the observed-count guard, so one attempt is one count.
//
// Mutation seam: drop the observed-count CASE from RecordBillingFailure → both
// writes land.
func TestBillingFailure_TwoRunsFailTogether_CountOnce(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Together")
	a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)

	watched := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	one, two := env.newEngine(watched, env.pricing), env.newEngine(watched, env.pricing)

	// Barrier at the failing dependency: neither attempt returns its error
	// until both have read the row. The second run starts only once the first
	// is parked at the barrier, so the first's fetch has released its lock.
	var arrivals atomic.Int32
	firstIn, both := make(chan struct{}), make(chan struct{})
	env.pricing.onBad = func() {
		switch arrivals.Add(1) {
		case 1:
			close(firstIn)
		case 2:
			close(both)
		}
		select {
		case <-both:
		case <-time.After(20 * time.Second):
			t.Error("the two attempts never met at the barrier")
		}
	}
	// Results are checked on the test goroutine: t.Fatalf must not run on
	// another one.
	var wg sync.WaitGroup
	var oneErrs []error
	wg.Go(func() { _, oneErrs = one.RunCycle(env.ctx, 50, nil) })
	<-firstIn
	wg.Go(func() {
		if _, failures := two.RunCycleForTenant(env.ctx, env.tenantID, 50); len(failures) != 1 {
			t.Errorf("manual run failures = %v, want 1", failures)
		}
	})
	wg.Wait()
	onlyFailure(t, oneErrs, a.ID)

	if landed, refused := watched.recorded(); landed != 1 || refused != 1 {
		t.Fatalf("failure writes: %d landed, %d refused; want exactly one to land", landed, refused)
	}
	if bf := env.get(t, a.ID).BillingFailure; bf == nil || bf.Attempts != 1 {
		t.Fatalf("two runs failing one attempt: %+v, want attempts 1", bf)
	}
}

// TestBillingFailure_AttemptFromAStalePageIsNotMade: a run fetches its page,
// then another run fails the subscription and saves the wait. The first run
// still holds the row from before. The re-check after billSubscription's own
// read stops it, so the attempt is not made and the count stays 1.
//
// Mutation seam: remove the BillingFailure check at the top of
// billSubscription's loop → the stale run attempts and the count becomes 2.
func TestBillingFailure_AttemptFromAStalePageIsNotMade(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Stale Page")
	a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)

	other := env.newEngine(&subStoreAdapter{env.subs}, env.pricing)
	stale := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	stale.afterFetch = func() {
		if _, failures := other.RunCycleForTenant(env.ctx, env.tenantID, 50); len(failures) != 1 {
			t.Errorf("the other run: failures=%v, want the one failure", failures)
		}
	}
	gen, errs := env.newEngine(stale, env.pricing).RunCycle(env.ctx, 50, nil)
	if gen != 0 || len(errs) != 0 {
		t.Fatalf("the run holding the stale page: generated=%d errors=%v, want nothing attempted", gen, errs)
	}
	if got := env.pricing.calls.Load(); got != 1 {
		t.Fatalf("plan was read %d times, want 1 (only the other run's attempt)", got)
	}
	if bf := env.get(t, a.ID).BillingFailure; bf == nil || bf.Attempts != 1 {
		t.Fatalf("failure = %+v, want attempts 1", bf)
	}
}

// TestBillingFailure_AttemptFromAStalePageOfAParkedSubIsNotMade is the parked
// arm of the same re-check. The page is fetched at count 7, another run
// records the eighth failure and parks, and the stale run then reaches the
// subscription. A parked record has no next attempt at all, so the gate must
// test for that before it compares instants.
//
// Mutation seam: drop the `NextAttemptAt == nil` arm of the check → a nil
// pointer dereference here.
func TestBillingFailure_AttemptFromAStalePageOfAParkedSubIsNotMade(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Stale Parked")
	a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)
	for n := 1; n < domain.MaxBillingAttempts; n++ {
		env.mustFail(t, a.ID)
		env.clock.Advance(billLadder[n-1])
	}
	if bf := env.get(t, a.ID).BillingFailure; bf == nil || bf.Attempts != 7 || bf.Parked {
		t.Fatalf("setup: %+v, want seven failures and due again", bf)
	}
	calls := env.pricing.calls.Load()

	other := env.newEngine(&subStoreAdapter{env.subs}, env.pricing)
	stale := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	stale.afterFetch = func() {
		if _, failures := other.RunCycleForTenant(env.ctx, env.tenantID, 50); len(failures) != 1 {
			t.Errorf("the other run: failures=%v, want the one failure", failures)
		}
	}
	gen, errs := env.newEngine(stale, env.pricing).RunCycle(env.ctx, 50, nil)
	if gen != 0 || len(errs) != 0 {
		t.Fatalf("the run holding the stale page: generated=%d errors=%v, want nothing attempted", gen, errs)
	}
	if got := env.pricing.calls.Load(); got != calls+1 {
		t.Fatalf("plan reads went %d → %d, want exactly the other run's one attempt", calls, got)
	}
	if bf := env.get(t, a.ID).BillingFailure; bf == nil || bf.Attempts != 8 || !bf.Parked || bf.NextAttemptAt != nil {
		t.Fatalf("failure = %+v, want parked at 8", bf)
	}
}

// TestBillingFailure_PanicAfterAnInvoiceLogsTheRealCount: three periods are
// overdue and the second panics. One invoice was committed in that call, and
// the failure line must say so; the named result of the attempt is never
// assigned when billSubscription panics. The record is keyed to the period
// that panicked.
func TestBillingFailure_PanicAfterAnInvoiceLogsTheRealCount(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Panic Count")
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feb := jan.AddDate(0, 1, 0)
	a := env.subWith(t, domain.Subscription{Code: "sub-three-periods", Status: domain.SubscriptionActive}, env.planOK, jan, feb, feb)
	inserts := 0
	env.invs.onCreateTx = func(domain.Invoice) error {
		if inserts++; inserts == 2 {
			panic("boom in the second period")
		}
		return nil
	}
	logs := captureLogs(t)
	recovered := func() (r any) {
		defer func() { r = recover() }()
		env.engine.RunCycle(env.ctx, 50, nil)
		return nil
	}()
	if recovered == nil {
		t.Fatal("the panic was swallowed")
	}
	if n := env.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices, want the first period's 1", n)
	}
	if !strings.Contains(logs.String(), "invoices_before_error=1") {
		t.Fatalf("the failure line does not report the invoice committed before the panic:\n%s", logs.String())
	}
	if cols := env.billingColumns(t, a.ID); !strings.HasPrefix(cols, "2026-03-01 00:00:00|1|") {
		t.Fatalf("record = %s, want it keyed to the second period's due instant", cols)
	}
}

// TestBillingFailure_GateDeclinedSubIsNotCountedAsNoProgress: a run that the
// attempt-time gate stopped has not attempted the subscription. If the record
// is then cleared while that run is still draining (an operator retry whose
// own attempt did not complete), the subscription comes back on the run's next
// page. It must not be saved as "made no progress": the run never tried it.
// The run leaves it for the next run, as it does an uncounted failure.
func TestBillingFailure_GateDeclinedSubIsNotCountedAsNoProgress(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Gate Declined")
	a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)

	other := env.newEngine(&subStoreAdapter{env.subs}, env.pricing)
	stale := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	stale.afterFetch = func() {
		if _, failures := other.RunCycleForTenant(env.ctx, env.tenantID, 50); len(failures) != 1 {
			t.Errorf("the other run: failures=%v, want the one failure", failures)
		}
	}
	stale.beforeFetch = func(call int) {
		if call != 2 {
			return
		}
		// Between the stale run's pages the record is cleared and nothing
		// re-records it: the subscription is due again with no record.
		if _, err := env.subs.ClearBillingFailure(env.ctx, env.tenantID, a.ID, nil); err != nil {
			t.Errorf("clear between pages: %v", err)
		}
	}
	gen, errs := env.newEngine(stale, env.pricing).RunCycle(env.ctx, 50, nil)
	if gen != 0 || len(errs) != 0 {
		t.Fatalf("the run that never attempted the subscription: generated=%d errors=%v, want nothing reported", gen, errs)
	}
	if got := env.pricing.calls.Load(); got != 1 {
		t.Fatalf("plan was read %d times, want 1 (only the other run's attempt)", got)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("a failure was saved for a subscription this run never attempted: %s", cols)
	}
}

// TestBillingFailure_StillDueAfterACleanAttemptIsCounted: a subscription whose
// attempt returns cleanly but leaves it due used to be logged on every run and
// nothing else. It is now a counted failure with its own sentence, so a
// subscription that never makes progress parks. Here every close loses the
// period CAS, which billSubscription absorbs and returns cleanly from.
func TestBillingFailure_StillDueAfterACleanAttemptIsCounted(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure No Progress")
	a := env.dueSub(t, "sub-a", env.planOK, env.periodEnd)
	stuck := &failableSubAdapter{subStoreAdapter: &subStoreAdapter{env.subs}, closePeriodTxErr: subscription.ErrWatermarkMoved}
	_, errs := env.newEngine(stuck, env.pricing).RunCycle(env.ctx, 50, nil)
	onlyFailure(t, errs, a.ID)
	bf := env.get(t, a.ID).BillingFailure
	if bf == nil || bf.Attempts != 1 || !strings.HasPrefix(bf.LastError, "Billing made no progress") {
		t.Fatalf("failure = %+v, want attempts 1 with the no-progress text", bf)
	}
}

// TestBillingFailure_PanicIsRecordedThenRepanics: a panic while billing one
// subscription is saved as a counted failure and then re-raised, so the tick
// still dies loudly. The next run skips that subscription and bills the
// healthy one behind it, which the panic used to block on every tick.
func TestBillingFailure_PanicIsRecordedThenRepanics(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Panic")
	a := env.dueSub(t, "sub-a-panics", env.planBad, env.periodEnd)
	b := env.dueSub(t, "sub-b-healthy", env.planOK, env.periodEnd.Add(time.Second))
	env.pricing.fail = func() error { panic("boom: secret panic value idx_secret") }

	recovered := func() (r any) {
		defer func() { r = recover() }()
		env.engine.RunCycle(env.ctx, 50, nil)
		return nil
	}()
	if recovered == nil {
		t.Fatal("RunCycle swallowed the panic: the tick must still die so the scheduler reports it")
	}
	bf := env.get(t, a.ID).BillingFailure
	if bf == nil || bf.Attempts != 1 {
		t.Fatalf("failure = %+v, want the panic recorded with attempts 1", bf)
	}
	if !strings.Contains(bf.LastError, a.ID) || strings.Contains(bf.LastError, "boom") || strings.Contains(bf.LastError, "idx_secret") {
		t.Fatalf("last_error = %q, want the generic reference and no panic text", bf.LastError)
	}
	if n := env.invoiceCount(t, b.ID); n != 0 {
		t.Fatalf("setup: the healthy subscription was billed before the panic (%d invoices)", n)
	}

	calls := env.pricing.calls.Load()
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 1 || len(errs) != 0 {
		t.Fatalf("the next run: generated=%d errors=%v, want the healthy subscription billed", gen, errs)
	}
	if env.pricing.calls.Load() != calls {
		t.Fatal("the next run attempted the subscription that panicked")
	}
	if n := env.invoiceCount(t, b.ID); n != 1 {
		t.Fatalf("the healthy subscription has %d invoices, want 1", n)
	}
}

// TestBillingFailure_RecordWriteFails_RunEnds_NextLeaderRecords: when the
// failure write itself fails, the run must not loop or lose the real error.
// The in-memory exclude list ends the run, healthy subscriptions bill, the
// row is unchanged, and the next run (a new leader) attempts and records.
func TestBillingFailure_RecordWriteFails_RunEnds_NextLeaderRecords(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Write Fails")
	a := env.dueSub(t, "sub-a-failing", env.planBad, env.periodEnd)
	b := env.dueSub(t, "sub-b-healthy", env.planOK, env.periodEnd.Add(time.Second))

	broken := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}, recordErr: errors.New("injected: failure write failed")}
	_, errs := env.newEngine(broken, env.pricing).RunCycle(env.ctx, 50, nil)
	onlyFailure(t, errs, a.ID)
	if !strings.Contains(errs[0].Error(), "plan lookup refused") {
		t.Fatalf("the run returned %v, want the original billing error", errs[0])
	}
	if n := env.invoiceCount(t, b.ID); n != 1 {
		t.Fatalf("the healthy subscription has %d invoices, want 1", n)
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("columns after a failed write: %s", cols)
	}

	// A successor: a fresh engine under a new lease token.
	next := leadertest.Token(t, env.admin, postgres.WithLivemode(context.Background(), false), leader.RoleBilling)
	_, errs = env.newEngine(&subStoreAdapter{env.subs}, env.pricing).RunCycle(next, 50, nil)
	onlyFailure(t, errs, a.ID)
	if bf := env.get(t, a.ID).BillingFailure; bf == nil || bf.Attempts != 1 {
		t.Fatalf("the successor did not record: %+v", bf)
	}
}

// nastySafeErr is a curated error whose message is hostile to a TEXT column.
type nastySafeErr struct{}

func (nastySafeErr) Error() string { return "raw: must not be saved" }
func (nastySafeErr) OperatorSafeMessage() string {
	return "Curated \xff\x00 " + strings.Repeat("é", 2000)
}

// TestBillingFailure_SavedTextIsCuratedOrGeneric: the saved error is returned
// on every subscription read, so it is an allowlist. Raw error text never
// reaches it; a curated sentence is saved as written; and whatever is saved is
// valid UTF-8 with no NUL and at most 500 characters, so the write cannot fail
// on its own payload.
func TestBillingFailure_SavedTextIsCuratedOrGeneric(t *testing.T) {
	t.Run("raw database text becomes the generic reference", func(t *testing.T) {
		env := newBillFailureEnv(t, "Billing Failure Text Raw")
		a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)
		// The string run_cycle_for_tenant_test.go pins as never reaching a response.
		env.pricing.fail = func() error {
			return errors.New("pq: duplicate key value violates unique constraint idx_secret")
		}
		env.mustFail(t, a.ID)
		if got, want := env.get(t, a.ID).BillingFailure.LastError, "An internal error occurred. Reference: "+a.ID; got != want {
			t.Fatalf("last_error = %q, want %q", got, want)
		}
	})
	t.Run("a deterministic refusal is saved as its sentence", func(t *testing.T) {
		env := newBillFailureEnv(t, "Billing Failure Text Refusal")
		a := env.dueSub(t, "sub-a", env.planOK, env.periodEnd)
		// stripe_tax with no Stripe client wired: the resolver refuses.
		settings := tenant.NewSettingsStore(env.db)
		ts, err := settings.Get(env.ctx, env.tenantID)
		if err != nil {
			t.Fatalf("settings: %v", err)
		}
		ts.TenantID, ts.TaxProvider = env.tenantID, "stripe_tax"
		if _, err := settings.Upsert(env.ctx, ts); err != nil {
			t.Fatalf("settings upsert: %v", err)
		}
		env.mustFail(t, a.ID)
		want := "The tax provider in settings cannot be used. Choose a tax provider that is configured, then retry billing."
		if got := env.get(t, a.ID).BillingFailure.LastError; got != want {
			t.Fatalf("last_error = %q, want %q", got, want)
		}
	})
	t.Run("a curated message is stored valid and at most 500 characters", func(t *testing.T) {
		env := newBillFailureEnv(t, "Billing Failure Text Hostile")
		a := env.dueSub(t, "sub-a", env.planBad, env.periodEnd)
		env.pricing.fail = func() error { return nastySafeErr{} }
		env.mustFail(t, a.ID)
		got := env.get(t, a.ID).BillingFailure.LastError
		if !utf8.ValidString(got) || strings.ContainsRune(got, 0) || utf8.RuneCountInString(got) != 500 || !strings.HasPrefix(got, "Curated ") {
			t.Fatalf("last_error: valid=%v runes=%d prefix=%q", utf8.ValidString(got), utf8.RuneCountInString(got), got[:min(len(got), 16)])
		}
	})
}

// clockSub pins a due subscription to a ready test clock frozen at the fake
// clock's instant. The pin is written directly: these tests are about the
// billing run's clock path, not about how a subscription joins a clock.
func (env *billFailureEnv) clockSub(t *testing.T, code string, plan domain.Plan) (sub domain.Subscription, clockID string) {
	t.Helper()
	sub = env.dueSub(t, code, plan, env.periodEnd)
	clockID = postgres.NewID("vlx_tclk")
	admin := env.admin
	if _, err := admin.ExecContext(env.ctx, `INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status)
		VALUES ($1, $2, false, 'billing failure', $3, 'ready')`, clockID, env.tenantID, env.clock.Now(env.ctx)); err != nil {
		t.Fatalf("test clock: %v", err)
	}
	if _, err := admin.ExecContext(env.ctx, `UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, clockID, sub.ID); err != nil {
		t.Fatalf("pin to clock: %v", err)
	}
	return sub, clockID
}

// TestRunCycleForClock_SavesNoRecordAndIgnoresOne: the test-clock path is
// unchanged. A failing clock-pinned subscription saves nothing (the clock has
// its own failure state and Retry advance), a panic there saves nothing
// either, and a record that somehow sits on a clock-pinned row neither hides
// it from Advance nor stops it billing.
func TestRunCycleForClock_SavesNoRecordAndIgnoresOne(t *testing.T) {
	env := newBillFailureEnv(t, "Billing Failure Clock")
	a, clockID := env.clockSub(t, "sub-clock", env.planBad)
	// The store refuses a clock-pinned row on its own, so blank columns alone
	// cannot show that the engine did not try. Count the write attempts.
	// Mutation seam: drop `seen.TestClockID != ""` from recordBillFailure and
	// the count becomes 1.
	watched := &observedSubs{subStoreAdapter: &subStoreAdapter{env.subs}}
	env.engine = env.newEngine(watched, env.pricing)
	logs := captureLogs(t)

	_, errs := env.engine.RunCycleForClock(env.ctx, env.tenantID, clockID, 50)
	onlyFailure(t, errs, a.ID)
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("a failing clock-pinned subscription saved a record: %s", cols)
	}
	if n := watched.writes.Load(); n != 0 {
		t.Fatalf("the engine attempted %d failure writes for a clock-pinned subscription", n)
	}
	// The log line says why a counted failure was not saved.
	if !strings.Contains(logs.String(), "class=counted recorded=false not_recorded_reason=test_clock") {
		t.Fatalf("the failure line does not name the reason it was not saved:\n%s", logs.String())
	}

	env.pricing.fail = func() error { panic("boom on a clock") }
	recovered := func() (r any) {
		defer func() { r = recover() }()
		env.engine.RunCycleForClock(env.ctx, env.tenantID, clockID, 50)
		return nil
	}()
	if recovered == nil {
		t.Fatal("the panic did not reach the caller: the catch-up's own recover must see it")
	}
	if cols := env.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("a panic on a clock-pinned subscription saved a record: %s", cols)
	}
	if n := watched.writes.Load(); n != 0 {
		t.Fatalf("the engine attempted %d failure writes after a panic on a clock-pinned subscription", n)
	}

	// A parked record seeded by hand on the clock-pinned row.
	if _, err := env.admin.ExecContext(env.ctx, `
		UPDATE subscriptions SET billing_failed_for = next_billing_at, billing_failure_count = 8,
		       billing_failed_at = next_billing_at, billing_next_attempt_at = NULL, billing_last_error = 'seeded'
		 WHERE id = $1`, a.ID); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	env.pricing.fail = nil
	env.pricing.failing.Store(false)
	if gen, errs := env.engine.RunCycleForClock(env.ctx, env.tenantID, clockID, 50); gen != 1 || len(errs) != 0 {
		t.Fatalf("Advance over a seeded record: generated=%d errors=%v, want the period billed", gen, errs)
	}
	// The wall-clock scans never return a clock-pinned row.
	if gen, errs := env.engine.RunCycle(env.ctx, 50, nil); gen != 0 || len(errs) != 0 {
		t.Fatalf("wall-clock run touched a clock-pinned subscription: generated=%d errors=%v", gen, errs)
	}
}
