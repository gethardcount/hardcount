package creditnote_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Credit notes while a charge record exists (ADR-127): a charge was sent to
// Stripe and its result is not saved, so the amount Stripe may capture is the
// amount due before any credit. An operator's credit note is refused; an
// automatic one (a downgrade's or a cancel's relief) is drafted and waits, and
// the ADR-059 reconciler issues it once the record clears.
func TestCreditNote_ChargeRecord_RefusedOrDeferred(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "CN Charge Record")
	_, inv := seedInvoiceForCN(t, db, tenantID, "charge-record", domain.InvoiceFinalized, domain.PaymentPending, "")

	invStore := invoice.NewPostgresStore(db)
	notes := creditnote.NewService(creditnote.NewPostgresStore(db), invStore, nil)
	notes.SetNumberGenerator(tenant.NewSettingsStore(db))

	// The charge path: the claim, then the record, then the hold lapses (a
	// crash or a timeout left the record).
	if ok, err := invStore.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	cur, err := invStore.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	seq := cur.ChargeAttemptSeq
	if ok, err := invStore.BeginChargeAttempt(ctx, tenantID, inv.ID, seq, "velox_inv_cn_0_pm_card", "cus_stripe_cn"); err != nil || !ok {
		t.Fatalf("record the charge: ok=%v err=%v", ok, err)
	}
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 second' WHERE id = $1`, inv.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// An operator's credit note: refused, with the wait in the message.
	_, err = notes.Create(ctx, tenantID, creditnote.CreateInput{
		InvoiceID: inv.ID, Reason: "order_change",
		Lines: []creditnote.CreditLineInput{{Description: "credit", Quantity: 1, UnitAmountCents: 1000}},
	})
	var de *errs.DomainError
	if !errors.Is(err, errs.ErrInvalidState) || !errors.As(err, &de) || de.Code != "payment_in_flight" {
		t.Fatalf("operator credit note while a charge is recorded: err = %v, want a payment_in_flight refusal", err)
	}
	if !strings.Contains(de.Message, "about an hour") {
		t.Errorf("refusal %q does not say an interrupted charge is confirmed within about an hour", de.Message)
	}

	// An automatic credit note: drafted, deferred, the amount due untouched.
	cn, err := notes.CreateAndIssueAdjustment(ctx, tenantID, inv.ID, 4000, "subscription_cancellation", "unused prebill")
	if err != nil {
		t.Fatalf("automatic credit note: %v", err)
	}
	if cn.Status != domain.CreditNoteDraft || !cn.IssuePending {
		t.Fatalf("automatic credit note status=%s pending=%v, want a deferred draft", cn.Status, cn.IssuePending)
	}
	if issued, errs := notes.RetryPendingClawbackIssue(ctx, 100); len(errs) != 0 || issued != 0 {
		t.Fatalf("reconciler while the record exists: issued=%d errs=%v, want nothing issued", issued, errs)
	}
	if got, _ := invStore.Get(ctx, tenantID, inv.ID); got.AmountDueCents != 10000 {
		t.Fatalf("amount_due %d while the charge is recorded, want 10000", got.AmountDueCents)
	}

	// The record clears (Stripe never got the charge): issued on the next run.
	if ok, err := invStore.EndChargeAttempt(ctx, tenantID, inv.ID, seq); err != nil || !ok {
		t.Fatalf("clear the record: ok=%v err=%v", ok, err)
	}
	if issued, errs := notes.RetryPendingClawbackIssue(ctx, 100); len(errs) != 0 || issued != 1 {
		t.Fatalf("reconciler after the record cleared: issued=%d errs=%v, want 1", issued, errs)
	}
	if got, _ := invStore.Get(ctx, tenantID, inv.ID); got.AmountDueCents != 6000 {
		t.Errorf("amount_due %d after the note issued, want 6000", got.AmountDueCents)
	}
}
