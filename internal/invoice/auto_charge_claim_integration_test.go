package invoice_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// seedClaimableInvoice creates an invoice matching the auto-charge sweep
// predicates exactly (finalized, payment pending, flag set, owing).
func seedClaimableInvoice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, num string) domain.Invoice {
	t.Helper()
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + num, DisplayName: "Claim " + num,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         cust.ID,
		InvoiceNumber:      num,
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      5000,
		TotalAmountCents:   5000,
		AmountDueCents:     5000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if err := store.SetAutoChargePending(ctx, tenantID, inv.ID, true); err != nil {
		t.Fatalf("set auto_charge_pending: %v", err)
	}
	return inv
}

// TestClaimAutoCharge_CollisionExactlyOne is the real-Postgres CAS pin
// (HA hazard #1): N racing claimers on one invoice — exactly one wins
// the lease window. Remove the claim's WHERE lease predicate and this
// fails (mutation-verify).
func TestClaimAutoCharge_CollisionExactlyOne(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Collision")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-RACE")
	store := invoice.NewPostgresStore(db)

	const racers = 8
	var wg sync.WaitGroup
	wins := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			wins <- h.Held()
		}()
	}
	wg.Wait()
	close(wins)
	won := 0
	for ok := range wins {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("claim winners: got %d, want exactly 1 — the charge leg must admit one leader per lease window", won)
	}
}

// TestClaimAutoCharge_LeaseExpiryAndRelease: a held hold refuses re-claims;
// only its owner's release (by token) re-admits; an expired hold refuses too
// (ADR-127).
func TestClaimAutoCharge_LeaseExpiryAndRelease(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Lease")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-LEASE")
	store := invoice.NewPostgresStore(db)

	first, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
	if !first.Held() {
		t.Fatal("first claim must succeed")
	}
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("second claim inside the lease must fail")
	}

	// A release with another owner's token clears nothing: a late owner
	// must never erase the hold of whoever holds the invoice now.
	if err := store.ReleaseChargeHold(ctx, tenantID, inv.ID, domain.ChargeHold{Until: first.Until.Add(-time.Second)}); err != nil {
		t.Fatalf("foreign release: %v", err)
	}
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("a release with another owner's token cleared the hold")
	}

	// Release: the owner's clear (provably-pre-Stripe skip path) re-admits
	// immediately, no wait.
	if err := store.ReleaseChargeHold(ctx, tenantID, inv.ID, first); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !h.Held() {
		t.Fatal("claim after explicit release must succeed")
	}

	// Expiry: push the hold into the past (a crashed charger, or one whose
	// caller's deadline ended). Its charge may have reached Stripe and its
	// result was not saved, so the hold is NOT free (ADR-127, #976): every
	// claim refuses it until the stale-hold sweep has asked Stripe.
	expireLease(t, db, inv.ID)
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("claim on an expired hold must be refused: its charge may have taken the money (#976)")
	}
	if h, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("dunning claim on an expired hold must be refused (#976)")
	}
	if h, _ := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("collect claim on an expired hold must be refused (#976)")
	}
}

func expireLease(t *testing.T, db *postgres.DB, invoiceID string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(
		`UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 second' WHERE id = $1`,
		invoiceID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestClaimAutoCharge_PredicateRecheck: the CAS re-asserts the full
// sweep eligibility, so any state movement between list and claim —
// a webhook settle, a rival leader's failed/unknown outcome, a cleared
// flag, a voided invoice — fails the claim (mutation-verify style: every
// predicate flipped once).
//
// A zeroed amount_due is deliberately NOT a refusal: the collector claims a
// $0 invoice to settle it paid with no charge (it re-reads the row after the
// claim, so a stale snapshot amount is never charged).
func TestClaimAutoCharge_PredicateRecheck(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Predicates")
	store := invoice.NewPostgresStore(db)

	mutations := []struct {
		name string
		sql  string
	}{
		{"webhook settled (status=paid, payment=succeeded)", `UPDATE invoices SET status='paid', payment_status='succeeded' WHERE id=$1`},
		{"rival outcome unknown", `UPDATE invoices SET payment_status='unknown' WHERE id=$1`},
		{"rival outcome failed", `UPDATE invoices SET payment_status='failed' WHERE id=$1`},
		{"flag cleared", `UPDATE invoices SET auto_charge_pending=FALSE WHERE id=$1`},
		{"voided", `UPDATE invoices SET status='voided' WHERE id=$1`},
	}
	for i, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-PRED-"+string(rune('A'+i)))
			tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.Exec(m.sql, inv.ID); err != nil {
				_ = tx.Rollback()
				t.Fatalf("mutate: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); h.Held() {
				t.Fatalf("claim must fail after %q — charging stale state is the double-charge path", m.name)
			}
		})
	}
}

// TestClaimAutoCharge_ZeroDueIsClaimable: an invoice brought to $0 (a credit
// note, or credits) is claimed so the collector can settle it. Pre-fix the
// claim and both sweep lists required amount_due > 0, so such an invoice sat
// finalized and unpaid forever.
func TestClaimAutoCharge_ZeroDueIsClaimable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Zero Due")
	store := invoice.NewPostgresStore(db)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-ZERO")
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`UPDATE invoices SET amount_due_cents=0 WHERE id=$1`, inv.ID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("zero amount_due: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	listed, err := store.ListAutoChargePending(ctx, domain.InvoiceKeyset{}, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, l := range listed {
		found = found || l.ID == inv.ID
	}
	if !found {
		t.Error("a queued $0 invoice must be listed by the sweep so it can be settled")
	}
	if h, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || !h.Held() {
		t.Fatalf("claim = %v, %v; want held — the collector settles a $0 invoice", h.Held(), err)
	}
}

// TestClaimAutoCharge_UpdatedAtStable pins that claim and release must NOT
// touch updated_at.
//
// The original reason — "the Stripe idempotency key derives from it" — expired
// with #678: the key is now seeded from charge_attempt_seq, which the lease
// cannot reach, and TestChargeKey_StableUntilAnOutcomeIsRecorded owns that
// property. What remains is still worth pinning: the sweep lists order by
// `updated_at ASC`, so a lease that bumped it would send an invoice to the back
// of the queue on every claim — an invoice that fails to complete its charge
// could be starved indefinitely behind newer arrivals.
func TestClaimAutoCharge_UpdatedAtStable(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim UpdatedAt")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-UAT")
	store := invoice.NewPostgresStore(db)

	before, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	held, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
	if !held.Held() {
		t.Fatal("claim must succeed")
	}
	if err := store.ReleaseChargeHold(ctx, tenantID, inv.ID, held); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !h.Held() {
		t.Fatal("re-claim must succeed")
	}
	after, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at moved across claim/release/re-claim (%v → %v) — this DIVERGES the Stripe idempotency key and re-opens the dual-leader double charge", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestClaimAutoCharge_ListsStayClaimBlind: a held claim must NOT hide the
// invoice from the sweep's list — only the claim consults the lease, so the
// list stays a plain read of the queue and the CAS is the single gate.
// (Originally the enrollment-starvation regression, adversarial-review flaw 2,
// when a no-payment dunning sweep shared the list; that sweep is gone,
// ADR-116.)
func TestClaimAutoCharge_ListsStayClaimBlind(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim Blind Lists")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-CLAIM-BLIND")
	store := invoice.NewPostgresStore(db)

	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); !h.Held() {
		t.Fatal("claim must succeed")
	}
	listed, err := store.ListAutoChargePending(ctx, domain.InvoiceKeyset{}, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, li := range listed {
		if li.ID == inv.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a claimed invoice vanished from ListAutoChargePending — dunning enrollment shares this list and would be starved (card-less invoices never enter dunning)")
	}
}

// TestClaimChargeForDunningRetry_CrossPathExclusion (HA hazard #11):
// the sweep claim and the dunning-retry claim share ONE lease column —
// whichever path claims first, the other is refused. Their Stripe
// idempotency keys differ by construction (purpose suffix), so this
// mutual exclusion is the only double-charge guard between them.
func TestClaimChargeForDunningRetry_CrossPathExclusion(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim CrossPath")
	store := invoice.NewPostgresStore(db)

	// Sweep first, dunning refused.
	a := seedClaimableInvoice(t, db, ctx, tenantID, "INV-XPATH-A")
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, a.ID); !h.Held() {
		t.Fatal("sweep claim must succeed")
	}
	if h, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, a.ID); h.Held() {
		t.Fatal("dunning claim must be refused while the sweep holds the lease — concurrent charges have divergent keys Stripe cannot dedupe")
	}

	// Dunning first, sweep refused.
	b := seedClaimableInvoice(t, db, ctx, tenantID, "INV-XPATH-B")
	if h, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, b.ID); !h.Held() {
		t.Fatal("dunning claim must succeed")
	}
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, b.ID); h.Held() {
		t.Fatal("sweep claim must be refused while dunning holds the lease")
	}
}

// TestClaimChargeForDunningRetry_StatusMatrix pins the deliberate
// predicate asymmetry between the two claims:
//   - 'pending'  → both paths may claim (card-less enrolled + card attached);
//   - 'failed'   → dunning only (the normal retry state; the sweep's list
//     never returns failed invoices);
//   - 'unknown'  → NEITHER — an ambiguous outcome may be a real payment
//     and must wait for the reconciler. This is the same-tick
//     N=1 window: billing half's unknown outcome followed
//     seconds later by the dunning half's due retry.
func TestClaimChargeForDunningRetry_StatusMatrix(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Claim StatusMatrix")
	store := invoice.NewPostgresStore(db)

	set := func(t *testing.T, id, status string) {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(`UPDATE invoices SET payment_status=$1, auto_charge_claimed_until=NULL WHERE id=$2`, status, id); err != nil {
			_ = tx.Rollback()
			t.Fatalf("set status: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-STATUS-MATRIX")

	// pending: dunning claimable.
	if h, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); !h.Held() {
		t.Fatal("dunning claim on 'pending' must succeed (card-less enrolled, card attached)")
	}

	// failed: dunning claimable, sweep NOT.
	set(t, inv.ID, "failed")
	if h, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); !h.Held() {
		t.Fatal("dunning claim on 'failed' must succeed (the normal retry state)")
	}
	set(t, inv.ID, "failed")
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("sweep claim on 'failed' must be refused — dunning owns failed invoices")
	}

	// unknown: NEITHER path may charge — reconciler territory.
	set(t, inv.ID, "unknown")
	if h, _ := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("dunning claim on 'unknown' must be refused — the ambiguous PI may be a real payment; blind re-charge is the same-tick double-charge window")
	}
	if h, _ := store.ClaimAutoCharge(ctx, tenantID, inv.ID); h.Held() {
		t.Fatal("sweep claim on 'unknown' must be refused")
	}
}

// TestClaimChargeForManualCollect_SharedLeaseAndPredicate pins the
// operator-collect claim added by the 2026-07-21 snapshot-race audit:
// manual collect was the one charge initiator outside the lease ring —
// it charged a handler snapshot with no mutual exclusion against the
// sweep/dunning and no post-credit reload. The claim shares the
// auto_charge_claimed_until lease, so holding it excludes every other
// initiator; 'unknown' is rejected (reconciler-owned); release frees it.
func TestClaimChargeForManualCollect_SharedLeaseAndPredicate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Manual Collect Claim")
	store := invoice.NewPostgresStore(db)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "MCC-1")

	// Sweep holds the lease → manual collect must lose.
	claimed, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
	if err != nil || !claimed.Held() {
		t.Fatalf("sweep claim: claimed=%v err=%v", claimed.Held(), err)
	}
	manual, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("manual claim: %v", err)
	}
	if manual.Held() {
		t.Fatal("manual collect must NOT claim while the sweep holds the lease")
	}

	// Released → manual collect wins; a second manual claim then loses.
	if err := store.ReleaseChargeHold(ctx, tenantID, inv.ID, claimed); err != nil {
		t.Fatalf("release: %v", err)
	}
	manual, err = store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
	if err != nil || !manual.Held() {
		t.Fatalf("manual claim after release: claimed=%v err=%v", manual.Held(), err)
	}
	second, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("second manual claim: %v", err)
	}
	if second.Held() {
		t.Fatal("second manual claim must lose while the first holds the lease")
	}

	// 'unknown' payment status is never claimable — the reconciler owns it.
	unknownInv := seedClaimableInvoice(t, db, ctx, tenantID, "MCC-2")
	stampOutcome(t, store, ctx, tenantID, unknownInv.ID, domain.PaymentUnknown, "pi_amb", "ambiguous outcome")
	claimed, err = store.ClaimChargeForManualCollect(ctx, tenantID, unknownInv.ID)
	if err != nil {
		t.Fatalf("claim unknown: %v", err)
	}
	if claimed.Held() {
		t.Fatal("an invoice with payment_status='unknown' must never be claimable for manual collect")
	}
}
