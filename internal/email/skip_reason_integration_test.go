package email

import (
	"context"
	"regexp"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestDispatcher_SkippedEmailRecordsWhy: when the dispatcher skips an
// action-required email because its invoice reached a final state while
// the email sat queued (an SMTP outage, a stopped dispatcher; found live on
// the FLOW E walk, 2026-07-19), the row records which state that was, as a
// field. The activity line names it ("The invoice was already paid"); before
// this, the state lived only inside the row's error text, so every surface
// could only say the invoice was "closed" (ADR-128 §3.1, I-9 to I-13).
func TestDispatcher_SkippedEmailRecordsWhy(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleEmailOutbox)
	tenantID := testutil.CreateTestTenant(t, db, "Skip Reason")
	store := NewOutboxStore(db)

	for _, state := range []string{"paid", "voided", "uncollectible", "gone"} {
		t.Run(state, func(t *testing.T) {
			id, err := store.EnqueueStandalone(ctx, tenantID, TypePaymentFailed, map[string]any{
				"to": "billing@skip-reason.test", "customer_name": "C", "invoice_number": "SKIP-" + state,
				"reason": "Your card was declined.",
			})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			sender := &recordingDeliverer{}
			d := NewDispatcher(store, sender, DispatcherConfig{}, leader.AlwaysLead{})
			d.SetSettledChecker(&recordingChecker{state: state})
			if _, err := store.ProcessBatch(ctx, 5, d.handle); err != nil {
				t.Fatalf("process batch: %v", err)
			}
			if len(sender.sent) != 0 {
				t.Fatalf("sent %v; an email whose invoice is %s must not go out", sender.sent, state)
			}

			tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer postgres.Rollback(tx)
			var status, reason string
			if err := tx.QueryRow(
				`SELECT status, COALESCE(skip_reason, '') FROM email_outbox WHERE id = $1`, id,
			).Scan(&status, &reason); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if status != "skipped" || reason != state {
				t.Errorf("row: status %q, skip_reason %q; want skipped, %q", status, reason, state)
			}
		})
	}
}

// TestSkipReasons_MatchCheckConstraint keeps SkipReasons and migration
// 00186's CHECK equal (the enum and CHECK sync rule): a reason the CHECK did
// not list would fail the skip mark, and the row would never end.
func TestSkipReasons_MatchCheckConstraint(t *testing.T) {
	testutil.SetupTestDB(t)
	admin := testutil.AdminPool(t)
	var def string
	if err := admin.QueryRowContext(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'email_outbox_skip_reason_check'`,
	).Scan(&def); err != nil {
		t.Fatalf("read the CHECK: %v", err)
	}
	inCheck := map[string]bool{}
	for _, m := range regexp.MustCompile(`'(\w+)'`).FindAllStringSubmatch(def, -1) {
		inCheck[m[1]] = true
	}
	for _, r := range SkipReasons() {
		if !inCheck[r] {
			t.Errorf("skip reason %q is not in the CHECK (%s)", r, def)
		}
		delete(inCheck, r)
	}
	for r := range inCheck {
		t.Errorf("the CHECK allows %q, which no Go constant names", r)
	}
}
