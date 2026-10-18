package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// #976 (ADR-127): a crash or a timeout on dunning's final retry leaves the
// charge record on the invoice, with the payment status still failed and the
// run at its last retry. Dunning must not cancel, pause or write off while
// that record exists: the reconciler asks Stripe about the charge first.

// chargeRecord writes the record the charge path writes before it calls
// Stripe, at the invoice's current attempt counter: what a crash after that
// write leaves on the invoice.
func chargeRecord(t *testing.T, ctx context.Context, f *exhaustFixture) {
	t.Helper()
	inv, err := f.invoices.Get(ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	tx, err := f.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE invoices SET charge_started_at = now() - interval '16 minutes', charge_started_seq = $2,
		charge_started_key = $3, charge_started_stripe_customer = 'cus_stripe_inflight' WHERE id = $1`,
		f.invoiceID, inv.ChargeAttemptSeq, recordedKey(f.invoiceID, inv.ChargeAttemptSeq)); err != nil {
		t.Fatalf("write charge record: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func recordedKey(invoiceID string, seq int64) string {
	return payment.ChargeIdempotencyKey(invoiceID, seq, "dunning_retry", "pm_card")
}

// recordedChargeStripe is Stripe holding the final retry's payment: it
// succeeded, and it carries the recorded key.
type recordedChargeStripe struct {
	key    string
	amount int64
}

func (s recordedChargeStripe) ListPaymentIntentsForCustomer(context.Context, string, time.Time) ([]payment.PaymentIntentResult, error) {
	return []payment.PaymentIntentResult{{ID: "pi_final_retry", Status: "succeeded", AttemptKey: s.key,
		Purpose: "dunning_retry", AmountReceivedCents: s.amount}}, nil
}

func (recordedChargeStripe) CreatePaymentIntent(context.Context, payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{}, nil
}
func (recordedChargeStripe) CancelPaymentIntent(context.Context, string) error { return nil }
func (recordedChargeStripe) GetPaymentIntent(context.Context, string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{}, nil
}
func (recordedChargeStripe) SearchPaymentIntentsByInvoiceID(context.Context, string) ([]payment.PaymentIntentResult, error) {
	return nil, nil
}

// The crash on the final retry: the run had recorded its last attempt and its
// 15-minute exhaustion lease, the charge recorded itself and reached Stripe,
// and the process died. The lease ends before the reconciler's check runs.
// Dunning holds the final action; the check finds the payment; the run closes
// as recovered and the subscription is untouched.
func TestExhaust_CrashOnFinalRetry_RecordHoldsUntilTheCheckFindsThePayment(t *testing.T) {
	db := testutil.SetupTestDB(t)
	acts := policyActions[0] // cancel and write off: the irreversible one
	f := newExhaustFixture(t, db, "Crash On Final Retry", acts, 2, nil, noRetry{t: t})
	chargeRecord(t, f.ctx, f)

	// The lease has ended and the reconciler has not run yet.
	f.tick(t)
	f.assertNothingFired(t)
	f.clk.Advance(time.Hour)
	f.tick(t)
	f.assertNothingFired(t)

	// The reconciler's check, wired as router.go wires it.
	inv, err := f.invoices.Get(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	stripe := recordedChargeStripe{key: recordedKey(f.invoiceID, inv.ChargeAttemptSeq), amount: inv.AmountDueCents}
	settler := payment.NewStripe(stripe, f.invoices, payment.NewPostgresWebhookStore(f.db), nil)
	settler.SetDunningResolver(f.svc)
	rec := payment.NewReconciler(stripe, f.invoices, time.Minute)
	rec.SetSettler(settler)
	if _, errs := rec.Run(postgres.WithLivemode(context.Background(), false), 50); len(errs) != 0 {
		t.Fatalf("reconciler: %v", errs)
	}

	got, err := f.invoices.Get(f.ctx, f.tenantID, f.invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != "pi_final_retry" || got.ChargeStartedAt != nil {
		t.Fatalf("after the check: %s with PI %q, record %v; want paid by pi_final_retry, no record", got.Status, got.StripePaymentIntentID, got.ChargeStartedAt)
	}
	f.tick(t)
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("run state=%s resolution=%q, want resolved / payment_recovered", run.State, run.Resolution)
	}
	sub, err := f.subs.Get(f.ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != domain.SubscriptionActive || sub.PauseCollection != nil {
		t.Errorf("subscription is %s (paused=%v), want active: the customer paid", sub.Status, sub.PauseCollection != nil)
	}
}
