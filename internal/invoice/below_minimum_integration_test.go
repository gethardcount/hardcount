package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// StampBelowMinimum on real Postgres: the refusal lands once, in one write
// (pending, out of the queue, stamped, the seq moved), and under the seq CAS
// of every charge outcome (P13): an outcome recorded during the call wins, a
// replay of the same attempt is refused, and a voided invoice is not moved.
func TestStampBelowMinimum(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Below Minimum Stamp")
	store := invoice.NewPostgresStore(db)
	const msg = "Amount must be at least $0.50 usd"

	t.Run("a queued invoice, then a replay of the same attempt", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-MIN-A")
		got, applied, err := store.StampBelowMinimum(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, msg)
		if err != nil || !applied {
			t.Fatalf("stamp: applied=%v err=%v", applied, err)
		}
		after, err := store.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range []domain.Invoice{got, after} {
			if row.PaymentStatus != domain.PaymentPending || row.AutoChargePending || row.BelowMinimumAt == nil ||
				row.LastPaymentError != msg || row.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 || row.StripePaymentIntentID != "" {
				t.Errorf("row %s queued=%v below_minimum_at=%v error=%q seq=%d pi=%q, want pending, out of the queue, stamped, the sentence, seq %d, no PaymentIntent",
					row.PaymentStatus, row.AutoChargePending, row.BelowMinimumAt, row.LastPaymentError, row.ChargeAttemptSeq, row.StripePaymentIntentID, inv.ChargeAttemptSeq+1)
			}
		}
		// The sweep's claim no longer takes it.
		if hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || hold.Held() {
			t.Errorf("auto-charge claim after the stamp: held=%v err=%v, want refused", hold.Held(), err)
		}
		// A replay of the same attempt (its seq) is refused and moves nothing.
		replay, applied, err := store.StampBelowMinimum(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, msg)
		if err != nil || applied || replay.ChargeAttemptSeq != after.ChargeAttemptSeq {
			t.Errorf("replay: applied=%v seq=%d err=%v, want refused at seq %d", applied, replay.ChargeAttemptSeq, err, after.ChargeAttemptSeq)
		}
	})

	t.Run("an earlier decline's failed state turns pending", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-MIN-B")
		stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentFailed, "pi_declined_b", "Your card was declined.")
		cur, _ := store.Get(ctx, tenantID, inv.ID)
		got, applied, err := store.StampBelowMinimum(ctx, tenantID, inv.ID, cur.ChargeAttemptSeq, msg)
		if err != nil || !applied || got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" {
			t.Errorf("stamp over a decline: applied=%v status=%s pi=%q err=%v, want pending with no PaymentIntent", applied, got.PaymentStatus, got.StripePaymentIntentID, err)
		}
	})

	t.Run("an outcome recorded during the call wins", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-MIN-C")
		stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentProcessing, "pi_other_c", "")
		got, applied, err := store.StampBelowMinimum(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, msg)
		if err != nil || applied || got.PaymentStatus != domain.PaymentProcessing || got.BelowMinimumAt != nil {
			t.Errorf("stamp after another outcome: applied=%v status=%s below_minimum_at=%v err=%v, want refused, processing kept", applied, got.PaymentStatus, got.BelowMinimumAt, err)
		}
	})

	t.Run("a voided invoice is not moved", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-MIN-D")
		if _, err := store.UpdateStatus(ctx, tenantID, inv.ID, domain.InvoiceVoided); err != nil {
			t.Fatal(err)
		}
		got, applied, err := store.StampBelowMinimum(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, msg)
		if err != nil || applied || got.BelowMinimumAt != nil {
			t.Errorf("stamp on a voided invoice: applied=%v below_minimum_at=%v err=%v, want refused", applied, got.BelowMinimumAt, err)
		}
	})
}
