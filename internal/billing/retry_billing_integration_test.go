package billing_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// The retry-billing tests live in this package, not in internal/subscription,
// because they need the real engine behind the route: the point of the route
// is that it bills through the scheduler's own attempt.

const retryKeyID = "vlx_key_retry_billing"

// retryRig mounts the real subscription handler and service over the
// billing-failure fixture, with the engine as the service's DueBiller.
type retryRig struct {
	*billFailureEnv
	audit  *audit.Logger
	svc    *subscription.Service
	router http.Handler
}

func newRetryRig(t *testing.T, name string, auditLogger subscription.AuditLogger) *retryRig {
	t.Helper()
	env := newBillFailureEnv(t, name)
	r := &retryRig{billFailureEnv: env, audit: audit.NewLogger(env.db)}
	if auditLogger == nil {
		auditLogger = r.audit
	}
	r.svc = subscription.NewService(env.subs, env.clock)
	r.svc.SetAuditLogger(auditLogger)
	r.svc.SetDueBiller(env.engine)
	r.router = subscription.NewHandler(r.svc).Routes()
	return r
}

type retryResponse struct {
	Subscription      domain.Subscription `json:"subscription"`
	InvoicesGenerated int                 `json:"invoices_generated"`
	StillDue          bool                `json:"still_due"`
}

// retry calls POST /{id}/retry-billing as an API-key operator of the tenant.
func (r *retryRig) retry(t *testing.T, subID string) (int, retryResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+subID+"/retry-billing", nil)
	req = req.WithContext(auth.WithKeyID(auth.WithTenantID(r.ctx, r.tenantID), retryKeyID))
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	var out retryResponse
	if rec.Code == http.StatusOK {
		// Errorf, not Fatalf: retry is also called from worker goroutines.
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Errorf("decode 200 body: %v\n%s", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

func (r *retryRig) auditRows(t *testing.T, subID string) []domain.AuditEntry {
	t.Helper()
	rows, _, err := r.audit.Query(r.ctx, r.tenantID, audit.QueryFilter{ResourceType: "subscription", ResourceID: subID})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	return rows
}

// park fails the subscription through the whole ladder.
func (r *retryRig) park(t *testing.T, subID string) {
	t.Helper()
	for n := 1; n <= domain.MaxBillingAttempts; n++ {
		r.mustFail(t, subID)
		if n < domain.MaxBillingAttempts {
			r.clock.Advance(billLadder[n-1])
		}
	}
	if bf := r.get(t, subID).BillingFailure; bf == nil || !bf.Parked {
		t.Fatalf("setup: %+v, want parked", bf)
	}
}

// TestRetryBilling_RepairedParkedSubscriptionBills: the cause is fixed, the
// operator retries a parked subscription, and it bills in that call. The
// retry leaves exactly one audit row, written with the clear.
func TestRetryBilling_RepairedParkedSubscriptionBills(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Repaired", nil)
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	r.park(t, a.ID)
	r.pricing.failing.Store(false)

	code, out, body := r.retry(t, a.ID)
	if code != http.StatusOK {
		t.Fatalf("retry: %d %s", code, body)
	}
	if out.InvoicesGenerated != 1 || out.Subscription.BillingFailure != nil || out.Subscription.ID != a.ID || out.StillDue {
		t.Fatalf("response: invoices_generated=%d billing_failure=%+v still_due=%v", out.InvoicesGenerated, out.Subscription.BillingFailure, out.StillDue)
	}
	if strings.Contains(body, `"billing_failure"`) {
		t.Fatalf("a billed subscription still carries billing_failure on the wire: %s", body)
	}
	if n := r.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices, want exactly 1", n)
	}

	rows := r.auditRows(t, a.ID)
	if len(rows) != 1 {
		t.Fatalf("%d audit rows for the subscription, want exactly the retry's 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Action != domain.AuditActionUpdate || row.ResourceLabel != a.Code || row.ActorType != "api_key" || row.ActorID != retryKeyID {
		t.Fatalf("audit row: action=%q label=%q actor=%s/%s", row.Action, row.ResourceLabel, row.ActorType, row.ActorID)
	}
	m := row.Metadata
	if m["action"] != "billing_retried" || m["attempts_before"] != float64(8) || m["was_parked"] != true ||
		m["customer_id"] != a.CustomerID || m["last_error_before"] != "An internal error occurred. Reference: "+a.ID || m["failed_at_before"] == nil {
		t.Fatalf("audit metadata: %+v", m)
	}

	// A second click has nothing to retry and writes no second row.
	code, _, body = r.retry(t, a.ID)
	if code != http.StatusConflict || !strings.Contains(body, "billing_not_failing") {
		t.Fatalf("second retry: %d %s, want 409 billing_not_failing", code, body)
	}
	if rows := r.auditRows(t, a.ID); len(rows) != 1 {
		t.Fatalf("%d audit rows after a no-op retry, want 1", len(rows))
	}
}

// TestRetryBilling_StillFailingRestartsTheLadder: a retry that fails again is
// not an HTTP error. The count restarts at 1 with the first wait, so repeated
// clicks can never park a subscription, and the retry is still audited.
func TestRetryBilling_StillFailingRestartsTheLadder(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Still Failing", nil)
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	r.park(t, a.ID)
	now := r.clock.Now(r.ctx)

	for click := 1; click <= 3; click++ {
		code, out, body := r.retry(t, a.ID)
		if code != http.StatusOK {
			t.Fatalf("click %d: %d %s", click, code, body)
		}
		bf := out.Subscription.BillingFailure
		if out.InvoicesGenerated != 0 || bf == nil || bf.Attempts != 1 || bf.Parked ||
			bf.NextAttemptAt == nil || !bf.NextAttemptAt.Equal(now.Add(15*time.Minute)) {
			t.Fatalf("click %d: invoices_generated=%d billing_failure=%+v, want attempts 1 and next attempt in 15 minutes", click, out.InvoicesGenerated, bf)
		}
		if rows := r.auditRows(t, a.ID); len(rows) != click {
			t.Fatalf("click %d: %d audit rows, want %d", click, len(rows), click)
		}
	}
	if n := r.invoiceCount(t, a.ID); n != 0 {
		t.Fatalf("%d invoices from failing retries", n)
	}
}

// TestRetryBilling_Refusals: nothing to retry is a 409 that writes nothing.
func TestRetryBilling_Refusals(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Refusals", nil)

	t.Run("not failing", func(t *testing.T) {
		a := r.dueSub(t, "sub-healthy", r.planOK, r.periodEnd)
		code, _, body := r.retry(t, a.ID)
		if code != http.StatusConflict || !strings.Contains(body, "billing_not_failing") {
			t.Fatalf("got %d %s, want 409 billing_not_failing", code, body)
		}
		if rows := r.auditRows(t, a.ID); len(rows) != 0 {
			t.Fatalf("a refused retry wrote %d audit rows", len(rows))
		}
		if n := r.invoiceCount(t, a.ID); n != 0 {
			t.Fatalf("a refused retry billed the subscription (%d invoices): the route is not a second billing trigger", n)
		}
	})
	t.Run("on a test clock", func(t *testing.T) {
		a, _ := r.clockSub(t, "sub-clock", r.planOK)
		code, _, body := r.retry(t, a.ID)
		if code != http.StatusConflict || !strings.Contains(body, "test clock") {
			t.Fatalf("got %d %s, want 409 pointing at the clock's retry advance", code, body)
		}
		// The documented code for this case is invalid_state. It must not be
		// billing_not_failing, which a client reads as "already healthy".
		if !strings.Contains(body, `"invalid_state"`) || strings.Contains(body, "billing_not_failing") {
			t.Fatalf("409 body %s, want code invalid_state", body)
		}
		if rows := r.auditRows(t, a.ID); len(rows) != 0 {
			t.Fatalf("a refused retry wrote %d audit rows", len(rows))
		}
	})
	t.Run("unknown subscription", func(t *testing.T) {
		if code, _, body := r.retry(t, "vlx_sub_missing"); code != http.StatusNotFound {
			t.Fatalf("got %d %s, want 404", code, body)
		}
	})
}

// failingRetryAudit fails the in-transaction audit emission.
type failingRetryAudit struct{}

func (failingRetryAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}
func (failingRetryAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("injected audit failure")
}

// TestRetryBilling_AuditFailureLeavesThePark: the clear and its audit row share
// a transaction. If the row cannot be written, the park is not cleared and
// nothing is billed: there is never an un-park without its evidence.
func TestRetryBilling_AuditFailureLeavesThePark(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Audit Fails", failingRetryAudit{})
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	r.park(t, a.ID)
	r.pricing.failing.Store(false)
	before := r.billingColumns(t, a.ID)

	code, _, body := r.retry(t, a.ID)
	if code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", code, body)
	}
	if got := r.billingColumns(t, a.ID); got != before {
		t.Fatalf("the park was cleared without its audit row: %s → %s", before, got)
	}
	if n := r.invoiceCount(t, a.ID); n != 0 {
		t.Fatalf("%d invoices: the retry billed although its clear rolled back", n)
	}
}

// TestRetryBilling_RacingTheScheduler_ExactlyOneInvoice: the operator retries
// while billing runs are attempting the same subscription. The retry bills
// through the scheduler's own attempt, so the period CAS lets one closer
// commit: exactly one invoice, whoever wins.
func TestRetryBilling_RacingTheScheduler_ExactlyOneInvoice(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Race", nil)
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	// One failure, then the wait passes and the cause is fixed: the scans
	// admit the subscription again and it still carries a record to clear.
	r.mustFail(t, a.ID)
	r.clock.Advance(billLadder[0])
	r.pricing.failing.Store(false)

	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, 2)
	for i := range codes {
		wg.Go(func() {
			<-start
			codes[i], _, _ = r.retry(t, a.ID)
		})
	}
	for range 2 {
		wg.Go(func() {
			<-start
			if _, errs := r.engine.RunCycle(r.ctx, 50, nil); len(errs) != 0 {
				t.Errorf("scheduler run: %v", errs)
			}
		})
		wg.Go(func() {
			<-start
			if _, failures := r.engine.RunCycleForTenant(r.ctx, r.tenantID, 50); len(failures) != 0 {
				t.Errorf("manual run: %v", failures)
			}
		})
	}
	close(start)
	wg.Wait()

	if n := r.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices after the race, want exactly 1 (codes %v)", n, codes)
	}
	cleared := 0
	for _, c := range codes {
		// 200: this retry cleared the record. 409: a run billed first (the
		// record was void) or the other retry cleared it.
		if c != http.StatusOK && c != http.StatusConflict {
			t.Fatalf("retry answered %d, want 200 or 409", c)
		}
		if c == http.StatusOK {
			cleared++
		}
	}
	// One audit row per clear, and never one without a clear.
	if rows := r.auditRows(t, a.ID); len(rows) != cleared {
		t.Fatalf("%d audit rows for %d retries that answered 200", len(rows), cleared)
	}
	if bf := r.get(t, a.ID).BillingFailure; bf != nil {
		t.Fatalf("billed subscription still reads as failing: %+v", bf)
	}
}

// TestRetryBilling_NoProgressIsSavedInTheSameCall: a retry whose attempt
// returns cleanly but leaves the subscription due has not billed it. The
// billing run only notices that when the subscription comes back on a later
// page; the retry has no later page, so it checks after its one attempt. The
// response must show the failure, not read as a success.
func TestRetryBilling_NoProgressIsSavedInTheSameCall(t *testing.T) {
	r := newRetryRig(t, "Retry Billing No Progress", nil)
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	r.park(t, a.ID)
	// The plan read is fixed, but every close now loses the period CAS, which
	// billSubscription absorbs and returns cleanly from.
	r.pricing.failing.Store(false)
	stuck := &failableSubAdapter{subStoreAdapter: &subStoreAdapter{r.subs}, closePeriodTxErr: subscription.ErrWatermarkMoved}
	r.svc.SetDueBiller(r.newEngine(stuck, r.pricing))
	now := r.clock.Now(r.ctx)

	code, out, body := r.retry(t, a.ID)
	if code != http.StatusOK {
		t.Fatalf("retry: %d %s", code, body)
	}
	bf := out.Subscription.BillingFailure
	if bf == nil || bf.Attempts != 1 || !strings.HasPrefix(bf.LastError, "Billing made no progress") ||
		bf.NextAttemptAt == nil || !bf.NextAttemptAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("billing_failure = %+v, want attempts 1 with the no-progress text and the first wait", bf)
	}
	if out.InvoicesGenerated != 0 || out.StillDue {
		t.Fatalf("invoices_generated=%d still_due=%v, want 0 and false (the failure is saved, so the record carries the outcome)", out.InvoicesGenerated, out.StillDue)
	}
	if rows := r.auditRows(t, a.ID); len(rows) != 1 {
		t.Fatalf("%d audit rows, want the retry's 1", len(rows))
	}
}

// TestRetryBilling_UncountedFailureAnswersStillDue: the retry's attempt fails
// with an error that is never saved (here a deadlock abort). The clear and its
// audit row are committed, so the answer is still 200, but it must not read as
// billed: there is no billing_failure, and still_due says the subscription is
// unbilled and waiting for the next billing run.
//
// Mutation seam: stop computing still_due in Service.RetryBilling → this fails.
func TestRetryBilling_UncountedFailureAnswersStillDue(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Uncounted", nil)
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	r.park(t, a.ID)
	r.pricing.fail = func() error {
		return fmt.Errorf("get plan: %w", &pgconn.PgError{Code: "40P01", Message: "deadlock detected"})
	}

	code, out, body := r.retry(t, a.ID)
	if code != http.StatusOK {
		t.Fatalf("retry: %d %s", code, body)
	}
	if out.Subscription.BillingFailure != nil || out.InvoicesGenerated != 0 || !out.StillDue {
		t.Fatalf("billing_failure=%+v invoices_generated=%d still_due=%v, want none, 0 and true", out.Subscription.BillingFailure, out.InvoicesGenerated, out.StillDue)
	}
	if !strings.Contains(body, `"still_due":true`) {
		t.Fatalf("still_due is not on the wire: %s", body)
	}
	if rows := r.auditRows(t, a.ID); len(rows) != 1 {
		t.Fatalf("%d audit rows, want the retry's 1", len(rows))
	}
	// A billed retry says still_due false.
	r.pricing.fail = nil
	r.pricing.failing.Store(false)
	if _, errs := r.engine.RunCycle(r.ctx, 50, nil); len(errs) != 0 {
		t.Fatalf("the next billing run: %v", errs)
	}
	if n := r.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices after the next run, want 1: an uncounted failure must leave the subscription due with no wait", n)
	}
}

// The race test above releases everything at once and accepts any order, so
// it can pass with no collision at all. The two tests below place the racing
// run at an exact point of the retry and assert both sides' outcome.

// TestRetryBilling_RunBillsDuringTheRetrysAttempt: the retry has cleared the
// record and is inside its attempt when a billing run bills the period. The
// retry's attempt then fails. Its failure must not be saved (the work is
// done), and the response must not read as still due.
func TestRetryBilling_RunBillsDuringTheRetrysAttempt(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Run Wins", nil)
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	r.park(t, a.ID)

	winner := r.newEngine(&subStoreAdapter{r.subs}, &pricingStoreAdapter{pricing.NewPostgresStore(r.db)})
	var once sync.Once
	r.pricing.onBad = func() {
		once.Do(func() {
			if gen, failures := winner.RunCycleForTenant(r.ctx, r.tenantID, 50); gen != 1 || len(failures) != 0 {
				t.Errorf("the run inside the retry's attempt: generated=%d failures=%v, want 1 and none", gen, failures)
			}
		})
	}
	code, out, body := r.retry(t, a.ID)
	if code != http.StatusOK {
		t.Fatalf("retry: %d %s", code, body)
	}
	if out.InvoicesGenerated != 0 || out.Subscription.BillingFailure != nil || out.StillDue {
		t.Fatalf("invoices_generated=%d billing_failure=%+v still_due=%v, want 0, none and false: the run billed the period", out.InvoicesGenerated, out.Subscription.BillingFailure, out.StillDue)
	}
	if n := r.invoiceCount(t, a.ID); n != 1 {
		t.Fatalf("%d invoices, want exactly 1", n)
	}
	if cols := r.billingColumns(t, a.ID); cols != blankBillingColumns {
		t.Fatalf("the retry's late failure landed on a billed period: %s", cols)
	}
	if rows := r.auditRows(t, a.ID); len(rows) != 1 {
		t.Fatalf("%d audit rows, want the retry's 1", len(rows))
	}
}

// interleavedBiller runs before() and then the real BillDueNow: the window
// between the retry's clear and its attempt.
type interleavedBiller struct {
	before func()
	next   subscription.DueBiller
}

func (b interleavedBiller) BillDueNow(ctx context.Context, tenantID, id string) (int, error) {
	b.before()
	return b.next.BillDueNow(ctx, tenantID, id)
}

// TestRetryBilling_RunFailsBetweenTheClearAndTheAttempt: after the retry's
// clear commits, a billing run attempts the subscription, fails and saves
// attempt 1 with its wait. The retry's own attempt must then not be made: the
// attempt-time gate stops it. The response shows the run's record, and one
// failed attempt is counted once.
//
// Mutation seam: remove the gate at the top of billSubscription's loop → the
// retry attempts as well and attempts becomes 2.
func TestRetryBilling_RunFailsBetweenTheClearAndTheAttempt(t *testing.T) {
	r := newRetryRig(t, "Retry Billing Run Fails First", nil)
	a := r.dueSub(t, "sub-a", r.planBad, r.periodEnd)
	r.park(t, a.ID)
	now := r.clock.Now(r.ctx)

	other := r.newEngine(&subStoreAdapter{r.subs}, r.pricing)
	r.svc.SetDueBiller(interleavedBiller{next: r.engine, before: func() {
		if _, failures := other.RunCycleForTenant(r.ctx, r.tenantID, 50); len(failures) != 1 {
			t.Errorf("the run between the clear and the attempt: failures=%v, want the one failure", failures)
		}
	}})
	calls := r.pricing.calls.Load()

	code, out, body := r.retry(t, a.ID)
	if code != http.StatusOK {
		t.Fatalf("retry: %d %s", code, body)
	}
	bf := out.Subscription.BillingFailure
	if bf == nil || bf.Attempts != 1 || bf.NextAttemptAt == nil || !bf.NextAttemptAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("billing_failure = %+v, want the run's record: attempts 1, next attempt in 15 minutes", bf)
	}
	if out.InvoicesGenerated != 0 || out.StillDue {
		t.Fatalf("invoices_generated=%d still_due=%v, want 0 and false", out.InvoicesGenerated, out.StillDue)
	}
	if got := r.pricing.calls.Load(); got != calls+1 {
		t.Fatalf("plan reads went %d → %d, want exactly 1: the retry's own attempt must not be made", calls, got)
	}
	if rows := r.auditRows(t, a.ID); len(rows) != 1 {
		t.Fatalf("%d audit rows, want the retry's 1", len(rows))
	}
	if n := r.invoiceCount(t, a.ID); n != 0 {
		t.Fatalf("%d invoices from two failed attempts", n)
	}
}
