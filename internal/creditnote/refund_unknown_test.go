package creditnote

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// These tests pin #965: a refund whose Stripe result is unclear is recorded
// as "unknown", never "failed", and Retry refund settles it without a second
// refund. The charge path made the same split first (ADR-107).

// refundUnknown is the refund status for "Stripe may or may not have made the
// refund".
const refundUnknown = domain.RefundUnknown

// outcomeErr is a refund error that says whether Stripe may have made the
// refund, the way the Stripe refunder's errors do.
type outcomeErr struct {
	msg     string
	unknown bool
	notRun  bool
}

func (e *outcomeErr) Error() string        { return e.msg }
func (e *outcomeErr) OutcomeUnknown() bool { return e.unknown }
func (e *outcomeErr) RequestNotRun() bool  { return e.notRun }

var (
	errRefundNoAnswer = &outcomeErr{msg: "stripe refund: An unknown error occurred", unknown: true}
	errRefundRefused  = &outcomeErr{msg: "stripe refund: Charge ch_1 has already been refunded.", unknown: false}
	// Stripe refused before running the request (an invalid API key), so it
	// saved no result for the key.
	errRefundNotRun = &outcomeErr{msg: "stripe refund: not run: Invalid API Key provided: sk_test_****FAKE", notRun: true}
)

func TestIssue_RefundResultUnknownOrFailed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want domain.RefundStatus
	}{
		{"no clear answer (timeout, Stripe 5xx): the refund may exist", errRefundNoAnswer, refundUnknown},
		// An error that does not say counts as unclear. A refusal has to be
		// proven, as classifyStripeError does for charges.
		{"an error that does not say which", errors.New("dial tcp: i/o timeout"), refundUnknown},
		{"Stripe refused: nothing was refunded", errRefundRefused, domain.RefundFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _, _, refunder := setupRefundSvc(t)
			refunder.failWith = tc.err

			cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "fraudulent"})
			if err != nil {
				t.Fatalf("CreateRefund must issue the note and record the refund leg, not fail: %v", err)
			}
			if cn.Status != domain.CreditNoteIssued {
				t.Fatalf("status = %q, want issued", cn.Status)
			}
			if cn.RefundStatus != tc.want {
				t.Fatalf("refund_status = %q, want %q", cn.RefundStatus, tc.want)
			}
			if cn.StripeRefundID != "" {
				t.Fatalf("stripe_refund_id = %q, want empty: Stripe returned no refund", cn.StripeRefundID)
			}
		})
	}
}

// issueUnknownRefund issues a full refund whose Stripe call gets no clear
// answer, and returns the note.
func issueUnknownRefund(t *testing.T, svc *Service, refunder *fakeRefunder) domain.CreditNote {
	t.Helper()
	refunder.failWith = errRefundNoAnswer
	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "fraudulent"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if cn.RefundStatus != refundUnknown {
		t.Fatalf("refund_status after an unclear answer = %q, want unknown", cn.RefundStatus)
	}
	refunder.failWith = nil
	return cn
}

// Stripe made the refund though Velox never heard back: Retry adopts it and
// sends nothing.
func TestRetryRefund_UnknownAdoptsTheRefundStripeMade(t *testing.T) {
	t.Parallel()
	svc, _, _, refunder := setupRefundSvc(t)
	cn := issueUnknownRefund(t, svc, refunder)
	refunder.foundID = "re_made_anyway"
	refunder.foundStatus = domain.RefundSucceeded

	out, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("RetryRefund: %v", err)
	}
	if out.RefundStatus != domain.RefundSucceeded || out.StripeRefundID != "re_made_anyway" {
		t.Fatalf("refund leg = (%q, %q), want (succeeded, re_made_anyway)", out.RefundStatus, out.StripeRefundID)
	}
	if len(refunder.calls) != 1 {
		t.Fatalf("refund requests = %d, want 1 (the original): Stripe already made it", len(refunder.calls))
	}
}

// Stripe has no refund for the note: Retry resends the unclear request with
// its own idempotency key. If that request is still running at Stripe, Stripe
// answers 409 instead of running it twice; if it ran, Stripe returns its
// result. A new key could start a second refund
// (https://docs.stripe.com/error-low-level#content-errors).
func TestRetryRefund_UnknownResendsTheSameRequest(t *testing.T) {
	t.Parallel()
	svc, _, _, refunder := setupRefundSvc(t)
	cn := issueUnknownRefund(t, svc, refunder)

	out, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("RetryRefund: %v", err)
	}
	if len(refunder.findCalls) != 1 {
		t.Fatalf("Stripe lookups before the resend = %d, want 1", len(refunder.findCalls))
	}
	if len(refunder.calls) != 2 {
		t.Fatalf("refund requests = %d, want 2 (the unclear one, then the resend)", len(refunder.calls))
	}
	first, resend := refunder.calls[0].idempotencyKey, refunder.calls[1].idempotencyKey
	if resend != first {
		t.Fatalf("resend key = %q, want the unclear request's key %q", resend, first)
	}
	if out.RefundStatus != domain.RefundSucceeded || out.StripeRefundID == "" {
		t.Fatalf("refund leg = (%q, %q), want succeeded with a refund id", out.RefundStatus, out.StripeRefundID)
	}
}

// A resend that again gets no clear answer stays unknown, keeps the same key,
// and tells the caller the result is unknown. A refusal after that is failed.
func TestRetryRefund_UnknownStaysUnknownUntilStripeAnswers(t *testing.T) {
	t.Parallel()
	svc, store, _, refunder := setupRefundSvc(t)
	cn := issueUnknownRefund(t, svc, refunder)

	refunder.failWith = errRefundNoAnswer
	_, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	var oe interface{ OutcomeUnknown() bool }
	if !errors.As(err, &oe) || !oe.OutcomeUnknown() {
		t.Fatalf("RetryRefund error = %v, want one that says the result is unknown", err)
	}
	if got := store.notes[cn.ID].RefundStatus; got != refundUnknown {
		t.Fatalf("refund_status = %q, want unknown", got)
	}

	refunder.failWith = errRefundRefused
	if _, err := svc.RetryRefund(context.Background(), "t1", cn.ID); err == nil {
		t.Fatal("RetryRefund returned no error for a refusal")
	}
	if got := store.notes[cn.ID].RefundStatus; got != domain.RefundFailed {
		t.Fatalf("refund_status after a refusal = %q, want failed", got)
	}
	keys := map[string]bool{}
	for _, c := range refunder.calls {
		keys[c.idempotencyKey] = true
	}
	if len(refunder.calls) != 3 || len(keys) != 1 {
		t.Fatalf("requests = %d with %d keys, want 3 requests on 1 key: each was a resend of the unclear request", len(refunder.calls), len(keys))
	}
}

// After Stripe reported refund re_dead failed, a new attempt got no clear
// answer. The note keeps re_dead's id with status unknown. Retry must not ask
// Stripe about re_dead: its answer, "failed", says nothing about the new
// attempt, and writing it would put back the false "failed".
func TestRetryRefund_UnknownAfterADeadRefund(t *testing.T) {
	t.Parallel()
	svc, store, _, refunder := setupRefundSvc(t)
	refunder.getStatusOf = map[string]domain.RefundStatus{"re_dead": domain.RefundFailed}
	readsOfDead := func() int {
		n := 0
		for _, id := range refunder.getCalls {
			if id == "re_dead" {
				n++
			}
		}
		return n
	}
	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		TenantID: "t1", InvoiceID: "inv_paid", CustomerID: "cus_1",
		CreditNoteNumber: "CN-DEAD-THEN-UNKNOWN", Status: domain.CreditNoteIssued,
		Reason: "retry", SubtotalCents: 5000, TotalCents: 5000,
		RefundAmountCents: 5000, Currency: "USD",
		RefundStatus: domain.RefundFailed, StripeRefundID: "re_dead",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	refunder.failWith = errRefundNoAnswer
	if _, err := svc.RetryRefund(context.Background(), "t1", cn.ID); err == nil {
		t.Fatal("RetryRefund returned no error for an unclear answer")
	}
	got := store.notes[cn.ID]
	if got.RefundStatus != refundUnknown || got.StripeRefundID != "re_dead" {
		t.Fatalf("after the unclear re-drive: (%q, %q), want (unknown, re_dead)", got.RefundStatus, got.StripeRefundID)
	}
	if readsOfDead() != 1 {
		t.Fatalf("reads of re_dead = %d, want 1", readsOfDead())
	}

	refunder.failWith = nil
	out, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("RetryRefund: %v", err)
	}
	if readsOfDead() != 1 {
		t.Fatalf("reads of re_dead = %d, want still 1: the unknown attempt is not re_dead", readsOfDead())
	}
	if len(refunder.findCalls) != 2 {
		t.Fatalf("Stripe lookups = %d, want 2 (one per retry)", len(refunder.findCalls))
	}
	if len(refunder.calls) != 2 || refunder.calls[1].idempotencyKey != refunder.calls[0].idempotencyKey {
		t.Fatalf("requests = %+v, want 2 on one key", refunder.calls)
	}
	if out.RefundStatus != domain.RefundSucceeded || out.StripeRefundID == "re_dead" {
		t.Fatalf("refund leg = (%q, %q), want a new succeeded refund", out.RefundStatus, out.StripeRefundID)
	}
}

// A resend can be answered from Stripe's saved response to the first request,
// which may be days old: the refund it names may have failed since. Retry
// records the refund's current status, read fresh, never the saved one.
func TestRetryRefund_ResendRecordsTheRefundsCurrentStatus(t *testing.T) {
	t.Parallel()
	svc, _, _, refunder := setupRefundSvc(t)
	cn := issueUnknownRefund(t, svc, refunder)
	// The saved response says succeeded; Stripe now says failed.
	refunder.returnStatus = domain.RefundSucceeded
	refunder.getStatusOf = map[string]domain.RefundStatus{"re_fake_2": domain.RefundFailed}

	out, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("RetryRefund: %v", err)
	}
	if out.StripeRefundID != "re_fake_2" || out.RefundStatus != domain.RefundFailed {
		t.Fatalf("refund leg = (%q, %q), want (re_fake_2, failed): the saved response is stale", out.StripeRefundID, out.RefundStatus)
	}

	// If the read fails, nothing is recorded: the note stays unknown.
	svc2, store2, _, refunder2 := setupRefundSvc(t)
	cn2 := issueUnknownRefund(t, svc2, refunder2)
	refunder2.getErr = errors.New("stripe get refund: timeout")
	if _, err := svc2.RetryRefund(context.Background(), "t1", cn2.ID); err == nil {
		t.Fatal("RetryRefund returned no error when the status read failed")
	}
	if got := store2.notes[cn2.ID]; got.RefundStatus != refundUnknown || got.StripeRefundID != "" {
		t.Fatalf("after a failed status read: (%q, %q), want (unknown, \"\")", got.RefundStatus, got.StripeRefundID)
	}
}

// On a resend, an error Stripe raised before running it (an invalid API key,
// a key without refund permission) is about that resend only: Stripe saved no
// result for the key, so the earlier request's result is still unknown. The
// note stays unknown with the same key, the caller is told the result is
// unknown (never "not refunded"), and the next retry resends the same request
// again. On a first request the same error is a refusal: nothing ran, so
// nothing was refunded.
func TestRetryRefund_ResendStripeDidNotRunStaysUnknown(t *testing.T) {
	t.Parallel()
	svc, store, _, refunder := setupRefundSvc(t)
	cn := issueUnknownRefund(t, svc, refunder)

	refunder.failWith = errRefundNotRun
	_, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	var oe interface{ OutcomeUnknown() bool }
	if !errors.As(err, &oe) || !oe.OutcomeUnknown() {
		t.Fatalf("RetryRefund error = %v, want one that says the result is unknown", err)
	}
	var sm interface{ OperatorSafeMessage() string }
	if !errors.As(err, &sm) || strings.Contains(sm.OperatorSafeMessage(), "not refunded") {
		t.Fatalf("operator message = %q, want one that does not say the customer was not refunded", safeMessage(err))
	}
	if got := store.notes[cn.ID].RefundStatus; got != refundUnknown {
		t.Fatalf("refund_status after a resend Stripe did not run = %q, want unknown", got)
	}

	refunder.failWith = nil
	out, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("RetryRefund: %v", err)
	}
	keys := map[string]bool{}
	for _, c := range refunder.calls {
		keys[c.idempotencyKey] = true
	}
	if len(refunder.calls) != 3 || len(keys) != 1 {
		t.Fatalf("requests = %d with %d keys, want 3 requests on 1 key", len(refunder.calls), len(keys))
	}
	if out.RefundStatus != domain.RefundSucceeded {
		t.Fatalf("refund_status = %q, want succeeded", out.RefundStatus)
	}

	// Control: the same error on the first request is "failed".
	svc2, _, _, refunder2 := setupRefundSvc(t)
	refunder2.failWith = errRefundNotRun
	cn2, err := svc2.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "fraudulent"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if cn2.RefundStatus != domain.RefundFailed {
		t.Fatalf("refund_status after a first request Stripe did not run = %q, want failed", cn2.RefundStatus)
	}
}

// safeMessage is the operator message an error carries, or "".
func safeMessage(err error) string {
	var sm interface{ OperatorSafeMessage() string }
	if errors.As(err, &sm) {
		return sm.OperatorSafeMessage()
	}
	return ""
}

// A refund asked for through the API runs on the request's context, which
// chi's middleware.Timeout ends after 30 seconds (internal/api/router.go).
// The Stripe client also allows 30 seconds, but its clock starts later, so
// when Stripe hangs, the request context ends first, during the Stripe call.
// The write that records the unclear result must still land. A database write
// on an ended context is refused, which used to leave a retried note at
// "failed" (the #965 label) and an issued one at "none".
func TestRefund_RequestEndsDuringTheStripeCall(t *testing.T) {
	t.Parallel()

	t.Run("Retry refund on a failed note records unknown and keeps the key", func(t *testing.T) {
		t.Parallel()
		svc, store, _, refunder := setupRefundSvc(t)
		cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
			TenantID: "t1", InvoiceID: "inv_paid", CustomerID: "cus_1",
			CreditNoteNumber: "CN-RETRY-DEADLINE", Status: domain.CreditNoteIssued,
			Reason: "retry", SubtotalCents: 5000, TotalCents: 5000,
			RefundAmountCents: 5000, Currency: "USD", RefundStatus: domain.RefundFailed,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		refunder.duringCreate = cancel

		_, err = svc.RetryRefund(ctx, "t1", cn.ID)
		var oe interface{ OutcomeUnknown() bool }
		if !errors.As(err, &oe) || !oe.OutcomeUnknown() {
			t.Fatalf("RetryRefund error = %v, want one that says the result is unknown", err)
		}
		got := store.notes[cn.ID]
		if got.RefundStatus != refundUnknown {
			t.Fatalf("refund_status = %q, want unknown: Stripe may have made the refund", got.RefundStatus)
		}
		if len(refunder.calls) != 1 || got.RefundUnknownKey != refunder.calls[0].idempotencyKey {
			t.Fatalf("stored key = %q, want the key of the request sent (%+v)", got.RefundUnknownKey, refunder.calls)
		}
	})

	t.Run("Issue records unknown with its key and returns the issued note", func(t *testing.T) {
		t.Parallel()
		svc, store, _, refunder := setupRefundSvc(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		refunder.duringCreate = cancel

		// The API answers with this note, so the caller sees "unknown"
		// rather than an error after the note was issued.
		cn, err := svc.CreateRefund(ctx, "t1", RefundInput{InvoiceID: "inv_paid", Reason: "fraudulent"})
		if err != nil {
			t.Fatalf("CreateRefund: %v (want the issued note with refund_status unknown)", err)
		}
		if cn.Status != domain.CreditNoteIssued || cn.RefundStatus != refundUnknown {
			t.Fatalf("returned note = (%q, %q), want (issued, unknown)", cn.Status, cn.RefundStatus)
		}
		got := store.notes[cn.ID]
		if got.RefundStatus != refundUnknown || got.RefundUnknownKey != "velox_cn_"+cn.ID {
			t.Fatalf("stored refund leg = (%q, key %q), want (unknown, velox_cn_%s)", got.RefundStatus, got.RefundUnknownKey, cn.ID)
		}
	})
}

// seedIssuedNoneRefund is an issued note whose refund leg has no recorded
// outcome: Issue sent velox_cn_<id> to Stripe (or was about to), then crashed
// or lost its write before recording the answer.
func seedIssuedNoneRefund(t *testing.T, store *memStore) domain.CreditNote {
	t.Helper()
	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		TenantID: "t1", InvoiceID: "inv_paid", CustomerID: "cus_1",
		CreditNoteNumber: "CN-NONE", Status: domain.CreditNoteIssued,
		Reason: "fraudulent", SubtotalCents: 5000, TotalCents: 5000,
		RefundAmountCents: 5000, Currency: "USD", RefundStatus: domain.RefundNone,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return cn
}

// From "none", Issue's request may still be running at Stripe, or may yet be
// made by Stripe's incident recovery, and its outcome was never recorded. So
// Retry refund resends that same request, velox_cn_<id>, as it does from
// "unknown", and reads the refund's current status after it. A new key could
// start a second refund (https://docs.stripe.com/error-low-level#content-errors).
func TestRetryRefund_NoneResendsTheIssueRequest(t *testing.T) {
	t.Parallel()

	t.Run("the resend reuses the issue key and records the current status", func(t *testing.T) {
		t.Parallel()
		svc, store, _, refunder := setupRefundSvc(t)
		cn := seedIssuedNoneRefund(t, store)
		// Stripe's saved answer to Issue's request says succeeded; the refund
		// has failed since.
		refunder.returnStatus = domain.RefundSucceeded
		refunder.getStatusOf = map[string]domain.RefundStatus{"re_fake_1": domain.RefundFailed}

		out, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
		if err != nil {
			t.Fatalf("RetryRefund: %v", err)
		}
		if len(refunder.findCalls) != 1 {
			t.Fatalf("Stripe lookups before the resend = %d, want 1", len(refunder.findCalls))
		}
		if len(refunder.calls) != 1 || refunder.calls[0].idempotencyKey != "velox_cn_"+cn.ID {
			t.Fatalf("requests = %+v, want one with Issue's key velox_cn_%s", refunder.calls, cn.ID)
		}
		if out.StripeRefundID != "re_fake_1" || out.RefundStatus != domain.RefundFailed {
			t.Fatalf("refund leg = (%q, %q), want (re_fake_1, failed): the saved answer is stale", out.StripeRefundID, out.RefundStatus)
		}
	})

	t.Run("a resend with no clear answer is unknown with the issue key", func(t *testing.T) {
		t.Parallel()
		svc, store, _, refunder := setupRefundSvc(t)
		cn := seedIssuedNoneRefund(t, store)
		refunder.failWith = errRefundNoAnswer

		if _, err := svc.RetryRefund(context.Background(), "t1", cn.ID); err == nil {
			t.Fatal("RetryRefund returned no error for an unclear answer")
		}
		got := store.notes[cn.ID]
		if got.RefundStatus != refundUnknown || got.RefundUnknownKey != "velox_cn_"+cn.ID {
			t.Fatalf("refund leg = (%q, key %q), want (unknown, velox_cn_%s)", got.RefundStatus, got.RefundUnknownKey, cn.ID)
		}
	})

	t.Run("a resend Stripe never ran is unknown, not failed", func(t *testing.T) {
		t.Parallel()
		svc, store, _, refunder := setupRefundSvc(t)
		cn := seedIssuedNoneRefund(t, store)
		refunder.failWith = errRefundNotRun

		_, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
		if strings.Contains(safeMessage(err), "not refunded") {
			t.Fatalf("operator message = %q: Issue's request may have made the refund", safeMessage(err))
		}
		got := store.notes[cn.ID]
		if got.RefundStatus != refundUnknown || got.RefundUnknownKey != "velox_cn_"+cn.ID {
			t.Fatalf("refund leg = (%q, key %q), want (unknown, velox_cn_%s)", got.RefundStatus, got.RefundUnknownKey, cn.ID)
		}
	})
}
