package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestListPendingClawbackDrafts_DefersInFlightSource is the real-Postgres proof
// of the ADR-059 reconciler gate: a clawback draft whose source invoice's
// payment is in flight (processing/unknown) is EXCLUDED from the scan, and
// becomes eligible the moment the source settles — with NO time window, so a
// draft far older than the prior 24h bound still issues once its (slow ACH/SEPA)
// source settles. The in-memory store can't model the cross-table NOT-EXISTS
// gate; this proves it against real SQL.
func TestListPendingClawbackDrafts_DefersInFlightSource(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Defer Gate")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_defer", DisplayName: "Defer",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issued := now
	inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      "INV-DEFER-1",
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentProcessing, // in flight
		Currency:           "USD",
		SubtotalCents:      10000,
		TotalAmountCents:   10000,
		AmountDueCents:     10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}

	cnStore := creditnote.NewPostgresStore(db)
	draft, err := cnStore.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:        inv.ID,
		CustomerID:       cust.ID,
		CreditNoteNumber: "CN-DEFER-1",
		Status:           domain.CreditNoteDraft,
		Reason:           "subscription_cancellation",
		SubtotalCents:    4000,
		TotalCents:       4000,
		Currency:         "USD",
		RefundStatus:     domain.RefundNone,
		IssuePending:     true,
	})
	if err != nil {
		t.Fatalf("create clawback draft: %v", err)
	}

	mustCount := func(want int, msg string) {
		t.Helper()
		drafts, err := cnStore.ListPendingClawbackDrafts(ctx, 100, false)
		if err != nil {
			t.Fatalf("list pending clawback drafts: %v", err)
		}
		if len(drafts) != want {
			t.Fatalf("%s: got %d pending drafts, want %d", msg, len(drafts), want)
		}
	}

	// 1. Source in flight (processing) → the draft is deferred, excluded from the scan.
	mustCount(0, "in-flight source must be skipped by the reconciler scan")

	// 2. Backdate the draft well past the old 24h window. A slow ACH/SEPA source
	//    can settle days later, so the scan must NOT age the draft out.
	backdateCreditNote(t, db, draft.ID)
	mustCount(0, "aged draft on an in-flight source must STILL be skipped (gate is source state, not age)")

	// 3. Source settles → the draft becomes eligible despite being >24h old,
	//    proving the 24h window was removed (else this would return 0).
	if _, err := invStore.MarkPaid(ctx, tenantID, inv.ID, "pi_defer", now); err != nil {
		t.Fatalf("settle source: %v", err)
	}
	mustCount(1, "settled source must make even an aged draft eligible — no 24h window")
}

// The other answer to a written-off park: the card charge did succeed. The
// card settlement records it (uncollectible -> paid, ADR-113: money that
// already moved is recorded), the held relief draft is released, and Issue
// takes the paid branch: the unused time goes to the customer's balance, the
// same money as a cancel on an invoice paid before the write-off (ADR-050).
// The draft must not have lowered the amount due first: that would have
// asked the customer for 6000c while the card took 10000c.
//
// Mutation seam: release an uncollectible source in the held-draft scan again
// (step 1 lists the draft while the charge is unknown).
func TestListPendingClawbackDrafts_ParkedWrittenOffSource_CardSucceeded_CreditsTheBalance(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Parked Card Succeeded")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_parked_paid", DisplayName: "Parked Paid",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-PARKED-PAID-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentUnknown,
		Currency: "USD", SubtotalCents: 10000, TotalAmountCents: 10000, AmountDueCents: 10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	}, nil)
	if err != nil {
		t.Fatalf("create parked invoice: %v", err)
	}
	if _, err := invStore.UpdateStatusWithReversalPrior(ctx, tenantID, inv.ID, domain.InvoiceUncollectible, nil); err != nil {
		t.Fatalf("write off parked invoice: %v", err)
	}
	cnStore := creditnote.NewPostgresStore(db)
	draft, err := cnStore.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-PARKED-PAID-1",
		Status: domain.CreditNoteDraft, Reason: "subscription_cancellation",
		SubtotalCents: 4000, TotalCents: 4000, Currency: "USD",
		RefundStatus: domain.RefundNone, IssuePending: true,
	})
	if err != nil {
		t.Fatalf("create clawback draft: %v", err)
	}
	pending := func() int {
		t.Helper()
		drafts, err := cnStore.ListPendingClawbackDrafts(ctx, 100, false)
		if err != nil {
			t.Fatalf("list pending clawback drafts: %v", err)
		}
		return len(drafts)
	}

	// 1. Written off and parked: the draft waits for the charge's answer.
	if n := pending(); n != 0 {
		t.Fatalf("%d pending drafts while the charge is unknown, want 0", n)
	}

	// 2. Stripe answers: the charge succeeded. The card settlement's write.
	paid, transitioned, err := invStore.MarkPaidCardSettlementTransition(ctx, tenantID, inv.ID, "pi_found", time.Now().UTC(), 0, nil)
	if err != nil || !transitioned {
		t.Fatalf("card settle: transitioned %v, err %v", transitioned, err)
	}
	if paid.Status != domain.InvoicePaid || paid.AmountPaidCents != 10000 {
		t.Fatalf("after the card settle: invoice %s, %dc paid; want paid, 10000c", paid.Status, paid.AmountPaidCents)
	}
	if n := pending(); n != 1 {
		t.Fatalf("%d pending drafts after the charge settled, want 1", n)
	}

	// 3. Issue takes the paid branch: 4000c to the balance.
	credits := credit.NewService(credit.NewPostgresStore(db))
	svc := creditnote.NewService(cnStore, invStore, nil, ledgerGranter{credits})
	got, err := svc.Issue(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if got.Status != domain.CreditNoteIssued || got.CreditAmountCents != 4000 {
		t.Fatalf("note %s crediting %dc, want issued crediting 4000c to the balance", got.Status, got.CreditAmountCents)
	}
	bal, err := testutil.BalanceUSD(credits.GetBalance(ctx, tenantID, cust.ID))
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.BalanceCents != 4000 {
		t.Errorf("customer balance = %dc, want 4000c: paid 10000c, 4000c of it unused", bal.BalanceCents)
	}
	if after, err := invStore.Get(ctx, tenantID, inv.ID); err != nil || after.Status != domain.InvoicePaid || after.AmountPaidCents != 10000 {
		t.Errorf("invoice after the note = %+v (err %v), want paid with 10000c paid", after.Status, err)
	}
}

// A relief draft on an invoice whose charge is unknown on a PaymentIntent
// waits like a draft on any in-flight source, and a write-off does not change
// that: the write-off closes the invoice, it does not answer the charge. A
// written-off invoice is billed but unpaid (ADR-050, amendment 2026-10-23),
// so its relief is issued like an unpaid invoice's: it lowers what the
// invoice asks for.
//
// The wait ends when the charge is answered. Here the unknown sweep finds the
// PaymentIntent took no payment and ends the attempt (EndAttemptWithoutPayment,
// which admits a written-off invoice). The draft is then listed, and Issue
// applies it to the written-off invoice.
//
// This state is reached more often than before ADR-137: a write-off is now
// allowed on any unknown invoice once its charge hold has ended.
//
// Mutation seams: put 'uncollectible' back in the scan's status term (step 2
// lists the draft), drop 'uncollectible' from EndAttemptWithoutPayment (step 3
// never releases it), or hold an uncollectible source in issueHeld (step 4
// voids the draft).
func TestListPendingClawbackDrafts_UnknownWrittenOffSourceWaitsThenIssues(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Unknown Written Off Source")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_unknown_wo", DisplayName: "Unknown Written Off",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issued := now
	inv, err := invStore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-UNKNOWN-WO-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
		SubtotalCents: 10000, TotalAmountCents: 10000, AmountDueCents: 10000,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	// The charge's stamp: unknown on pi_X, before any confirm.
	if _, applied, err := invStore.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentUnknown, "pi_X", ""); err != nil || !applied {
		t.Fatalf("stamp unknown on pi_X: applied=%v err=%v", applied, err)
	}

	cnStore := creditnote.NewPostgresStore(db)
	draft, err := cnStore.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: cust.ID, CreditNoteNumber: "CN-UNKNOWN-WO-1",
		Status: domain.CreditNoteDraft, Reason: "subscription_cancellation",
		SubtotalCents: 4000, TotalCents: 4000, Currency: "USD",
		RefundStatus: domain.RefundNone, IssuePending: true,
	})
	if err != nil {
		t.Fatalf("create clawback draft: %v", err)
	}
	mustCount := func(want int, msg string) {
		t.Helper()
		drafts, err := cnStore.ListPendingClawbackDrafts(ctx, 100, false)
		if err != nil {
			t.Fatalf("list pending clawback drafts: %v", err)
		}
		if len(drafts) != want {
			t.Fatalf("%s: got %d pending drafts, want %d", msg, len(drafts), want)
		}
	}

	// 1. Open, unknown on pi_X: the draft waits.
	mustCount(0, "an unknown source on an open invoice must defer: its charge may yet be confirmed")

	// 2. Written off, still unknown: the draft still waits.
	if _, err := invStore.UpdateStatusWithReversalPrior(ctx, tenantID, inv.ID, domain.InvoiceUncollectible, nil); err != nil {
		t.Fatalf("write off the unknown invoice: %v", err)
	}
	if after, err := invStore.Get(ctx, tenantID, inv.ID); err != nil || after.PaymentStatus != domain.PaymentUnknown || after.StripePaymentIntentID != "pi_X" {
		t.Fatalf("after the write-off: %q on %q (%v), want unknown on pi_X: the write-off does not answer the charge", after.PaymentStatus, after.StripePaymentIntentID, err)
	}
	mustCount(0, "an unknown written-off source must defer like an open one")

	// 3. The unknown sweep finds pi_X took no payment and ends the attempt.
	//    The draft is now eligible.
	if status, ended, err := invStore.EndAttemptWithoutPayment(ctx, tenantID, inv.ID, "pi_X", domain.AttemptReasonNeverConfirmed); err != nil || !ended || status != domain.PaymentPending {
		t.Fatalf("end the attempt: status %q ended %v err %v, want pending, ended", status, ended, err)
	}
	mustCount(1, "an answered charge must release the draft")

	// 4. Issue applies it to the written-off invoice: 4000c less to pay, still
	//    written off.
	svc := creditnote.NewService(cnStore, invStore, nil)
	got, err := svc.Issue(ctx, tenantID, draft.ID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if got.Status != domain.CreditNoteIssued {
		t.Fatalf("credit note status = %s, want issued: a written-off invoice's relief lowers what it asks for", got.Status)
	}
	after, err := invStore.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if after.Status != domain.InvoiceUncollectible || after.AmountDueCents != 6000 {
		t.Fatalf("invoice after the note: %s with %dc due, want uncollectible with 6000c due", after.Status, after.AmountDueCents)
	}
}

// backdateCreditNote pushes a credit note's updated_at 10 days into the past to
// prove the reconciler scan has no time window (the prior 24h bound would have
// dropped it). TxBypass: the test is RLS-agnostic infrastructure setup.
func backdateCreditNote(t *testing.T, db *postgres.DB, id string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin backdate tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE credit_notes SET updated_at = now() - interval '10 days' WHERE id = $1`, id); err != nil {
		t.Fatalf("backdate credit note: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}
