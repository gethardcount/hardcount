package creditnote

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// fakeTaxReverser records reversal calls and returns a deterministic
// upstream transaction id so tests can assert idempotency + persistence.
type fakeTaxReverser struct {
	calls    []tax.ReversalRequest
	failWith error
	returnID string
	// emptyResult makes ReverseTax return an empty ReversalResult with no
	// error — what Engine.ReverseTax returns when no provider resolves.
	emptyResult bool
}

func (f *fakeTaxReverser) ReverseTax(_ context.Context, _ string, req tax.ReversalRequest) (*tax.ReversalResult, error) {
	f.calls = append(f.calls, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.emptyResult {
		return &tax.ReversalResult{}, nil
	}
	id := f.returnID
	if id == "" {
		id = "tx_reversal_fake"
	}
	return &tax.ReversalResult{TransactionID: id}, nil
}

// TestCreate_ProportionalTaxBreakdown verifies Create() splits the gross
// CN subtotal into net + tax using the invoice's tax ratio. Without this,
// the CN would show the gross as net and claim zero tax was collected —
// wrong for any tax-bearing invoice.
func TestCreate_ProportionalTaxBreakdown(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_taxed": {
				ID: "inv_taxed", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, Currency: "USD",
				SubtotalCents:    10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800,
				AmountDueCents:   11800,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})

	// Half refund → should break out half the tax.
	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_taxed",
		Reason:    "partial adjustment",
		Lines: []CreditLineInput{
			{Description: "adj", Quantity: 1, UnitAmountCents: 5900},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// taxAmount = 5900 * 1800 / 11800 = 900
	if cn.TaxAmountCents != 900 {
		t.Errorf("tax_amount_cents: got %d, want 900 (5900 * 1800 / 11800)", cn.TaxAmountCents)
	}
	if cn.SubtotalCents != 5000 {
		t.Errorf("subtotal_cents (net): got %d, want 5000 (5900 - 900 tax)", cn.SubtotalCents)
	}
	if cn.TotalCents != 5900 {
		t.Errorf("total_cents (gross): got %d, want 5900 (unchanged gross)", cn.TotalCents)
	}
}

// TestCreate_ZeroTaxInvoiceLeavesZeroTaxOnCN verifies the proportional
// block preserves legacy behaviour when the invoice has no tax: the CN
// reports zero tax and subtotal == total == sum of line amounts. Existing
// tests and integrations that assume this invariant continue to work.
func TestCreate_ZeroTaxInvoiceLeavesZeroTaxOnCN(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_notax": {
				ID: "inv_notax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, Currency: "USD",
				SubtotalCents:    10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 0},
				TotalAmountCents: 10000,
				AmountDueCents:   10000,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})

	cn, err := svc.Create(context.Background(), "t1", CreateInput{
		InvoiceID: "inv_notax", Reason: "credit",
		Lines: []CreditLineInput{{Description: "x", Quantity: 1, UnitAmountCents: 3000}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cn.TaxAmountCents != 0 {
		t.Errorf("tax_amount_cents: got %d, want 0", cn.TaxAmountCents)
	}
	if cn.SubtotalCents != 3000 {
		t.Errorf("subtotal_cents: got %d, want 3000", cn.SubtotalCents)
	}
	if cn.TotalCents != 3000 {
		t.Errorf("total_cents: got %d, want 3000", cn.TotalCents)
	}
}

// TestIssue_TaxReversalOnPaidInvoiceWithTransaction verifies the happy
// path: a paid invoice with a committed tax_transaction triggers a
// partial reversal at the CN's gross total, and the returned upstream id
// is persisted onto the CN.
func TestIssue_TaxReversalOnPaidInvoiceWithTransaction(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_1",
				TaxTransactionID:      "tx_upstream_1",
			},
		},
	}
	refunder := &fakeRefunder{}
	rev := &fakeTaxReverser{returnID: "tx_reversal_1"}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_paid", Reason: "duplicate",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if len(rev.calls) != 1 {
		t.Fatalf("reverse calls: got %d, want 1", len(rev.calls))
	}
	call := rev.calls[0]
	if call.OriginalTransactionID != "tx_upstream_1" {
		t.Errorf("OriginalTransactionID: got %q, want tx_upstream_1", call.OriginalTransactionID)
	}
	if call.Mode != tax.ReversalModePartial {
		t.Errorf("Mode: got %q, want partial", call.Mode)
	}
	if call.GrossAmountCents != 11800 {
		t.Errorf("GrossAmountCents: got %d, want 11800", call.GrossAmountCents)
	}
	if call.CreditNoteID == "" {
		t.Error("CreditNoteID should be set for idempotency")
	}
	// Verify reversal id got persisted.
	stored, err := store.Get(context.Background(), "t1", cn.ID)
	if err != nil {
		t.Fatalf("Get stored CN: %v", err)
	}
	if stored.TaxTransactionID != "tx_reversal_1" {
		t.Errorf("stored TaxTransactionID: got %q, want tx_reversal_1", stored.TaxTransactionID)
	}
}

// TestIssue_NoReversalWhenInvoiceHasNoTransactionID covers tenants on
// manual/none providers (or legacy invoices from before the commit
// column existed). The reverser must not be called — invoking it with
// an empty OriginalTransactionID would produce a provider error.
func TestIssue_NoReversalWhenInvoiceHasNoTransactionID(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_manual": {
				ID: "inv_manual", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_2",
				// TaxTransactionID deliberately empty.
			},
		},
	}
	refunder := &fakeRefunder{}
	rev := &fakeTaxReverser{}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	_, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_manual", Reason: "duplicate",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Errorf("reverse calls: got %d, want 0 (no upstream state)", len(rev.calls))
	}
}

// TestIssue_NoReverserWiredDoesNotCall covers the narrow-test and
// no-provider-configured paths. Without SetTaxReverser the CN issues
// normally without crashing.
func TestIssue_NoReverserWiredDoesNotCall(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", TotalAmountCents: 10000, AmountPaidCents: 10000,
				StripePaymentIntentID: "pi_3",
				TaxTransactionID:      "tx_upstream_x",
			},
		},
	}
	refunder := &fakeRefunder{}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_paid", Reason: "duplicate",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if cn.Status != domain.CreditNoteIssued {
		t.Errorf("status: got %q, want issued", cn.Status)
	}
	if cn.TaxTransactionID != "" {
		t.Errorf("TaxTransactionID: got %q, want empty (no reverser)", cn.TaxTransactionID)
	}
}

// TestIssue_ReversalFailureStillIssuesCN — parity with the Stripe-refund
// failure policy: the CN is an accounting document, so a provider error
// on the tax reversal is logged (warn) but the CN still transitions to
// issued. Operators reconcile manually.
func TestIssue_ReversalFailureStillIssuesCN(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800, TaxProvider: "stripe_tax"},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_4",
				TaxTransactionID:      "tx_upstream_2",
			},
		},
	}
	store.invoices = invoices
	// Every separate marker write fails: the marker must not depend on one.
	store.setPendingErr = errors.New("db: connection reset")
	refunder := &fakeRefunder{}
	rev := &fakeTaxReverser{failWith: errors.New("stripe: api error")}
	svc := NewService(store, invoices, refunder, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{
		InvoiceID: "inv_paid", Reason: "fraudulent",
	})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if cn.Status != domain.CreditNoteIssued {
		t.Errorf("status: got %q, want issued (CN issues even on reversal failure)", cn.Status)
	}
	if cn.TaxTransactionID != "" {
		t.Errorf("TaxTransactionID: got %q, want empty on failure", cn.TaxTransactionID)
	}
	if !cn.TaxReversalPending {
		t.Error("TaxReversalPending: got false, want true — issuing marks the reversal as owed, so a failed reversal needs no second write for the sweep to re-drive it")
	}
	if len(rev.calls) != 1 {
		t.Errorf("reverse calls: got %d, want 1", len(rev.calls))
	}
}

// TestRetryPendingCreditNoteTaxReversal_ReDrivesAndClears proves the CN-scoped
// reconciler closes the over-remit gap #310 can't see: it re-drives a failed
// credit-note tax reversal and clears the marker on success. The first inline
// attempt fails (marked pending); the sweep, once Stripe recovers, reverses and
// stamps the transaction id.
func TestRetryPendingCreditNoteTaxReversal_ReDrivesAndClears(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_paid": {
				ID: "inv_paid", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1800, TaxProvider: "stripe_tax"},
				TotalAmountCents: 11800, AmountPaidCents: 11800,
				StripePaymentIntentID: "pi_5",
				TaxTransactionID:      "tx_upstream_3",
			},
		},
	}
	store.invoices = invoices
	rev := &fakeTaxReverser{failWith: errors.New("stripe: transient")}
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})
	svc.SetTaxReverser(rev)

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_paid", Reason: "fraudulent"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if !cn.TaxReversalPending {
		t.Fatal("precondition: CN must be marked tax_reversal_pending after the failed inline reversal")
	}

	// Stripe recovers; the sweep must re-drive and clear the marker.
	rev.failWith = nil
	rev.returnID = "tx_reversal_recovered"
	n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(postgres.WithLivemode(context.Background(), true), 10)
	if len(sweepErrs) != 0 {
		t.Fatalf("sweep errors: %v", sweepErrs)
	}
	if n != 1 {
		t.Errorf("recovered: got %d, want 1", n)
	}

	got, _ := store.Get(context.Background(), "t1", cn.ID)
	if got.TaxReversalPending {
		t.Error("marker not cleared after a successful re-drive")
	}
	if got.TaxTransactionID != "tx_reversal_recovered" {
		t.Errorf("reversal tx id: got %q, want tx_reversal_recovered", got.TaxTransactionID)
	}
	if len(rev.calls) != 2 {
		t.Errorf("reverse calls: got %d, want 2 (failed inline + recovered sweep)", len(rev.calls))
	}
}

// A note is issued and the process stops before the reversal call (here: no
// reverser is wired while it is issued, so the call is never made). The issue
// statement already marked the reversal as owed, so the sweep makes it on a
// later tick. Before, the marker was written only after a failed call, and
// such a note was found only for 24 hours, by a second arm of the list.
func TestRetryPendingCreditNoteTaxReversal_RecoversANoteWhoseInlineReversalNeverRan(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_tax": {
				ID: "inv_tax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1000, TaxProvider: "stripe_tax"},
				TotalAmountCents: 11000, AmountPaidCents: 11000,
				StripePaymentIntentID: "pi_never_ran",
				TaxTransactionID:      "tx_upstream_never_ran",
			},
		},
	}
	store.invoices = invoices
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetNumberGenerator(&fakeCNNumbers{})

	cn, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_tax", Reason: "fraudulent"})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if !cn.TaxReversalPending || cn.TaxTransactionID != "" {
		t.Fatalf("after issue: pending=%v tx=%q, want the reversal marked as owed and not yet made", cn.TaxReversalPending, cn.TaxTransactionID)
	}

	rev := &fakeTaxReverser{returnID: "tx_reversal_recovered"}
	svc.SetTaxReverser(rev)
	n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(postgres.WithLivemode(context.Background(), true), 10)
	if len(sweepErrs) != 0 || n != 1 {
		t.Fatalf("sweep: reversed=%d errs=%v, want 1 and none", n, sweepErrs)
	}
	got, _ := store.Get(context.Background(), "t1", cn.ID)
	if got.TaxTransactionID != "tx_reversal_recovered" || got.TaxReversalPending {
		t.Errorf("after the sweep: tx=%q pending=%v, want the reversal stored and the marker cleared", got.TaxTransactionID, got.TaxReversalPending)
	}
	if len(rev.calls) != 1 {
		t.Errorf("reverse calls: got %d, want 1", len(rev.calls))
	}
}

// fakeCreditGranter satisfies the CreditGranter optional dep so
// CreateRefund doesn't blow up when refund_type is credit.
type fakeCreditGranter struct {
	cnCalls                                        []fakeCreditGranterCNCall
	seenCNIDs                                      map[string]bool
	commitGranted, commitConsumed, commitCNRetired int64
	commitFound                                    bool
	retiredSlices, refundedGross                   []int64
}

type fakeCreditGranterCNCall struct {
	creditNoteID string
	input        CreditGrantInput
}

// GrantForCreditNoteTx ignores the (nil) coordinator tx and records one call
// per credit-note id; a repeat id is ignored. Real in-tx atomicity is covered
// by the PG test.
func (f *fakeCreditGranter) GrantForCreditNoteTx(_ context.Context, _ *sql.Tx, _, creditNoteID string, in CreditGrantInput) error {
	if f.seenCNIDs == nil {
		f.seenCNIDs = make(map[string]bool)
	}
	if f.seenCNIDs[creditNoteID] {
		return nil
	}
	f.seenCNIDs[creditNoteID] = true
	f.cnCalls = append(f.cnCalls, fakeCreditGranterCNCall{creditNoteID: creditNoteID, input: in})
	return nil
}

// Relief-pair stubs: unit doubles carry a configurable commit grant so the
// relief coordinator's math is unit-testable; lock semantics are proven on
// real Postgres.
func (f *fakeCreditGranter) LockCommitGrantForReliefTx(_ context.Context, _ *sql.Tx, _, _ string) (int64, int64, int64, bool, error) {
	return f.commitGranted, f.commitConsumed, f.commitCNRetired, f.commitFound, nil
}

func (f *fakeCreditGranter) RetireCommitSliceForReliefTx(_ context.Context, _ *sql.Tx, _, _, _ string, slice, refundedGross, _ int64) (int64, error) {
	f.retiredSlices = append(f.retiredSlices, slice)
	f.refundedGross = append(f.refundedGross, refundedGross)
	f.commitConsumed += slice
	f.commitCNRetired += slice
	return f.commitGranted - f.commitConsumed, nil
}

// An invoice Stripe Tax recorded with no tax still has a tax transaction: it
// records the sale in Stripe's tax reports, and Stripe reverses it like any
// other (checked in test mode, 2026-11-10: a full and a partial reversal of a
// zero-tax transaction each return a reversal with an id). So a credit note
// on it owes its reversal: the issue statement marks it, and the sweep makes
// the reversal and stores it. Before, the sweep cleared the marker with no
// call, on the belief that Stripe returns no id for such a reversal, and an
// inline reversal that failed at issue was never made.
func TestRetryPendingCreditNoteTaxReversal_ZeroTaxInvoice_IsReversedLikeAnyOther(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_zero_tax": {
				ID: "inv_zero_tax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 0, TaxProvider: "stripe_tax"},
				TotalAmountCents: 10000, AmountPaidCents: 10000,
				StripePaymentIntentID: "pi_zero_tax",
				TaxTransactionID:      "tx_upstream_zero_tax",
			},
		},
	}
	rev := &fakeTaxReverser{returnID: "tx_reversal_zero_tax"}
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetTaxReverser(rev)
	store.invoices = invoices

	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		InvoiceID: "inv_zero_tax", CustomerID: "cus_1", Status: domain.CreditNoteIssued,
		TotalCents: 1000, TaxReversalPending: true, TaxTransactionID: "",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(postgres.WithLivemode(context.Background(), true), 10)
	if len(sweepErrs) != 0 || n != 1 {
		t.Fatalf("sweep: reversed=%d errs=%v, want 1 and none", n, sweepErrs)
	}
	if len(rev.calls) != 1 || rev.calls[0].OriginalTransactionID != "tx_upstream_zero_tax" || rev.calls[0].GrossAmountCents != 1000 ||
		rev.calls[0].Mode != tax.ReversalModePartial {
		t.Fatalf("reverse calls = %+v, want one partial reversal against the invoice's transaction for the note's total", rev.calls)
	}
	got, _ := store.Get(context.Background(), "t1", cn.ID)
	if got.TaxTransactionID != "tx_reversal_zero_tax" || got.TaxReversalPending {
		t.Errorf("after the sweep: tx=%q pending=%v, want the reversal stored and the marker cleared", got.TaxTransactionID, got.TaxReversalPending)
	}

	// A note on the same invoice, issued now, is marked as owing its reversal.
	svc.SetNumberGenerator(&fakeCNNumbers{})
	rev.failWith = errors.New("stripe: 500")
	fresh, err := svc.CreateRefund(context.Background(), "t1", RefundInput{InvoiceID: "inv_zero_tax", Reason: "fraudulent", AmountCents: 2000})
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if !fresh.TaxReversalPending {
		t.Error("a note on a zero-tax invoice whose inline reversal failed must stay marked, so the sweep makes it")
	}
}

// One write stores the reversal and clears the marker, so there is no state
// in which the reversal is recorded and the note is still listed.
func TestRetryPendingCreditNoteTaxReversal_OneWriteStoresTheReversalAndClearsTheMarker(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_tax": {
				ID: "inv_tax", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
				Currency: "USD", SubtotalCents: 10000,
				TaxFacts:         domain.TaxFacts{TaxAmountCents: 1000},
				TotalAmountCents: 11000, AmountPaidCents: 11000,
				TaxTransactionID: "tx_upstream",
			},
		},
	}
	rev := &fakeTaxReverser{returnID: "tx_reversal_ok"}
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetTaxReverser(rev)
	store.invoices = invoices

	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		InvoiceID: "inv_tax", CustomerID: "cus_1", Status: domain.CreditNoteIssued,
		TotalCents: 5500, TaxReversalPending: true, TaxTransactionID: "",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := store.setPendingCalls
	if _, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(postgres.WithLivemode(context.Background(), true), 10); len(sweepErrs) != 0 {
		t.Fatalf("sweep errors: %v", sweepErrs)
	}
	if got := store.setPendingCalls - before; got != 0 {
		t.Errorf("separate marker writes: got %d, want 0 — the write that stores the reversal clears the marker", got)
	}
	got, _ := store.Get(context.Background(), "t1", cn.ID)
	if got.TaxReversalPending {
		t.Error("marker not cleared")
	}
	if got.TaxTransactionID != "tx_reversal_ok" {
		t.Errorf("reversal tx id: got %q, want tx_reversal_ok", got.TaxTransactionID)
	}
}

// --- a failed re-drive keeps the note marked ---

func seedTaxOrphan(t *testing.T, store *memStore, pending bool) domain.CreditNote {
	t.Helper()
	cn, err := store.Create(context.Background(), "t1", domain.CreditNote{
		InvoiceID: "inv_tax", CustomerID: "cus_1", Status: domain.CreditNoteIssued,
		TotalCents: 5500, TaxReversalPending: pending, TaxTransactionID: "",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return cn
}

func taxInvoiceReader() *memInvoiceReader {
	return &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_tax": {
			ID: "inv_tax", TenantID: "t1", CustomerID: "cus_1",
			Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
			Currency: "USD", SubtotalCents: 10000, TaxFacts: domain.TaxFacts{TaxAmountCents: 1000},
			TotalAmountCents: 11000, AmountPaidCents: 11000, TaxTransactionID: "tx_upstream",
		},
	}}
}

// A re-drive that fails writes nothing to the note. Any write stamps
// updated_at, which the refunds-needing-attention rule reads as the note's
// age: a note re-driven every hour would never be old enough to be listed
// there, and a stuck refund would stay hidden.
func TestRetryPendingCreditNoteTaxReversal_AFailedReDriveWritesNothing(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	rev := &fakeTaxReverser{failWith: errors.New("stripe 503")}
	invoices := taxInvoiceReader()
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetTaxReverser(rev)
	store.invoices = invoices
	cn := seedTaxOrphan(t, store, true)

	if _, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(postgres.WithLivemode(context.Background(), true), 10); len(sweepErrs) != 1 {
		t.Fatalf("errs=%v, want exactly the re-drive error", sweepErrs)
	}
	if store.setPendingCalls != 0 {
		t.Fatalf("marker writes: %d, want 0 — a failed re-drive must not write the note", store.setPendingCalls)
	}
	if got, _ := store.Get(context.Background(), "t1", cn.ID); !got.TaxReversalPending {
		t.Fatal("the marker must stay set after a failed re-drive")
	}
}

// An empty reversal result on a TAX-BEARING invoice (the engine resolved
// no provider) is a failure, not a success: the marker is never cleared and
// the row is not counted reversed. Before 2026-08-30 this path cleared the
// marker and reported advanced=1.
func TestRetryPendingCreditNoteTaxReversal_EmptyResultOnTaxBearingInvoiceIsFailure(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	rev := &fakeTaxReverser{emptyResult: true} // provider returned no transaction
	invoices := taxInvoiceReader()
	svc := NewService(store, invoices, &fakeRefunder{}, &fakeCreditGranter{})
	svc.SetTaxReverser(rev)
	store.invoices = invoices
	cnPending := seedTaxOrphan(t, store, true)

	n, sweepErrs := svc.RetryPendingCreditNoteTaxReversal(postgres.WithLivemode(context.Background(), true), 10)
	if n != 0 {
		t.Fatalf("reversed=%d, want 0 — an empty result reversed nothing", n)
	}
	if len(sweepErrs) != 1 {
		t.Fatalf("errs=%v, want the one re-drive error", sweepErrs)
	}
	got, _ := store.Get(context.Background(), "t1", cnPending.ID)
	if !got.TaxReversalPending {
		t.Error("marker must stay true after an empty-result re-drive")
	}
	if got.TaxTransactionID != "" {
		t.Error("no transaction must be stamped")
	}
}
