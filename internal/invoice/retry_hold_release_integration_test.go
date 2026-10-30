package invoice_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestReleaseRetryChargeHoldTx pins the release of a processing dunning
// retry's charge hold when its payment fails (#1289), on the real failure
// settle's transaction. It frees only the failed PaymentIntent's own hold, and
// only one the stale-hold sweep cannot have looked at yet: the sweep keeps a
// hold older than domain.StaleChargeHoldWait while a payment the invoice does
// not follow is still processing at Stripe, and this must not undo that.
func TestReleaseRetryChargeHoldTx(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Retry Hold Release")
	store := invoice.NewPostgresStore(db)

	setHold := func(t *testing.T, id, interval string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET auto_charge_claimed_until = now() + $2::interval WHERE id = $1 AND tenant_id = $3`, id, interval, tenantID); err != nil {
			t.Fatalf("set hold: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit hold: %v", err)
		}
	}
	held := func(t *testing.T, id string) bool {
		t.Helper()
		state, err := store.ChargeHoldStateOf(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("read hold: %v", err)
		}
		return state != domain.ChargeHoldNone
	}

	cases := []struct {
		name     string
		hold     string // the hold's end relative to now
		askPI    string
		released bool
	}{
		{"the retry's live hold", "5 minutes", "pi_slow", true},
		{"ended 5 minutes ago, before the sweep can look", "-5 minutes", "pi_slow", true},
		{"ended 11 minutes ago, the sweep's to decide", "-11 minutes", "pi_slow", false},
		{"another PaymentIntent", "5 minutes", "pi_other", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, fmt.Sprintf("INV-RHR-%d", i))
			hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
			if err != nil || !hold.Held() {
				t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
			}
			if _, applied, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentProcessing, "pi_slow", ""); err != nil || !applied {
				t.Fatalf("stamp processing: applied=%v err=%v", applied, err)
			}
			setHold(t, inv.ID, tc.hold)

			var released bool
			var rerr error
			_, res, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID,
				domain.PaymentFailureReport{PaymentIntentID: "pi_slow", Message: "payment failed", DeclineCode: "insufficient_funds"},
				func(tx *sql.Tx, _ domain.Invoice) error {
					released, rerr = store.ReleaseRetryChargeHoldTx(ctx, tx, inv.ID, tc.askPI)
					return rerr
				})
			if err != nil || !res.Recorded {
				t.Fatalf("failure settle: recorded=%v err=%v", res.Recorded, err)
			}
			if rerr != nil {
				t.Fatalf("release: %v", rerr)
			}
			if released != tc.released || held(t, inv.ID) == tc.released {
				t.Errorf("released=%v held=%v, want released=%v", released, held(t, inv.ID), tc.released)
			}
		})
	}

	t.Run("a payment still processing keeps its hold", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-RHR-proc")
		if hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || !hold.Held() {
			t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
		}
		if _, applied, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentProcessing, "pi_slow", ""); err != nil || !applied {
			t.Fatalf("stamp processing: applied=%v err=%v", applied, err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		released, err := store.ReleaseRetryChargeHoldTx(ctx, tx, inv.ID, "pi_slow")
		if err != nil || released {
			t.Fatalf("released=%v err=%v, want the hold kept while the payment is processing", released, err)
		}
	})
}
