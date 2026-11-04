// Package subscriptiontest holds test-only fixtures for the subscription
// domain, in the same shape as platform/leader/leadertest. Nothing here is
// reachable from production code.
package subscriptiontest

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// SetBillingCycle re-stamps a newly created subscription's period
// boundaries, next_billing_at and billing anchor day. It uses raw SQL on
// the app pool, scoped by RLS to tenantID. It is test-only on purpose.
// Production has no unguarded period writer: every closer (the code that
// closes a billing period) proves a subscription.PeriodSnapshot in its
// first statement (ADR-115). A fixture that rewinds a subscription to
// "due" has no snapshot to prove.
func SetBillingCycle(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, subID string, start, end, next time.Time, anchorDay int) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("subscriptiontest.SetBillingCycle: begin: %v", err)
	}
	defer postgres.Rollback(tx)
	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions
		   SET current_billing_period_start = $1,
		       current_billing_period_end   = $2,
		       next_billing_at              = $3,
		       billing_anchor_day           = $4,
		       updated_at                   = now()
		 WHERE id = $5
	`, start, end, next, anchorDay, subID)
	if err != nil {
		t.Fatalf("subscriptiontest.SetBillingCycle: update %s: %v", subID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("subscriptiontest.SetBillingCycle: %s matched %d rows, want 1 (wrong tenant on ctx?)", subID, n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("subscriptiontest.SetBillingCycle: commit: %v", err)
	}
}
