package creditnote_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
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
	inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
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
	})
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

// TestListPendingClawbackDrafts_ParkedWrittenOffSourceWaitsThenIssues: a
// relief draft on a parked invoice (payment_status 'unknown', no PaymentIntent
// id: ADR-107) waits like a draft on any in-flight source, and a write-off does
// not change that. A written-off invoice is billed but unpaid (ADR-050,
// amendment 2026-10-23), so its relief is issued like an unpaid invoice's: it
// lowers what the invoice asks for. Until 2026-10-23 a write-off released the
// draft to the reconciler, and Issue voided it as an orphan, so a customer who
// later paid the written-off invoice paid for the unused time too.
//
// The wait ends when the park is answered: the stale-hold sweep asks Stripe
// about the lost charge (ADR-127) and, when Stripe has nothing, writes the
// payment status back to pending or failed. The draft is then eligible, and
// Issue applies it to the written-off invoice.
//
// Mutation seams: put 'uncollectible' back in the scan's status term (step 2
// lists the draft), or hold an uncollectible source in issueHeld (step 4 voids
// the draft).
func TestListPendingClawbackDrafts_ParkedWrittenOffSourceWaitsThenIssues(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Parked Source")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_parked", DisplayName: "Parked",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	issued := now
	inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
		CustomerID:            cust.ID,
		InvoiceNumber:         "INV-PARKED-1",
		Status:                domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentUnknown,
		StripePaymentIntentID: "",
		Currency:              "USD",
		SubtotalCents:         10000,
		TotalAmountCents:      10000,
		AmountDueCents:        10000,
		BillingPeriodStart:    now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:      now,
		IssuedAt:              &issued,
	})
	if err != nil {
		t.Fatalf("create parked invoice: %v", err)
	}

	cnStore := creditnote.NewPostgresStore(db)
	draft, err := cnStore.Create(ctx, tenantID, domain.CreditNote{
		InvoiceID:        inv.ID,
		CustomerID:       cust.ID,
		CreditNoteNumber: "CN-PARKED-1",
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

	// 1. Open and parked: the draft waits.
	mustCount(0, "a parked source on an open invoice must defer: its charge may yet be confirmed")

	// 2. Written off, still parked: the draft still waits. The write-off leaves
	//    payment_status 'unknown' (it does not answer whether the card was
	//    charged), and a written-off invoice can still be paid.
	if _, err := invStore.UpdateStatus(ctx, tenantID, inv.ID, domain.InvoiceUncollectible); err != nil {
		t.Fatalf("write off parked invoice: %v", err)
	}
	if after, err := invStore.Get(ctx, tenantID, inv.ID); err != nil || after.PaymentStatus != domain.PaymentUnknown {
		t.Fatalf("payment_status after the write-off = %q (%v), want 'unknown': the write-off closes the invoice, it does not answer the charge", after.PaymentStatus, err)
	}
	mustCount(0, "a parked written-off source must defer like a parked open one")

	// 3. The stale-hold sweep finds no charge at Stripe and writes the
	//    payment status back (ADR-127). The draft is now eligible.
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET payment_status = 'failed' WHERE id = $1`, inv.ID); err != nil {
		t.Fatalf("answer the park: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mustCount(1, "an answered park must release the draft")

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
		t.Errorf("invoice %s with %dc due, want uncollectible with 6000c due", after.Status, after.AmountDueCents)
	}
	mustCount(0, "an issued draft leaves the scan")
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
