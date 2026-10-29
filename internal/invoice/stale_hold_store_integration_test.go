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

// The charge hold's store writes and reads (ADR-127), on real Postgres.

// A release whose caller's context has ended still frees the hold. Collect
// releases on the request's context, and the auto-charge sweep on the tick's:
// an operator who closed the tab during the credit apply or the charge's
// re-check, or a shutdown, left a hold for a charge that was never sent, and
// every charge, void and offline payment refused the invoice until the
// stale-hold sweep asked Stripe about it, an hour or more later.
func TestReleaseChargeHold_CallerContextEnded_StillReleases(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Release Ended Ctx")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REL-ENDED")
	store := invoice.NewPostgresStore(db)
	hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
	if err != nil || !hold.Held() {
		t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
	}

	ended, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.ReleaseChargeHold(ended, tenantID, inv.ID, hold); err != nil {
		t.Fatalf("release on an ended context: %v, want it to run to completion", err)
	}
	if holdIsSet(t, db, inv.ID) {
		t.Fatal("hold still set after the release: the invoice waits for the stale-hold sweep though no charge was sent")
	}
}

// The stale-hold sweep reads open invoices first. Only an open invoice's hold
// refuses anyone (charges, the hosted page, void, offline payment, dunning),
// so when a run's time budget cuts the drain, a settled invoice's leftover
// hold must not take an open one's place, however old it is.
func TestListStaleChargeHolds_OpenInvoicesFirst(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Stale Holds Open First")
	store := invoice.NewPostgresStore(db)
	open := seedClaimableInvoice(t, db, ctx, tenantID, "INV-STALE-OPEN")
	paid := seedClaimableInvoice(t, db, ctx, tenantID, "INV-STALE-PAID")
	if _, err := store.MarkPaid(ctx, tenantID, paid.ID, "pi_paid", time.Now().UTC()); err != nil {
		t.Fatalf("pay: %v", err)
	}
	setHoldAgo(t, db, paid.ID, 3*time.Hour)
	setHoldAgo(t, db, open.ID, 20*time.Minute)

	all, err := listStaleHolds(ctx, store, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("setup: %d stale holds (err %v), want both", len(all), err)
	}
	first, err := listStaleHolds(ctx, store, 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(first) != 1 || first[0].Invoice.ID != open.ID {
		t.Fatalf("first stale hold = %+v, want the open invoice %s ahead of the paid one's older hold", first, open.ID)
	}
}

// The stale-hold sweep's stamp covers a parked invoice ('unknown' with no
// PaymentIntent, REC-5). The sweep is the only one that checks a park now
// (the Search sweep that used the column as its cool-off is gone), and a
// park it could not stamp came back first on every tick, ahead of every
// other stale hold. An 'unknown' invoice that names its PaymentIntent is
// still the unknown sweep's to stamp: the column is that sweep's rotation key.
func TestMarkStaleHoldChecked_StampsAParkNotAnUnknownThatNamesItsPayment(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Stale Hold Stamp Park")
	store := invoice.NewPostgresStore(db)
	parked := seedClaimableInvoice(t, db, ctx, tenantID, "INV-STAMP-PARKED")
	named := seedClaimableInvoice(t, db, ctx, tenantID, "INV-STAMP-NAMED")
	execBypassStamp(t, db, `UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = NULL WHERE id = $1`, parked.ID)
	execBypassStamp(t, db, `UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = 'pi_named' WHERE id = $1`, named.ID)

	for _, id := range []string{parked.ID, named.ID} {
		if err := store.MarkStaleHoldChecked(ctx, tenantID, id); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}
	if got, err := store.Get(ctx, tenantID, parked.ID); err != nil || got.ProviderSyncedAt == nil {
		t.Errorf("parked invoice: provider_synced_at %v (err %v), want stamped so it rotates", got.ProviderSyncedAt, err)
	}
	if got, err := store.Get(ctx, tenantID, named.ID); err != nil || got.ProviderSyncedAt != nil {
		t.Errorf("unknown invoice with a PaymentIntent: provider_synced_at %v (err %v), want untouched (the unknown sweep's key)", got.ProviderSyncedAt, err)
	}
}

func execBypassStamp(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// A park keeps its hold until it ends (ADR-127 amendment 2026-10-29): the
// stale-hold sweep finds parks only by their hold, so a parked invoice whose
// hold is cleared refuses every charge and is never looked at again. Neither
// write that clears a hold without ending a park may clear a park's: the
// owner's release (with the exact token its claim wrote) and the sweep's
// clear (with the exact stale value and counter it read). Each answers as for
// a row that changed under it.
func TestParkedHold_NeverClearedWhileParked(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Parked Hold Kept")
	store := invoice.NewPostgresStore(db)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-PARK-KEEP")
	hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
	if err != nil || !hold.Held() {
		t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
	}
	stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentUnknown, "", "stripe: 500")

	if err := store.ReleaseChargeHold(ctx, tenantID, inv.ID, hold); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !holdIsSet(t, db, inv.ID) {
		t.Fatal("the owner's release cleared a parked invoice's hold: nothing would ever check it again")
	}

	setHoldAgo(t, db, inv.ID, 20*time.Minute)
	stale, err := listStaleHolds(ctx, store, 10)
	if err != nil || len(stale) != 1 {
		t.Fatalf("setup: %d stale holds (err %v), want the park's", len(stale), err)
	}
	h := stale[0]
	cleared, err := store.ClearStaleChargeHold(ctx, tenantID, inv.ID, h.Hold, h.Invoice.ChargeAttemptSeq, false)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared || !holdIsSet(t, db, inv.ID) {
		t.Fatalf("the sweep's clear cleared a parked invoice's hold (reported %v)", cleared)
	}

	// Negative control: once the park has ended (here, its payment found and
	// recorded), the same clear works.
	if _, err := store.MarkPaid(ctx, tenantID, inv.ID, "pi_found_later", time.Now().UTC()); err != nil {
		t.Fatalf("pay: %v", err)
	}
	paid, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err = store.ClearStaleChargeHold(ctx, tenantID, inv.ID, h.Hold, paid.ChargeAttemptSeq, false)
	if err != nil || !cleared || holdIsSet(t, db, inv.ID) {
		t.Fatalf("clear after the park ended: cleared=%v err=%v hold set=%v, want cleared", cleared, err, holdIsSet(t, db, inv.ID))
	}
}
