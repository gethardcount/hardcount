package migrate

import (
	"context"
	"testing"
)

// clearLiveStateVersion is 00192_clear_canceled_subscription_live_state.sql.
const clearLiveStateVersion = 192

// clearLiveStateSeed is one account with two canceled subscriptions that kept
// what an earlier cancel left (a cancel schedule, a pending plan change, a
// collection pause) and one active subscription with the same state.
const clearLiveStateSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_cls', 'Clear live state');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_cls', 'ten_cls', 'cls', 'CLS');
INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, status) VALUES
	('pln_cls_a', 'ten_cls', 'a', 'A', 'USD', 'monthly', 'active'),
	('pln_cls_b', 'ten_cls', 'b', 'B', 'USD', 'monthly', 'active');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, canceled_at,
		cancel_at, cancel_at_period_end, pause_collection_behavior, pause_collection_resumes_at) VALUES
	('sub_cls_period_end', 'ten_cls', 'period-end', 'P', 'cus_cls', 'canceled', '2026-08-10T00:00:00Z',
		NULL, true, 'keep_as_draft', '2026-12-01T00:00:00Z'),
	('sub_cls_dated', 'ten_cls', 'dated', 'D', 'cus_cls', 'canceled', '2026-08-10T00:00:00Z',
		'2026-09-15T00:00:00Z', false, 'keep_as_draft', '2026-12-01T00:00:00Z'),
	('sub_cls_live', 'ten_cls', 'live', 'L', 'cus_cls', 'active', NULL,
		NULL, true, 'keep_as_draft', '2026-12-01T00:00:00Z');
INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, pending_plan_id, pending_plan_effective_at) VALUES
	('si_cls_period_end', 'ten_cls', 'sub_cls_period_end', 'pln_cls_a', 'pln_cls_b', '2026-09-01T00:00:00Z'),
	('si_cls_dated', 'ten_cls', 'sub_cls_dated', 'pln_cls_a', 'pln_cls_b', '2026-09-01T00:00:00Z'),
	('si_cls_live', 'ten_cls', 'sub_cls_live', 'pln_cls_a', 'pln_cls_b', '2026-09-01T00:00:00Z');
`

// TestClearCanceledLiveState_Up: 00192 clears, on every canceled
// subscription, the cancel schedule and its items' pending plan change, both
// of which say what a live subscription does next, and keeps the collection
// pause, which still holds its unpaid invoices. It writes no item-change row
// and leaves an active subscription as it was.
func TestClearCanceledLiveState_Up(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_clear_live_state")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, clearLiveStateVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", clearLiveStateVersion, err)
	}
	if err := deadContractExec(ctx, db, clearLiveStateSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const state = `SELECT string_agg(concat_ws(' ', s.id, s.status, (s.cancel_at AT TIME ZONE 'UTC')::date, s.cancel_at_period_end,
			s.pause_collection_behavior, (s.pause_collection_resumes_at AT TIME ZONE 'UTC')::date,
			i.pending_plan_id, (i.pending_plan_effective_at AT TIME ZONE 'UTC')::date), E'\n' ORDER BY s.id)
		FROM subscriptions s JOIN subscription_items i ON i.subscription_id = s.id`
	const changes = `SELECT count(*) FROM subscription_item_changes`
	changesBefore := noDraftScalar[int](t, db, changes)

	if err := up(ctx, dsn, filesBefore(t, clearLiveStateVersion+1)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	const want = `sub_cls_dated canceled f keep_as_draft 2026-12-01
sub_cls_live active t keep_as_draft 2026-12-01 pln_cls_b 2026-09-01
sub_cls_period_end canceled f keep_as_draft 2026-12-01`
	if got := noDraftScalar[string](t, db, state); got != want {
		t.Errorf("after Up:\n%s\nwant:\n%s", got, want)
	}
	if got := noDraftScalar[int](t, db, changes); got != changesBefore {
		t.Errorf("Up wrote %d item-change rows, want none: clearing a pending plan is not a plan change", got-changesBefore)
	}
}
