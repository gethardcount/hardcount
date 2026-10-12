package billing_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
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
	router http.Handler
}

func newRetryRig(t *testing.T, name string, auditLogger subscription.AuditLogger) *retryRig {
	t.Helper()
	env := newBillFailureEnv(t, name)
	r := &retryRig{billFailureEnv: env, audit: audit.NewLogger(env.db)}
	if auditLogger == nil {
		auditLogger = r.audit
	}
	svc := subscription.NewService(env.subs, env.clock)
	svc.SetAuditLogger(auditLogger)
	svc.SetDueBiller(env.engine)
	r.router = subscription.NewHandler(svc).Routes()
	return r
}

type retryResponse struct {
	Subscription      domain.Subscription `json:"subscription"`
	InvoicesGenerated int                 `json:"invoices_generated"`
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
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode 200 body: %v\n%s", err, rec.Body.String())
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
	if out.InvoicesGenerated != 1 || out.Subscription.BillingFailure != nil || out.Subscription.ID != a.ID {
		t.Fatalf("response: invoices_generated=%d billing_failure=%+v", out.InvoicesGenerated, out.Subscription.BillingFailure)
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
	for _, c := range codes {
		// 200: this retry cleared the record. 409: a run billed first (the
		// record was void) or the other retry cleared it.
		if c != http.StatusOK && c != http.StatusConflict {
			t.Fatalf("retry answered %d, want 200 or 409", c)
		}
	}
	if bf := r.get(t, a.ID).BillingFailure; bf != nil {
		t.Fatalf("billed subscription still reads as failing: %+v", bf)
	}
}
