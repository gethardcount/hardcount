package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// This file is the saved failure state of subscription billing (ADR-119,
// issue #960). A subscription whose billing fails used to be re-attempted on
// every run and left only a log line. Now a failed attempt on a wall-clock
// subscription is saved on the subscription row, the next attempts wait on a
// fixed ladder, and after domain.MaxBillingAttempts the subscription is parked
// until an operator retries it.
//
// The record is keyed to the due instant it failed on (domain.DueInstant).
// Nothing here clears it after a clean bill: the period advance moves the due
// instant, and that voids the record in the same statement.

// Failure classes. Only billClassCounted is saved.
const (
	// billClassInterrupted: the run's own ctx is done (shutdown, a lost
	// lease, a request or catch-up timeout). Nothing is known about the
	// subscription.
	billClassInterrupted = "interrupted"
	// billClassTransient: the connection or the transaction was taken away
	// (postgres.IsTransient). The subscription's data cannot cause that.
	billClassTransient = "transient"
	// billClassCounted: everything else, including every timeout while the
	// run's ctx is alive. A timeout is the only way a subscription whose own
	// query is too slow ever shows up, so it counts.
	billClassCounted = "counted"
)

// classifyBillFailure classifies the error of a failed billing attempt. It
// reads the run's ctx and typed driver errors only; it never matches message
// text. It knows nothing about the subscription: recordBillFailure additionally
// never saves a failure for a clock-pinned subscription, whatever the class.
//
// The ctx is tested, not the error chain: a cancel can surface as a driver
// error with no context.Canceled in it, and an inner deadline (the tenant
// store's 5s) on a live run ctx is a timeout that must count.
func classifyBillFailure(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return billClassInterrupted
	}
	if postgres.IsTransient(err) {
		return billClassTransient
	}
	return billClassCounted
}

// billRetryWaits is the wait after each counted failure before the next
// attempt. It is billing's own ladder, not the tax-retry one: sharing it would
// move time-to-park whenever tax policy is tuned. No jitter: one leader drains
// sequentially, so there is no herd to spread. The earliest park is the sum,
// 3d 17h 15m after the first failure.
var billRetryWaits = [...]time.Duration{
	15 * time.Minute,
	time.Hour,
	4 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
	24 * time.Hour,
	24 * time.Hour,
}

// billRetryWait returns the wait after the n-th counted failure (n starts at
// 1). ok is false when there is no further attempt: the subscription parks.
// That is the case at domain.MaxBillingAttempts and for any n past the table,
// so a count above the bound can never un-park.
func billRetryWait(n int) (wait time.Duration, ok bool) {
	if n < 1 {
		n = 1
	}
	if n >= domain.MaxBillingAttempts || n > len(billRetryWaits) {
		return 0, false
	}
	return billRetryWaits[n-1], true
}

// billRefusal is a deterministic refusal on the bill path that has a sentence
// an operator can act on. The sentence is what gets saved as the last error;
// Error() and the chain are unchanged, so logs, tests and callers that match
// on the cause see what they saw before.
//
// It does not implement errs.SafeMessageError on purpose. The helpers that
// return it are shared with request paths (proration, create, cancel), and the
// HTTP responder turns any SafeMessageError into a 502 provider error.
type billRefusal struct {
	msg string
	err error
}

func (r *billRefusal) Error() string { return r.err.Error() }
func (r *billRefusal) Unwrap() error { return r.err }

// refuse tags err with the sentence to save if it fails a billing attempt.
func refuse(msg string, err error) error { return &billRefusal{msg: msg, err: err} }

const (
	refusalNoPeriod    = "This subscription has no billing period, so it cannot be billed."
	refusalNoItems     = "This subscription has no items to bill. Add an item, then retry billing."
	refusalNetTerms    = "The net payment terms in settings are negative. Set them to zero or more, then retry billing."
	refusalTimezone    = "The billing timezone in settings cannot be loaded. Set a valid timezone, then retry billing."
	refusalTaxProvider = "The tax provider in settings cannot be used. Choose a tax provider that is configured, then retry billing."
	refusalNoProgress  = "Billing didn't finish, and the subscription is still due. Retry billing, or wait for the next automatic try."
)

// errBillingGated is returned by billSubscription when the attempt-time gate
// declined the attempt: the fresh row carries a failure another run saved, and
// its wait has not passed (or it is parked). Nothing was attempted, so it is
// not a failure: attemptSub does not record or log it, drainDue leaves the sub
// out of the rest of the run without reporting it, and BillDueNow treats it as
// a clean return.
var errBillingGated = errors.New("billing attempt declined: a saved billing failure is waiting or parked")

// errBillPanic is the cause recorded when a billing attempt panics. The panic
// value and stack go to the log only.
var errBillPanic = errors.New("billing attempt panicked")

// maxBillingErrorRunes caps the saved error text.
const maxBillingErrorRunes = 500

// billFailureText builds the error text saved on the subscription. It is an
// allowlist: a curated sentence (a billRefusal, or an error type that opted
// into errs.SafeMessageError) is saved as written; everything else becomes a
// generic line that names the subscription id, and the full chain stays in the
// ERROR log under that id. This text is returned on every subscription read,
// so raw error text (constraint names, provider internals) must not reach it.
//
// The result is valid UTF-8 with no NUL byte and at most 500 runes, so the
// write can never fail on its own payload.
func billFailureText(subID string, cause error) string {
	var text string
	var refusal *billRefusal
	var safe errs.SafeMessageError
	switch {
	case errors.As(cause, &refusal):
		text = refusal.msg
	case errors.As(cause, &safe):
		text = safe.OperatorSafeMessage()
	default:
		text = "An internal error occurred. Reference: " + subID
	}
	text = strings.ReplaceAll(strings.ToValidUTF8(text, ""), "\x00", "")
	if utf8.RuneCountInString(text) > maxBillingErrorRunes {
		text = string([]rune(text)[:maxBillingErrorRunes])
	}
	return text
}

// billFailureOutcome is what recordBillFailure did, for the log line.
type billFailureOutcome struct {
	class    string
	recorded bool
	// notRecorded says why a counted failure was not saved: "test_clock",
	// "no_due_instant", "moved_on" (the write matched no row: another writer
	// finished the work or counted this attempt) or "write_error". Empty when
	// recorded, and for an uncounted class, which is its own reason.
	notRecorded string
	attempt     int
	next        *time.Time
}

// billProgress is what billSubscription publishes to attemptSub while it
// runs, so both survive an error return and a panic.
type billProgress struct {
	// seen is the row of the latest loop-top read: the row the step that
	// failed planned from. Until the first read it is the page's row.
	seen domain.Subscription
	// invoiced is how many invoices the call has generated so far.
	invoiced int
}

// recordBillFailure saves one failed billing attempt on the subscription the
// attempt planned from (seen), when the failure counts.
//
// Nothing is written for a clock-pinned subscription (a count and a real-time
// wait mean nothing against a frozen simulated time; the clock has its own
// internal_failure state and Retry advance), for an interrupted run, or for a
// transient database error. Those subscriptions stay due and are attempted on
// the next run, exactly as before.
//
// The failed billing transaction has already rolled back, so this is its own
// transaction on the run's ctx. If the write errors or matches no row, it is
// logged and nothing else happens: no retry, no detached ctx. The subscription
// keeps its previous record and stays due, so the next run attempts it and
// records then. The count ends one low, which can never over-park.
func (e *Engine) recordBillFailure(ctx context.Context, seen domain.Subscription, cause error) billFailureOutcome {
	out := billFailureOutcome{class: classifyBillFailure(ctx, cause)}
	if out.class != billClassCounted {
		return out
	}
	if seen.TestClockID != "" {
		out.notRecorded = "test_clock"
		return out
	}
	due := domain.DueInstant(seen)
	if due == nil {
		out.notRecorded = "no_due_instant"
		return out
	}
	observed := 0
	if seen.BillingFailure != nil {
		observed = seen.BillingFailure.Attempts
	}
	out.attempt = observed + 1
	// The app clock on both sides: the scans compare next_attempt_at with the
	// run's e.clock.Now(ctx), never with the database's now().
	now := e.clock.Now(ctx)
	if wait, ok := billRetryWait(out.attempt); ok {
		next := now.Add(wait)
		out.next = &next
	}
	recorded, err := e.subs.RecordBillingFailure(ctx, seen.TenantID, seen.ID, subscription.BillingFailureWrite{
		DueSeen:          *due,
		ObservedAttempts: observed,
		Attempts:         out.attempt,
		FailedAt:         now,
		NextAttemptAt:    out.next,
		LastError:        billFailureText(seen.ID, cause),
	})
	switch {
	case err != nil:
		out.notRecorded = "write_error"
		slog.Error("billing failure not recorded",
			"subscription_id", seen.ID, "tenant_id", seen.TenantID, "error", err, "billing_error", cause)
	case !recorded:
		// Another writer finished this work or already counted this attempt.
		out.notRecorded = "moved_on"
		slog.Debug("billing failure not recorded: the subscription moved on",
			"subscription_id", seen.ID, "tenant_id", seen.TenantID, "attempt", out.attempt)
	default:
		out.recorded = true
		if out.next == nil {
			slog.Error("subscription billing parked",
				"subscription_id", seen.ID, "tenant_id", seen.TenantID, "attempts", out.attempt)
		}
	}
	return out
}

// reportBillFailure records a failed attempt and writes its "bill subscription
// failed" ERROR line, the message the runbook greps for. Other lines may
// accompany it (a panic's stack, a failed failure-write, a park).
//
// The line carries class, and recorded. When a counted failure was not saved,
// not_recorded_reason says why, so class=counted recorded=false is never
// ambiguous: every failure on a test clock logs it with reason test_clock.
func (e *Engine) reportBillFailure(ctx context.Context, seen domain.Subscription, invoicesBefore int, cause error, logArgs ...any) {
	out := e.recordBillFailure(ctx, seen, cause)
	args := []any{
		"subscription_id", seen.ID, "tenant_id", seen.TenantID,
		"invoices_before_error", invoicesBefore, "error", cause,
		"class", out.class, "recorded", out.recorded,
	}
	if out.recorded {
		args = append(args, "attempt", out.attempt)
		if out.next != nil {
			args = append(args, "next_attempt_at", *out.next)
		}
	} else if out.notRecorded != "" {
		args = append(args, "not_recorded_reason", out.notRecorded)
	}
	slog.Error("bill subscription failed", append(args, logArgs...)...)
}

// attemptSub is the one caller of billSubscription, the bill of a
// subscription's due work: the scheduler run, the operator's billing run,
// test-clock Advance and the operator retry all come through it. (Invoices
// written by other paths, such as the day-1 bill on create, a cancel's final
// invoice or a threshold fire, do not, and save no failure.) It bills sub and,
// when the attempt fails, saves the failure against the row the failing step
// planned from.
//
// That row is kept in billProgress, which billSubscription keeps current. So
// a failure in the second of three overdue periods is recorded against the
// second period's due instant, and the row and the invoice count survive both
// an error return and a panic. Until the first read the row is the page's.
//
// A panic is recorded as a counted failure and then re-raised. The tick still
// dies and the scheduler still reports it; the next tick skips this
// subscription, so one bad subscription no longer replays the same panic at
// the head of every tick. Whether to record is a property of the subscription
// (its test clock), not of the caller: for a clock-pinned subscription
// nothing is written and the panic reaches the catch-up's own recover.
//
// grace is the invoice grace the caller's run honours (see billSubscription).
func (e *Engine) attemptSub(ctx context.Context, sub domain.Subscription, grace time.Duration, logArgs ...any) (n int, err error) {
	progress := billProgress{seen: sub}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("bill subscription panicked",
				append([]any{"subscription_id", sub.ID, "tenant_id", sub.TenantID, "panic", fmt.Sprint(r), "stack", string(debug.Stack())}, logArgs...)...)
			// n is not assigned when billSubscription panics; the count of
			// invoices already committed in this call is in progress.
			e.reportBillFailure(ctx, progress.seen, progress.invoiced, errBillPanic, logArgs...)
			panic(r)
		}
	}()
	n, err = e.billSubscription(ctx, sub, grace, &progress)
	if err != nil && !errors.Is(err, errBillingGated) {
		e.reportBillFailure(ctx, progress.seen, n, err, logArgs...)
	}
	return n, err
}

// BillDueNow bills one subscription now, for the operator's retry-billing
// action (subscription.Service.RetryBilling). It is the scheduler's own
// attempt: the same billSubscription, period CAS and failure write, so a
// retry that races the billing run still produces one invoice per period, and
// a retry that fails is saved like any other failed attempt.
//
// The caller has already cleared the failure record. If a billing run saved a
// new failure in between, the attempt-time gate stops this attempt.
//
// A clean attempt that leaves the subscription due is a failure here too. The
// billing run notices that case when the subscription comes back on a later
// page of the same run (drainDue); a retry has no later page, so it re-reads
// after its one attempt and saves errStillDue the same way. Without this a
// retry of a subscription that makes no progress would read as a success and
// the next run would only then find it failing again. A subscription that
// already carries a record is left alone: another run saved it, and counting
// here as well would count one attempt twice.
//
// Every error is logged before it is returned, so the caller may answer the
// operator without it: a failed attempt by attemptSub, the two reads here.
//
// No invoice grace (ADR-120): the operator is acting on this subscription
// now, so its ended period is closed now. Its callers (retry-billing, and the
// pre-close of an immediate cancel and of every other subscription write)
// compare with domain.BillingDueAt, which has no grace either.
func (e *Engine) BillDueNow(ctx context.Context, tenantID, subscriptionID string) (int, error) {
	logArgs := []any{"run", "operator_retry"}
	sub, err := e.subs.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		// No row was read, so there is nothing to record a failure against.
		slog.Error("bill subscription failed", append([]any{
			"subscription_id", subscriptionID, "tenant_id", tenantID, "error", err, "recorded", false,
		}, logArgs...)...)
		return 0, fmt.Errorf("read subscription %s: %w", subscriptionID, err)
	}
	n, err := e.attemptSub(ctx, sub, 0, logArgs...)
	if errors.Is(err, errBillingGated) {
		// A billing run saved a failure between the caller's clear and this
		// attempt. The response shows that record.
		return n, nil
	}
	if err != nil {
		return n, err
	}
	fresh, err := e.subs.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		slog.Error("bill subscription: re-read after a clean attempt failed", append([]any{
			"subscription_id", subscriptionID, "tenant_id", tenantID, "error", err,
		}, logArgs...)...)
		return n, fmt.Errorf("re-read subscription %s: %w", subscriptionID, err)
	}
	if fresh.TestClockID == "" && fresh.BillingFailure == nil && domain.BillingDueAt(fresh, e.clock.Now(ctx)) {
		e.reportBillFailure(ctx, fresh, n, errStillDue, logArgs...)
		return n, errStillDue
	}
	return n, nil
}
