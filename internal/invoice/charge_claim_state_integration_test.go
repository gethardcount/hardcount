package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// ChargeClaimStateOf is the auto-charge claim's own rule, read rather than
// taken. For one invoice per reason the claim refuses, and one it takes, the
// read gives that reason, and the claim is taken exactly when the read says
// claimable. A condition added to the claim and not to the read fails here.
func TestChargeClaimStateOf_AgreesWithTheClaim(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim State")
	store := invoice.NewPostgresStore(db)
	now := time.Now().UTC()

	for i, c := range []struct {
		name  string
		setup func(invID, subID string)
		want  domain.ChargeClaimState
	}{
		{"queued, nothing in the way", func(_, _ string) {}, domain.ChargeClaimable},
		{"a draft", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET status = 'draft' WHERE id = $1`, id)
		}, domain.ChargeNothingToCharge},
		{"paid", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET status = 'paid', payment_status = 'succeeded', paid_at = now(), amount_due_cents = 0, amount_paid_cents = total_amount_cents WHERE id = $1`, id)
		}, domain.ChargeNothingToCharge},
		{"failed, out of the queue", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET payment_status = 'failed', auto_charge_pending = false WHERE id = $1`, id)
		}, domain.ChargeNothingToCharge},
		{"not queued", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET auto_charge_pending = false WHERE id = $1`, id)
		}, domain.ChargeNothingToCharge},
		{"payment unknown", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = 'pi_state_unknown' WHERE id = $1`, id)
		}, domain.ChargeHeldByAttempt},
		{"payment processing", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET payment_status = 'processing', stripe_payment_intent_id = 'pi_state_processing' WHERE id = $1`, id)
		}, domain.ChargeHeldByAttempt},
		{"a live hold", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET auto_charge_claimed_until = now() + interval '5 minutes' WHERE id = $1`, id)
		}, domain.ChargeHeldByAttempt},
		{"a stale hold", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 hour' WHERE id = $1`, id)
		}, domain.ChargeHeldByAttempt},
		{"the customer on the hosted page", func(id, _ string) {
			bypassExec(t, db, `INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, stripe_session_id, amount_cents, currency, status, expires_at)
				VALUES ($1, $2, $3, false, 'cs_' || $1, 5000, 'USD', 'open', now() + interval '50 minutes')`, postgres.NewID("hc_cks"), tenantID, id)
		}, domain.ChargePayingOnHostedPage},
		{"collection paused", func(_, subID string) {
			bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft' WHERE id = $1`, subID)
		}, domain.ChargeCollectionPaused},
		{"a timed pause, before it resumes", func(_, subID string) {
			bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft', pause_collection_resumes_at = $1 WHERE id = $2`, now.Add(10*24*time.Hour), subID)
		}, domain.ChargeCollectionPaused},
		// Two conditions fail at once: the list's order picks the answer.
		{"held, with a live hosted page", func(id, _ string) {
			bypassExec(t, db, `UPDATE invoices SET auto_charge_claimed_until = now() + interval '5 minutes' WHERE id = $1`, id)
			bypassExec(t, db, `INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, stripe_session_id, amount_cents, currency, status, expires_at)
				VALUES ($1, $2, $3, false, 'cs_' || $1, 5000, 'USD', 'open', now() + interval '50 minutes')`, postgres.NewID("hc_cks"), tenantID, id)
		}, domain.ChargeHeldByAttempt},
		{"held, and collection paused", func(id, subID string) {
			bypassExec(t, db, `UPDATE invoices SET auto_charge_claimed_until = now() + interval '5 minutes' WHERE id = $1`, id)
			bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft' WHERE id = $1`, subID)
		}, domain.ChargeHeldByAttempt},
		{"a live hosted page, and collection paused", func(id, subID string) {
			bypassExec(t, db, `INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, stripe_session_id, amount_cents, currency, status, expires_at)
				VALUES ($1, $2, $3, false, 'cs_' || $1, 5000, 'USD', 'open', now() + interval '50 minutes')`, postgres.NewID("hc_cks"), tenantID, id)
			bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft' WHERE id = $1`, subID)
		}, domain.ChargePayingOnHostedPage},
		{"a timed pause, after it resumed", func(_, subID string) {
			bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft', pause_collection_resumes_at = $1 WHERE id = $2`, now.Add(-time.Hour), subID)
		}, domain.ChargeClaimable},
	} {
		t.Run(c.name, func(t *testing.T) {
			inv, subID := seedQueuedSubInvoice(t, db, ctx, tenantID, "INV-STATE-"+string(rune('A'+i)))
			c.setup(inv.ID, subID)
			state, err := store.ChargeClaimStateOf(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if state != c.want {
				t.Errorf("read %d, want %d", state, c.want)
			}
			hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if hold.Held() != (state == domain.ChargeClaimable) {
				t.Errorf("the claim was taken = %v, but the read said %d", hold.Held(), state)
			}
		})
	}
	if state, err := store.ChargeClaimStateOf(ctx, tenantID, "hc_inv_missing"); err != nil || state != domain.ChargeNothingToCharge {
		t.Errorf("a missing invoice: %d (err %v), want nothing to charge", state, err)
	}
}
