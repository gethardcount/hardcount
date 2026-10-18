package creditnote

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// These tests pin #965: a refund whose Stripe result is unclear is recorded
// as "unknown", never "failed", and Retry refund settles it without a second
// refund. The charge path made the same split first (ADR-107).

// refundUnknown is the refund status for "Stripe may or may not have made the
// refund".
const refundUnknown = domain.RefundStatus("unknown")

// outcomeErr is a refund error that says whether Stripe may have made the
// refund, the way the Stripe refunder's errors do.
type outcomeErr struct {
	msg     string
	unknown bool
}

func (e *outcomeErr) Error() string        { return e.msg }
func (e *outcomeErr) OutcomeUnknown() bool { return e.unknown }

var (
	errRefundNoAnswer = &outcomeErr{msg: "stripe refund: An unknown error occurred", unknown: true}
	errRefundRefused  = &outcomeErr{msg: "stripe refund: Charge ch_1 has already been refunded.", unknown: false}
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
	refunder.getStatus = domain.RefundFailed
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
	if len(refunder.getCalls) != 1 {
		t.Fatalf("reads of re_dead = %d, want 1", len(refunder.getCalls))
	}

	refunder.failWith = nil
	out, err := svc.RetryRefund(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("RetryRefund: %v", err)
	}
	if len(refunder.getCalls) != 1 {
		t.Fatalf("reads of re_dead = %d, want still 1: the unknown attempt is not re_dead", len(refunder.getCalls))
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
