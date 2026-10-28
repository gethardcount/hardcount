package payment_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedPayable inserts a customer + a finalized, payable invoice (the
// precondition both audited emission sites share: the token flow needs an
// invoice to point at, ClaimOpen re-checks payability under FOR SHARE).
// Same shape as token_consume_integration_test.go / checkout_claim_integration_test.go.
func seedPayable(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string) (custID, invID string) {
	t.Helper()
	suffix := postgres.NewID("s")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + suffix, DisplayName: "InTx Audit",
	})
	if err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	now := time.Now().UTC()
	issued := now
	inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: "INV-" + suffix[len(suffix)-8:],
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 10000, TotalAmountCents: 10000, AmountDueCents: 10000,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return cust.ID, inv.ID
}

// auditRowsFor returns the audit rows recorded against one resource whose
// metadata `action` key matches — the emission sites here all use the frozen
// wire action "update" and distinguish the business event in metadata.
func auditRowsFor(t *testing.T, ctx context.Context, logger *audit.Logger, tenantID, resourceType, resourceID, metaAction string) []domain.AuditEntry {
	t.Helper()
	rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{
		ResourceType: resourceType, ResourceID: resourceID,
	})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	var out []domain.AuditEntry
	for _, r := range rows {
		if r.Metadata["action"] == metaAction {
			out = append(out, r)
		}
	}
	return out
}

// TestTokenAudit_ConsumeRestoreSharedFate pins the ADR-090 emission hooks on
// the payment-update token CAS (TokenService.ConsumeAudited / RestoreAudited),
// which shipped with no fault injection. The invariants:
//
//   - the CAS WINNER emits exactly once, and the emission commits with the burn;
//   - the CAS LOSER (a replay / the second click) emits NOTHING — a token that
//     was not burned by this call must not leave evidence claiming it was;
//   - an emit failure ABORTS the burn (shared fate): the customer's emailed link
//     survives an audit-write bug rather than dying silently unrecorded;
//   - Restore emits only when a row was ACTUALLY revived — a no-op restore
//     (never consumed, or consumed outside the 1-minute recency window, i.e. by a
//     SUCCESSFUL create) must not fabricate a revival row.
//
// Mutation-verify: drop the `n == 1` guard on either hook (emit unconditionally)
// and the loser/no-op subtests fail; drop the emit-error propagation and the
// rollback subtests fail.
func TestTokenAudit_ConsumeRestoreSharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Token InTx Audit")

	tokens := payment.NewTokenService(db)
	logger := audit.NewLogger(db)

	// emitFor builds the emission the public handler passes (customer resource,
	// business event in metadata) and counts its invocations.
	emitFor := func(custID, invID, metaAction string, calls *int) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			*calls++
			return logger.LogInTx(ctx, tx, audit.Entry{
				Action:       domain.AuditActionUpdate,
				ResourceType: "customer",
				ResourceID:   custID,
				Metadata:     map[string]any{"action": metaAction, "invoice_id": invID},
			})
		}
	}
	// writeThenFail lands the row and THEN errors: proves the ROW rolls back with
	// the business write, not merely that a never-attempted write left nothing.
	writeThenFail := func(custID, invID, metaAction string, calls *int) func(*sql.Tx) error {
		inner := emitFor(custID, invID, metaAction, calls)
		return func(tx *sql.Tx) error {
			if err := inner(tx); err != nil {
				return err
			}
			return errors.New("injected audit failure")
		}
	}

	t.Run("CAS winner emits once and commits the burn", func(t *testing.T) {
		custID, invID := seedPayable(t, db, ctx, tenantID)
		raw, err := tokens.Create(ctx, tenantID, custID, invID)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}

		calls := 0
		consumed, err := tokens.ConsumeAudited(ctx, tenantID, raw, emitFor(custID, invID, "payment_update_checkout_started", &calls))
		if err != nil || !consumed {
			t.Fatalf("consume: consumed=%v err=%v, want true/nil", consumed, err)
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want exactly 1", calls)
		}
		rows := auditRowsFor(t, ctx, logger, tenantID, "customer", custID, "payment_update_checkout_started")
		if len(rows) != 1 {
			t.Fatalf("want exactly one checkout-started audit row; got %d: %+v", len(rows), rows)
		}
		if rows[0].Metadata["invoice_id"] != invID {
			t.Errorf("audit metadata invoice_id: got %v, want %s", rows[0].Metadata["invoice_id"], invID)
		}
		// The burn itself committed with it.
		if _, err := tokens.Validate(ctx, raw); err == nil {
			t.Error("token still validates after a committed consume — the burn did not persist")
		}
	})

	t.Run("second consume of a burned token emits nothing", func(t *testing.T) {
		custID, invID := seedPayable(t, db, ctx, tenantID)
		raw, err := tokens.Create(ctx, tenantID, custID, invID)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}
		firstCalls := 0
		if consumed, err := tokens.ConsumeAudited(ctx, tenantID, raw, emitFor(custID, invID, "payment_update_checkout_started", &firstCalls)); err != nil || !consumed {
			t.Fatalf("first consume: consumed=%v err=%v", consumed, err)
		}

		// The replay / second click: loses the CAS, records nothing.
		replayCalls := 0
		consumed, err := tokens.ConsumeAudited(ctx, tenantID, raw, emitFor(custID, invID, "payment_update_checkout_started", &replayCalls))
		if err != nil {
			t.Fatalf("second consume: %v", err)
		}
		if consumed {
			t.Fatal("second consume won the CAS; single-use must be exactly-once")
		}
		if replayCalls != 0 {
			t.Errorf("loser fired emit %d time(s); a call that burned nothing must record nothing", replayCalls)
		}
		if rows := auditRowsFor(t, ctx, logger, tenantID, "customer", custID, "payment_update_checkout_started"); len(rows) != 1 {
			t.Errorf("audit rows after replay = %d, want 1 (the winner's only)", len(rows))
		}
	})

	t.Run("emit failure rolls the consume back and the token survives", func(t *testing.T) {
		custID, invID := seedPayable(t, db, ctx, tenantID)
		raw, err := tokens.Create(ctx, tenantID, custID, invID)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}

		calls := 0
		consumed, err := tokens.ConsumeAudited(ctx, tenantID, raw, writeThenFail(custID, invID, "payment_update_checkout_started", &calls))
		if err == nil {
			t.Fatal("consume must fail when its audit emission fails (shared fate)")
		}
		if consumed {
			t.Error("consumed=true on a rolled-back consume")
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want 1", calls)
		}
		if rows := auditRowsFor(t, ctx, logger, tenantID, "customer", custID, "payment_update_checkout_started"); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back consume: %+v", rows)
		}
		// The burn rolled back with it — the link still works, and a later
		// consume still WINS the CAS.
		if _, err := tokens.Validate(ctx, raw); err != nil {
			t.Fatalf("validate after rolled-back consume: %v (the failed audit killed the customer's link)", err)
		}
		ok, err := tokens.Consume(ctx, tenantID, raw)
		if err != nil || !ok {
			t.Fatalf("later consume: ok=%v err=%v, want true/nil — the token was never burned", ok, err)
		}
	})

	t.Run("restore emits on an actual revive", func(t *testing.T) {
		custID, invID := seedPayable(t, db, ctx, tenantID)
		raw, err := tokens.Create(ctx, tenantID, custID, invID)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}
		if ok, err := tokens.Consume(ctx, tenantID, raw); err != nil || !ok {
			t.Fatalf("consume: ok=%v err=%v", ok, err)
		}

		calls := 0
		if err := tokens.RestoreAudited(ctx, tenantID, raw, emitFor(custID, invID, "payment_update_checkout_restored", &calls)); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want exactly 1", calls)
		}
		if rows := auditRowsFor(t, ctx, logger, tenantID, "customer", custID, "payment_update_checkout_restored"); len(rows) != 1 {
			t.Fatalf("want one restore audit row; got %d: %+v", len(rows), rows)
		}
		if _, err := tokens.Validate(ctx, raw); err != nil {
			t.Errorf("validate after restore: %v (the revive did not commit)", err)
		}
	})

	t.Run("no-op restore emits nothing", func(t *testing.T) {
		custID, invID := seedPayable(t, db, ctx, tenantID)
		raw, err := tokens.Create(ctx, tenantID, custID, invID)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}

		// (a) Never consumed → nothing to revive.
		calls := 0
		if err := tokens.RestoreAudited(ctx, tenantID, raw, emitFor(custID, invID, "payment_update_checkout_restored", &calls)); err != nil {
			t.Fatalf("restore (unused token): %v", err)
		}
		if calls != 0 {
			t.Errorf("emit fired %d time(s) restoring a token that was never consumed", calls)
		}

		// (b) Consumed OUTSIDE the recency window — i.e. burned by a SUCCESSFUL
		// create minutes ago. The recency guard refuses to revive it, so the
		// emission must not claim a revival that never happened.
		if ok, err := tokens.Consume(ctx, tenantID, raw); err != nil || !ok {
			t.Fatalf("consume: ok=%v err=%v", ok, err)
		}
		btx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin bypass: %v", err)
		}
		if _, err := btx.ExecContext(ctx,
			`UPDATE payment_update_tokens SET used_at = NOW() - INTERVAL '10 minutes' WHERE customer_id = $1`,
			custID,
		); err != nil {
			t.Fatalf("age used_at: %v", err)
		}
		if err := btx.Commit(); err != nil {
			t.Fatalf("commit age: %v", err)
		}

		agedCalls := 0
		if err := tokens.RestoreAudited(ctx, tenantID, raw, emitFor(custID, invID, "payment_update_checkout_restored", &agedCalls)); err != nil {
			t.Fatalf("restore (aged token): %v", err)
		}
		if agedCalls != 0 {
			t.Errorf("emit fired %d time(s) on an out-of-window restore that revived no row", agedCalls)
		}
		if rows := auditRowsFor(t, ctx, logger, tenantID, "customer", custID, "payment_update_checkout_restored"); len(rows) != 0 {
			t.Errorf("fabricated revival rows for restores that revived nothing: %+v", rows)
		}
	})

	t.Run("emit failure rolls the restore back", func(t *testing.T) {
		custID, invID := seedPayable(t, db, ctx, tenantID)
		raw, err := tokens.Create(ctx, tenantID, custID, invID)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}
		if ok, err := tokens.Consume(ctx, tenantID, raw); err != nil || !ok {
			t.Fatalf("consume: ok=%v err=%v", ok, err)
		}

		calls := 0
		err = tokens.RestoreAudited(ctx, tenantID, raw, writeThenFail(custID, invID, "payment_update_checkout_restored", &calls))
		if err == nil {
			t.Fatal("restore must fail when its audit emission fails (shared fate)")
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want 1", calls)
		}
		if rows := auditRowsFor(t, ctx, logger, tenantID, "customer", custID, "payment_update_checkout_restored"); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back restore: %+v", rows)
		}
		// The revive rolled back too: the token stays burned.
		if _, err := tokens.Validate(ctx, raw); err == nil {
			t.Error("token validates after a rolled-back restore — the revive leaked")
		}
	})
}

// TestCheckoutClaimAudit_SharedFate pins the ADR-090 emission hook on
// CheckoutSessionStore.ClaimOpenAudited (the hosted-invoice-pay claim, which
// shipped with no fault injection):
//
//   - the WINNER (the call that created the claim) emits exactly once, and the
//     claim row + audit row commit together;
//   - an emit failure ABORTS the claim — no open claim is left behind to block
//     the next Pay click, and no audit row survives;
//   - the LOSER / reuse path (an open claim already exists) mutates nothing new
//     and therefore records nothing — otherwise every re-click of the Pay button
//     would log a fresh "checkout started" for a session that was never minted.
func TestCheckoutClaimAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Claim InTx Audit")

	store := payment.NewCheckoutSessionStore(db)
	logger := audit.NewLogger(db)

	emitFor := func(invID string, calls *int) func(*sql.Tx, payment.CheckoutClaim) error {
		return func(tx *sql.Tx, claim payment.CheckoutClaim) error {
			*calls++
			return logger.LogInTx(ctx, tx, audit.Entry{
				Action:       domain.AuditActionUpdate,
				ResourceType: "invoice",
				ResourceID:   invID,
				Metadata: map[string]any{
					"action":       "hosted_checkout_started",
					"amount_cents": claim.AmountCents,
					"claim_id":     claim.ID,
				},
			})
		}
	}

	t.Run("winner emits once and commits the claim", func(t *testing.T) {
		_, invID := seedPayable(t, db, ctx, tenantID)

		calls := 0
		claim, winner, err := store.ClaimOpenAudited(ctx, tenantID, invID, 10000, "USD", false, emitFor(invID, &calls))
		if err != nil || !winner {
			t.Fatalf("claim: winner=%v err=%v, want true/nil", winner, err)
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want exactly 1", calls)
		}
		rows := auditRowsFor(t, ctx, logger, tenantID, "invoice", invID, "hosted_checkout_started")
		if len(rows) != 1 {
			t.Fatalf("want exactly one checkout-started audit row; got %d: %+v", len(rows), rows)
		}
		if rows[0].Metadata["claim_id"] != claim.ID {
			t.Errorf("audit metadata claim_id: got %v, want %s", rows[0].Metadata["claim_id"], claim.ID)
		}
		// The claim row committed with it.
		open, err := store.GetOpenForInvoice(ctx, tenantID, invID)
		if err != nil {
			t.Fatalf("open claim after winner: %v", err)
		}
		if open.ID != claim.ID {
			t.Errorf("open claim = %s, want the winner's %s", open.ID, claim.ID)
		}
	})

	t.Run("emit failure rolls the claim back", func(t *testing.T) {
		_, invID := seedPayable(t, db, ctx, tenantID)

		calls := 0
		inner := emitFor(invID, &calls)
		_, winner, err := store.ClaimOpenAudited(ctx, tenantID, invID, 10000, "USD", false,
			func(tx *sql.Tx, claim payment.CheckoutClaim) error {
				if err := inner(tx, claim); err != nil {
					return err
				}
				return errors.New("injected audit failure")
			})
		if err == nil {
			t.Fatal("claim must fail when its audit emission fails (shared fate)")
		}
		if winner {
			t.Error("winner=true on a rolled-back claim")
		}
		if calls != 1 {
			t.Fatalf("emit calls = %d, want 1", calls)
		}
		if rows := auditRowsFor(t, ctx, logger, tenantID, "invoice", invID, "hosted_checkout_started"); len(rows) != 0 {
			t.Errorf("audit row survived the rolled-back claim: %+v", rows)
		}
		// No orphan claim: the next Pay click can still mint one (a leaked open
		// claim would send the customer to a session that was never created).
		if _, err := store.GetOpenForInvoice(ctx, tenantID, invID); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("open claim after rollback: err = %v, want ErrNotFound (the claim leaked)", err)
		}
		retryCalls := 0
		if _, winner, err := store.ClaimOpenAudited(ctx, tenantID, invID, 10000, "USD", false, emitFor(invID, &retryCalls)); err != nil || !winner {
			t.Fatalf("retry claim after rollback: winner=%v err=%v — the aborted claim poisoned the invoice", winner, err)
		}
	})

	t.Run("loser reusing an open claim emits nothing", func(t *testing.T) {
		_, invID := seedPayable(t, db, ctx, tenantID)

		winnerCalls := 0
		first, winner, err := store.ClaimOpenAudited(ctx, tenantID, invID, 10000, "USD", false, emitFor(invID, &winnerCalls))
		if err != nil || !winner {
			t.Fatalf("first claim: winner=%v err=%v", winner, err)
		}

		// Second call while the open claim stands: the partial unique index sends
		// it down the loser protocol — it gets the WINNER's row and records nothing.
		loserCalls := 0
		second, winner, err := store.ClaimOpenAudited(ctx, tenantID, invID, 10000, "USD", false, emitFor(invID, &loserCalls))
		if err != nil {
			t.Fatalf("second claim: %v", err)
		}
		if winner {
			t.Fatal("second claim won; the open-claim uniqueness guard must make it a loser")
		}
		if second.ID != first.ID {
			t.Fatalf("loser holds claim %s, want the winner's %s", second.ID, first.ID)
		}
		if loserCalls != 0 {
			t.Errorf("loser fired emit %d time(s); a reused claim mutates nothing and must record nothing", loserCalls)
		}
		if rows := auditRowsFor(t, ctx, logger, tenantID, "invoice", invID, "hosted_checkout_started"); len(rows) != 1 {
			t.Errorf("audit rows after reuse = %d, want 1 (the winner's only)", len(rows))
		}
	})
}
