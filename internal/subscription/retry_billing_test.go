package subscription

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// The real-Postgres, real-engine coverage of retry-billing lives in
// internal/billing/retry_billing_integration_test.go. These unit tests pin
// the service's order of operations on the in-memory store.

type retryAuditSpy struct {
	entries []audit.Entry
	err     error
}

func (a *retryAuditSpy) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}
func (a *retryAuditSpy) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, e)
	return nil
}

type dueBillerSpy struct {
	calls int
	// sawRecord is whether the subscription still carried its failure record
	// when the bill ran: it must not, the clear comes first.
	sawRecord bool
	store     *memStore
	// err, if set, is what the attempt returns (with zero invoices).
	err error
}

func (b *dueBillerSpy) BillDueNow(_ context.Context, _, id string) (int, error) {
	b.calls++
	b.sawRecord = b.store.subs[id].BillingFailure != nil
	if b.err != nil {
		return 0, b.err
	}
	return 2, nil
}

func seedFailingSub(t *testing.T, store *memStore, tenantID string) string {
	t.Helper()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	sub := store.subs[subID]
	sub.BillingFailure = &domain.BillingFailure{
		Attempts: 8, MaxAttempts: domain.MaxBillingAttempts, Parked: true,
		LastError: "An internal error occurred. Reference: " + subID, FailedAt: time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC),
	}
	store.subs[subID] = sub
	return subID
}

func TestRetryBilling_ClearsWithAuditThenBills(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	subID := seedFailingSub(t, store, tenantID)
	spy, biller := &retryAuditSpy{}, &dueBillerSpy{store: store}
	svc := NewService(store, nil)
	svc.SetAuditLogger(spy)
	svc.SetDueBiller(biller)

	res, err := svc.RetryBilling(context.Background(), tenantID, subID)
	if err != nil {
		t.Fatalf("RetryBilling: %v", err)
	}
	if res.InvoicesGenerated != 2 || res.Subscription.BillingFailure != nil {
		t.Fatalf("generated=%d failure=%+v", res.InvoicesGenerated, res.Subscription.BillingFailure)
	}
	if biller.calls != 1 || biller.sawRecord {
		t.Fatalf("biller calls=%d sawRecord=%v: the record must be cleared before the one bill", biller.calls, biller.sawRecord)
	}
	if len(spy.entries) != 1 {
		t.Fatalf("%d audit entries, want 1", len(spy.entries))
	}
	e := spy.entries[0]
	if e.Action != domain.AuditActionUpdate || e.ResourceType != "subscription" || e.ResourceID != subID ||
		e.Metadata["action"] != "billing_retried" || e.Metadata["attempts_before"] != 8 || e.Metadata["was_parked"] != true {
		t.Fatalf("audit entry: %+v", e)
	}
}

// TestRetryBilling_AttemptErrorIsNotTheOperatorsError pins the contract after
// the clear has committed: an attempt that failed without saving a failure is
// answered as a result, never as an error (a 5xx would invite a second call,
// which gets 409 for a subscription that is still unbilled). StillDue carries
// the truth: true while the subscription is due with no record, false once it
// is not due.
func TestRetryBilling_AttemptErrorIsNotTheOperatorsError(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	subID := seedFailingSub(t, store, tenantID)
	past := time.Now().Add(-time.Hour)
	sub := store.subs[subID]
	sub.Status, sub.NextBillingAt = domain.SubscriptionActive, &past
	store.subs[subID] = sub
	spy, biller := &retryAuditSpy{}, &dueBillerSpy{store: store, err: errors.New("deadlock detected")}
	svc := NewService(store, nil)
	svc.SetAuditLogger(spy)
	svc.SetDueBiller(biller)

	res, err := svc.RetryBilling(context.Background(), tenantID, subID)
	if err != nil {
		t.Fatalf("RetryBilling returned the attempt's error: %v", err)
	}
	if res.InvoicesGenerated != 0 || res.Subscription.BillingFailure != nil || !res.StillDue {
		t.Fatalf("generated=%d failure=%+v still_due=%v, want 0, none and true", res.InvoicesGenerated, res.Subscription.BillingFailure, res.StillDue)
	}
	if len(spy.entries) != 1 || biller.calls != 1 {
		t.Fatalf("audit entries=%d bills=%d, want 1 and 1", len(spy.entries), biller.calls)
	}

	// Not due any more (another run billed it): the same attempt error is
	// not "still due".
	store.subs[subID] = func() domain.Subscription {
		s := store.subs[subID]
		future := time.Now().Add(24 * time.Hour)
		s.NextBillingAt = &future
		s.BillingFailure = &domain.BillingFailure{Attempts: 1, MaxAttempts: domain.MaxBillingAttempts}
		return s
	}()
	res, err = svc.RetryBilling(context.Background(), tenantID, subID)
	if err != nil || res.StillDue {
		t.Fatalf("err=%v still_due=%v, want nil and false for a subscription that is no longer due", err, res.StillDue)
	}
}

func TestRetryBilling_NotFailingWritesNothing(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	subID, _ := seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	spy, biller := &retryAuditSpy{}, &dueBillerSpy{store: store}
	svc := NewService(store, nil)
	svc.SetAuditLogger(spy)
	svc.SetDueBiller(biller)

	_, err := svc.RetryBilling(context.Background(), tenantID, subID)
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "billing_not_failing" {
		t.Fatalf("err = %v (code %q), want InvalidState billing_not_failing", err, errs.Code(err))
	}
	if len(spy.entries) != 0 || biller.calls != 0 {
		t.Fatalf("a refused retry wrote %d audit entries and billed %d times", len(spy.entries), biller.calls)
	}
}

func TestRetryBilling_AuditFailureKeepsTheRecordAndDoesNotBill(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	subID := seedFailingSub(t, store, tenantID)
	biller := &dueBillerSpy{store: store}
	svc := NewService(store, nil)
	svc.SetAuditLogger(&retryAuditSpy{err: errors.New("audit down")})
	svc.SetDueBiller(biller)

	if _, err := svc.RetryBilling(context.Background(), tenantID, subID); err == nil {
		t.Fatal("RetryBilling succeeded although its audit row failed")
	}
	if store.subs[subID].BillingFailure == nil || biller.calls != 0 {
		t.Fatalf("after a failed audit: record=%+v bills=%d, want the record kept and no bill", store.subs[subID].BillingFailure, biller.calls)
	}
}

func TestRetryBilling_ClockPinnedIsRefused(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	subID := seedFailingSub(t, store, tenantID)
	sub := store.subs[subID]
	sub.TestClockID = "vlx_tclk_1"
	store.subs[subID] = sub
	svc := NewService(store, nil)

	_, err := svc.RetryBilling(context.Background(), tenantID, subID)
	if !errors.Is(err, errs.ErrInvalidState) || errs.Code(err) != "" {
		t.Fatalf("err = %v (code %q), want InvalidState with the default code: billing_not_failing means something else", err, errs.Code(err))
	}
	if store.subs[subID].BillingFailure == nil {
		t.Fatal("a refused retry cleared the record")
	}
}

// TestListHandler_BillingParam: ?billing= is a closed set. An unknown value is
// a 400, never an unfiltered list.
func TestListHandler_BillingParam(t *testing.T) {
	const tenantID = "t1"
	store := newMemStore()
	seedSubWithItem(t, store, tenantID, "cus_1", "plan_old")
	h := NewHandler(NewService(store, nil))

	for query, want := range map[string]int{
		"":                  http.StatusOK,
		"?billing=failing":  http.StatusOK,
		"?billing=parked":   http.StatusBadRequest,
		"?billing=FAILING":  http.StatusBadRequest,
		"?billing=failing+": http.StatusBadRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/subscriptions"+query, nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.TestTenantIDKey(), tenantID))
		rr := httptest.NewRecorder()
		h.list(rr, req)
		if rr.Code != want {
			t.Errorf("GET /subscriptions%s: got %d, want %d (%s)", query, rr.Code, want, rr.Body.String())
		}
	}
}
