package creditnote

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestCreate_InFlightPaymentGate pins the Part-A gate: an OPERATOR credit note
// must not reduce amount_due while the invoice's payment is in flight
// (processing/unknown) — else MarkPaid at settle records amount_paid off the
// lowered amount_due, under-sizing the refund cap (invoice/postgres.go KNOWN
// EDGE). The AUTOMATED clawback (CreateAndIssueAdjustment, ADR-050) must STILL
// proceed — it calls create() directly, bypassing the gate.
func TestCreate_InFlightPaymentGate(t *testing.T) {
	ctx := context.Background()
	line := []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 1000}}
	mk := func(id string, ps domain.InvoicePaymentStatus, st domain.InvoiceStatus) domain.Invoice {
		return domain.Invoice{
			ID: id, TenantID: "t1", CustomerID: "cus_1",
			Status: st, PaymentStatus: ps, Currency: "USD",
			TotalAmountCents: 10000, AmountDueCents: 10000,
		}
	}
	newSvc := func(inv domain.Invoice) (*Service, *memStore, *memInvoiceReader) {
		store := newMemStore()
		reader := &memInvoiceReader{invoices: map[string]domain.Invoice{inv.ID: inv}}
		s := NewService(store, reader, nil)
		s.SetNumberGenerator(&fakeCNNumbers{})
		return s, store, reader
	}

	t.Run("operator CN on a processing invoice is rejected (InvalidState→409)", func(t *testing.T) {
		s, store, _ := newSvc(mk("inv_proc", domain.PaymentProcessing, domain.InvoiceFinalized))
		_, err := s.Create(ctx, "t1", CreateInput{InvoiceID: "inv_proc", Reason: "downgrade", Lines: line})
		if !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("want ErrInvalidState (→409) for an in-flight payment, got %v", err)
		}
		if len(store.notes) != 0 {
			t.Errorf("no credit note should be created when gated, got %d", len(store.notes))
		}
	})

	t.Run("operator CN on an unknown-payment invoice is rejected", func(t *testing.T) {
		s, _, _ := newSvc(mk("inv_unk", domain.PaymentUnknown, domain.InvoiceFinalized))
		_, err := s.Create(ctx, "t1", CreateInput{InvoiceID: "inv_unk", Reason: "downgrade", Lines: line})
		if !errors.Is(err, errs.ErrInvalidState) {
			t.Errorf("ambiguous (unknown) outcome is in-flight too; want ErrInvalidState, got %v", err)
		}
	})

	t.Run("operator CN on a not-in-flight (pending) finalized invoice is allowed", func(t *testing.T) {
		s, _, _ := newSvc(mk("inv_pend", domain.PaymentPending, domain.InvoiceFinalized))
		if _, err := s.Create(ctx, "t1", CreateInput{InvoiceID: "inv_pend", Reason: "downgrade", Lines: line}); err != nil {
			t.Errorf("pending = no charge in flight; CN must be allowed, got %v", err)
		}
	})

	t.Run("operator CN on a PAID invoice is allowed (refund branch never reduces amount_due)", func(t *testing.T) {
		s, _, _ := newSvc(mk("inv_paid", domain.PaymentSucceeded, domain.InvoicePaid))
		if _, err := s.Create(ctx, "t1", CreateInput{InvoiceID: "inv_paid", Reason: "refund", Lines: line}); err != nil {
			t.Errorf("paid invoice CN must be allowed, got %v", err)
		}
	})

	t.Run("automated clawback on a processing source DEFERS: drafts but does not issue (ADR-059)", func(t *testing.T) {
		s, store, reader := newSvc(mk("inv_auto", domain.PaymentProcessing, domain.InvoiceFinalized))
		cn, err := s.CreateAndIssueAdjustment(ctx, "t1", "inv_auto", 4000, "subscription_cancellation", "unused prebill")
		if err != nil {
			t.Fatalf("automated clawback must proceed (defer) on a processing source, got %v", err)
		}
		// The automated path must create the clawback but DEFER issuance while the
		// source charge is in flight — else amount_due is reduced before MarkPaid
		// records the captured amount (under-record). The note stays a recoverable
		// draft (issue_pending); amount_due is untouched.
		if cn.Status != domain.CreditNoteDraft {
			t.Errorf("status: got %q, want draft (deferred until settle)", cn.Status)
		}
		if !cn.IssuePending {
			t.Error("deferred draft must be issue_pending so the reconciler re-drives it")
		}
		if due := reader.invoices["inv_auto"].AmountDueCents; due != 10000 {
			t.Errorf("amount_due must be UNCHANGED while in flight; got %d, want 10000", due)
		}
		if len(store.notes) != 1 {
			t.Errorf("clawback draft should exist, got %d notes", len(store.notes))
		}
	})
}

// TestAutomatedClawback_DeferUntilSettleCycle proves the full ADR-059 e2e flow:
// an automated clawback against an in-flight source is drafted-not-issued, the
// reconciler SKIPS it while in flight, and once the source settles the reconciler
// issues it down the correct channel — credit when the source settled PAID,
// amount_due reduction when it settled FAILED. Reuses the real Issue() chokepoint
// + RetryPendingClawbackIssue reconciler (per the "verify the real path" rule).
func TestAutomatedClawback_DeferUntilSettleCycle(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), false)

	setup := func(ps domain.InvoicePaymentStatus, st domain.InvoiceStatus) (*Service, *memStore, *memInvoiceReader) {
		store := newMemStore()
		reader := &memInvoiceReader{invoices: map[string]domain.Invoice{
			"inv_1": {
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
				Status: st, PaymentStatus: ps, Currency: "USD",
				TotalAmountCents: 10000, AmountDueCents: 10000,
			},
		}}
		s := NewService(store, reader, nil)
		s.SetNumberGenerator(&fakeCNNumbers{})
		return s, store, reader
	}

	// Create the clawback while the source is processing → drafted, not issued.
	draftOn := func(s *Service) domain.CreditNote {
		cn, err := s.CreateAndIssueAdjustment(ctx, "t1", "inv_1", 4000, "subscription_cancellation", "unused prebill")
		if err != nil {
			t.Fatalf("automated clawback (defer) failed: %v", err)
		}
		if cn.Status != domain.CreditNoteDraft || !cn.IssuePending {
			t.Fatalf("want deferred draft (draft+issue_pending), got status=%q pending=%v", cn.Status, cn.IssuePending)
		}
		return cn
	}

	t.Run("source settles PAID → reconciler issues down the credit channel, amount_due stays full", func(t *testing.T) {
		s, _, reader := setup(domain.PaymentProcessing, domain.InvoiceFinalized)
		draft := draftOn(s)

		// Reconciler tick WHILE still in flight: even if the draft is scanned, the
		// Issue() chokepoint no-ops it — draft stays draft+issue_pending and
		// amount_due is untouched. (Production's SQL source-terminal gate also
		// excludes it from the scan; the memStore can't model that cross-table
		// join, so we assert the chokepoint's effect directly — integration covers
		// the SQL gate.)
		s.RetryPendingClawbackIssue(ctx, 100)
		if got, _ := s.Get(ctx, "t1", draft.ID); got.Status != domain.CreditNoteDraft || !got.IssuePending {
			t.Fatalf("draft must stay draft+pending while in flight, got status=%q pending=%v", got.Status, got.IssuePending)
		}
		if due := reader.invoices["inv_1"].AmountDueCents; due != 10000 {
			t.Fatalf("amount_due must be untouched while in flight; got %d, want 10000", due)
		}

		// Source settles PAID (MarkPaid would set succeeded + amount_paid=full).
		inv := reader.invoices["inv_1"]
		inv.PaymentStatus = domain.PaymentSucceeded
		inv.Status = domain.InvoicePaid
		reader.invoices["inv_1"] = inv

		issued, errs := s.RetryPendingClawbackIssue(ctx, 100)
		if len(errs) != 0 || issued != 1 {
			t.Fatalf("after settle PAID, reconciler should issue 1; got issued=%d errs=%v", issued, errs)
		}
		got, _ := s.Get(ctx, "t1", draft.ID)
		if got.Status != domain.CreditNoteIssued {
			t.Errorf("status after settle: got %q, want issued", got.Status)
		}
		// Paid branch credits the customer — it must NOT reduce amount_due.
		if due := reader.invoices["inv_1"].AmountDueCents; due != 10000 {
			t.Errorf("paid-source clawback must credit (not reduce amount_due); amount_due=%d, want 10000", due)
		}
	})

	t.Run("source settles FAILED → reconciler issues down the reduce channel", func(t *testing.T) {
		s, _, reader := setup(domain.PaymentProcessing, domain.InvoiceFinalized)
		draft := draftOn(s)

		inv := reader.invoices["inv_1"]
		inv.PaymentStatus = domain.PaymentFailed
		reader.invoices["inv_1"] = inv

		issued, errs := s.RetryPendingClawbackIssue(ctx, 100)
		if len(errs) != 0 || issued != 1 {
			t.Fatalf("after settle FAILED, reconciler should issue 1; got issued=%d errs=%v", issued, errs)
		}
		got, _ := s.Get(ctx, "t1", draft.ID)
		if got.Status != domain.CreditNoteIssued {
			t.Errorf("status: got %q, want issued", got.Status)
		}
		// Unpaid branch reduces amount_due to the consumed portion.
		if due := reader.invoices["inv_1"].AmountDueCents; due != 6000 {
			t.Errorf("failed-source clawback must reduce amount_due to consumed; got %d, want 6000 (10000-4000)", due)
		}
	})

	t.Run("source VOIDED before settle → reconciler voids the orphan draft, never applies", func(t *testing.T) {
		s, _, reader := setup(domain.PaymentProcessing, domain.InvoiceFinalized)
		draft := draftOn(s)

		inv := reader.invoices["inv_1"]
		inv.Status = domain.InvoiceVoided
		inv.PaymentStatus = domain.PaymentFailed
		reader.invoices["inv_1"] = inv

		// Reconciler picks it up (source no longer in flight) → Issue() orphan guard
		// voids the draft instead of re-reversing tax the void already handled.
		if _, errs := s.RetryPendingClawbackIssue(ctx, 100); len(errs) != 0 {
			t.Fatalf("orphan handling should not error; got %v", errs)
		}
		got, _ := s.Get(ctx, "t1", draft.ID)
		if got.Status != domain.CreditNoteVoided {
			t.Errorf("orphaned draft (voided source) must be voided; got %q", got.Status)
		}
		if due := reader.invoices["inv_1"].AmountDueCents; due != 10000 {
			t.Errorf("orphan must not apply to amount_due; got %d, want 10000", due)
		}
	})
}
