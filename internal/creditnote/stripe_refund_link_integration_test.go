package creditnote_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// failingIssueAudit fails the credit_note.issued audit row while fail is
// set: an issue that fails after its note was inserted on the same
// transaction.
type failingIssueAudit struct{ fail bool }

func (a *failingIssueAudit) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	if a.fail && e.Action == "credit_note.issued" {
		return errors.New("injected: issue audit write failed")
	}
	return nil
}

type nopOutbox struct{}

func (nopOutbox) Enqueue(context.Context, *sql.Tx, string, string, map[string]any) (string, error) {
	return "evt_test", nil
}

type oneRefundReader struct{ facts domain.StripeRefundFacts }

func (r oneRefundReader) ReadRefund(context.Context, string) (domain.StripeRefundFacts, error) {
	return r.facts, nil
}

// TestLinkAtCreate_IssueFailureLeavesNoHiddenDraft (2026-10-30 review, F1):
// the operator's link of a refund made in Stripe creates and issues its note
// in one transaction. When the issue fails, nothing is left behind: no
// draft holding the refund id (which Void could never remove), and the
// flagged row still needs review, so the notice stays and a retry can link
// it. Once the issue works, the link records it.
func TestLinkAtCreate_IssueFailureLeavesNoHiddenDraft(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN link issue failure")
	store := creditnote.NewPostgresStore(db)
	invStore := invoice.NewPostgresStore(db)
	_, inv := seedInvoiceForCN(t, db, tenantID, "link-f1", domain.InvoicePaid, domain.PaymentSucceeded, "pi_link_f1")

	auditor := &failingIssueAudit{}
	svc := creditnote.NewService(store, invStore, nil)
	svc.SetNumberGenerator(&seqNumbers{prefix: "LINKF1"})
	svc.SetAuditLogger(auditor)
	svc.SetStripeRefundRecording(store, invStore, nopOutbox{})
	svc.SetRefundReader(oneRefundReader{facts: domain.StripeRefundFacts{
		ID: "re_link_f1", PaymentIntentID: "pi_link_f1", AmountCents: 4000, Currency: "usd", Status: domain.StripeRefundSucceeded,
	}})

	// A refund first seen two days late: flagged.
	made := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	if _, err := svc.RecordStripeRefund(ctx, tenantID, domain.StripeRefundSnapshot{
		StripeEventID: "evt_link_f1", EventAt: made.Add(48 * time.Hour), StripeRefundID: "re_link_f1",
		StripePaymentIntentID: "pi_link_f1", AmountCents: 4000, Currency: "usd",
		Status: domain.StripeRefundSucceeded, CreatedAt: made,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	needsReview := func() bool {
		t.Helper()
		rows, err := svc.StripeRefundsNeedingReview(ctx, tenantID, []string{inv.ID})
		if err != nil {
			t.Fatalf("read flagged refunds: %v", err)
		}
		_, ok := rows[inv.ID]
		return ok
	}
	if !needsReview() {
		t.Fatal("the late refund is not flagged")
	}
	input := creditnote.CreateInput{
		InvoiceID: inv.ID, Reason: "Refund made in Stripe", AutoIssue: true,
		RefundAmountCents: 4000, StripeRefundID: "re_link_f1",
		Lines: []creditnote.CreditLineInput{{Description: "Refund made in Stripe", Quantity: 1, UnitAmountCents: 4000}},
	}

	auditor.fail = true
	if _, err := svc.Create(ctx, tenantID, input); err == nil {
		t.Fatal("Create succeeded with the issue failing")
	}
	notes, err := store.List(ctx, creditnote.ListFilter{TenantID: tenantID, InvoiceID: inv.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("credit notes after the failed issue: %+v, want none (no hidden draft)", notes)
	}
	if !needsReview() {
		t.Error("the refund is no longer flagged after the failed link: the notice would clear with nothing recorded")
	}

	auditor.fail = false
	cn, err := svc.Create(ctx, tenantID, input)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if cn.Status != domain.CreditNoteIssued || cn.StripeRefundID != "re_link_f1" || !cn.RecordedFromStripe {
		t.Errorf("linked note = %+v, want issued, holding re_link_f1", cn)
	}
	if needsReview() {
		t.Error("the refund is still flagged after the link")
	}
}
