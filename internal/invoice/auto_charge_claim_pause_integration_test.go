package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedQueuedSubInvoice is a finalized, owed, queued invoice on a fresh
// subscription of a fresh customer: what an operator's Finalize of a cycle
// draft leaves behind.
func seedQueuedSubInvoice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, num string) (inv domain.Invoice, subID, custID string) {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + num, DisplayName: num})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "plan-" + num, Name: num, Currency: "USD", BillingInterval: domain.BillingMonthly,
		Status: domain.PlanActive, BaseAmountCents: 5000, BaseBillTiming: domain.BillInArrears,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	ps := time.Now().UTC().AddDate(0, -1, 0)
	pe := ps.AddDate(0, 1, 0)
	sub, err := subscription.NewPostgresStore(db).Create(ctx, tenantID, domain.Subscription{
		Code: "sub-" + num, DisplayName: num, CustomerID: cust.ID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &ps, Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	inv, err = store.CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, SubscriptionID: sub.ID, InvoiceNumber: num,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		BillingReason: domain.BillingReasonSubscriptionCycle, Currency: "USD",
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: ps, BillingPeriodEnd: pe,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if err := store.SetAutoChargePending(ctx, tenantID, inv.ID, true); err != nil {
		t.Fatalf("queue invoice: %v", err)
	}
	return inv, sub.ID, cust.ID
}

func bypassExec(t *testing.T, db *postgres.DB, query string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestClaimAutoCharge_RefusesWhileCollectionPaused: the auto-charge claim
// reads the subscription's collection pause, as the sweeps' lists do. The
// collect that runs right after a finalize claims without a list, so an
// operator's Finalize of a paused subscription's draft charged the card.
//
// Mutation: drop notPausedForCollection from ClaimAutoCharge, and the
// open-ended and future pauses are claimed. Read the pause at now() instead
// of the invoice's clock, and the pinned customer's lifted pause is refused.
func TestClaimAutoCharge_RefusesWhileCollectionPaused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Pause")
	store := invoice.NewPostgresStore(db)
	subs := subscription.NewPostgresStore(db)
	now := time.Now().UTC()

	t.Run("open-ended pause: refused; Charge now still claims", func(t *testing.T) {
		inv, subID, _ := seedQueuedSubInvoice(t, db, ctx, tenantID, "INV-PAUSE-OPEN")
		if _, err := subs.SetPauseCollection(ctx, tenantID, subID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if h, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || h.Held() {
			t.Fatalf("auto-charge claim on a paused subscription's invoice: held=%v err=%v, want refused", h.Held(), err)
		}
		// The operator's explicit collect is not automation: the pause does
		// not govern it.
		if h, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID); err != nil || !h.Held() {
			t.Fatalf("Charge now claim: held=%v err=%v, want held", h.Held(), err)
		}
	})

	t.Run("resumed: claimed", func(t *testing.T) {
		inv, subID, _ := seedQueuedSubInvoice(t, db, ctx, tenantID, "INV-PAUSE-RESUMED")
		if _, err := subs.SetPauseCollection(ctx, tenantID, subID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
			t.Fatalf("pause: %v", err)
		}
		if _, err := subs.ClearPauseCollection(ctx, tenantID, subID, nil); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if h, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || !h.Held() {
			t.Fatalf("claim after resume: held=%v err=%v, want held", h.Held(), err)
		}
	})

	t.Run("timed pause: holds before resumes_at, not after", func(t *testing.T) {
		future, subF, _ := seedQueuedSubInvoice(t, db, ctx, tenantID, "INV-PAUSE-FUTURE")
		bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft', pause_collection_resumes_at = $1 WHERE id = $2`,
			now.Add(24*time.Hour), subF)
		if h, err := store.ClaimAutoCharge(ctx, tenantID, future.ID); err != nil || h.Held() {
			t.Fatalf("claim under a pause that resumes tomorrow: held=%v err=%v, want refused", h.Held(), err)
		}
		past, subP, _ := seedQueuedSubInvoice(t, db, ctx, tenantID, "INV-PAUSE-PAST")
		bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft', pause_collection_resumes_at = $1 WHERE id = $2`,
			now.Add(-time.Hour), subP)
		if h, err := store.ClaimAutoCharge(ctx, tenantID, past.ID); err != nil || !h.Held() {
			t.Fatalf("claim after the pause's resumes_at: held=%v err=%v, want held", h.Held(), err)
		}
	})

	// A pinned customer's pause is read at its test clock's frozen time, as
	// ListAutoChargePendingForClock reads it: the clock is 20 days ahead of
	// the wall, and the pause resumes 10 days from now.
	t.Run("test clock: read at the clock's frozen time", func(t *testing.T) {
		resumesAt := now.Add(10 * 24 * time.Hour)
		for _, tc := range []struct {
			name   string
			frozen time.Time
			held   bool
		}{
			{"clock past resumes_at", now.Add(20 * 24 * time.Hour), true},
			{"clock before resumes_at", now.Add(5 * 24 * time.Hour), false},
		} {
			inv, subID, custID := seedQueuedSubInvoice(t, db, ctx, tenantID, "INV-PAUSE-CLK-"+map[bool]string{true: "A", false: "B"}[tc.held])
			clockID := postgres.NewID("hc_tclk")
			bypassExec(t, db, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode) VALUES ($1, $2, 'claim-pause', $3, 'ready', false)`,
				clockID, tenantID, tc.frozen)
			bypassExec(t, db, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, clockID, custID)
			bypassExec(t, db, `UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft', pause_collection_resumes_at = $1 WHERE id = $2`,
				resumesAt, subID)
			h, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
			if err != nil || h.Held() != tc.held {
				t.Errorf("%s: held=%v err=%v, want held=%v", tc.name, h.Held(), err, tc.held)
			}
		}
	})
}
