package migrate

import (
	"context"
	"strings"
	"testing"
)

// removeScheduledCancelVersion is 00252_remove_scheduled_cancel.sql.
const removeScheduledCancelVersion = 252

// removeScheduledCancelSeed is one account with an active subscription
// scheduled to cancel at its period end, one scheduled to cancel on a date in
// a later period, one with no schedule and a canceled one, and five webhook
// endpoints: one that lists a cancel-schedule event among other events, one
// that lists one of them alone, one that lists both alone, and two that name
// neither.
const removeScheduledCancelSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_rsc', 'Remove scheduled cancel');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_rsc', 'ten_rsc', 'rsc', 'RSC');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, started_at, canceled_at,
		current_billing_period_start, current_billing_period_end, next_billing_at, billing_anchor_day,
		cancel_at, cancel_at_period_end) VALUES
	('sub_period_end', 'ten_rsc', 'period-end', 'Period end', 'cus_rsc', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', 1, NULL, true),
	('sub_dated', 'ten_rsc', 'dated', 'Dated', 'cus_rsc', 'active', '2026-10-15T00:00:00Z', NULL,
		'2026-11-15T00:00:00Z', '2026-12-15T00:00:00Z', '2026-12-15T00:00:00Z', 15, '2027-01-20T00:00:00Z', false),
	('sub_plain', 'ten_rsc', 'plain', 'Plain', 'cus_rsc', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', 1, NULL, false),
	('sub_canceled', 'ten_rsc', 'canceled', 'Canceled', 'cus_rsc', 'canceled', '2026-01-01T00:00:00Z', '2026-02-10T00:00:00Z',
		'2026-02-01T00:00:00Z', '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z', 1, NULL, false);
INSERT INTO webhook_endpoints (id, tenant_id, url, secret_encrypted, secret_last4, events) VALUES
	('whe_mixed', 'ten_rsc', 'https://example.test/mixed', 'x', 'xxxx', '["subscription.created", "subscription.cancel_scheduled", "invoice.paid"]'),
	('whe_one', 'ten_rsc', 'https://example.test/one', 'x', 'xxxx', '["subscription.cancel_cleared"]'),
	('whe_both', 'ten_rsc', 'https://example.test/both', 'x', 'xxxx', '["subscription.cancel_scheduled", "subscription.cancel_cleared"]'),
	('whe_family', 'ten_rsc', 'https://example.test/family', 'x', 'xxxx', '["subscription.*"]'),
	('whe_all', 'ten_rsc', 'https://example.test/all', 'x', 'xxxx', '["*"]');
`

// TestRemoveScheduledCancel_UpAndDown: 00252 leaves every subscription as it
// was, apart from the two schedule columns it drops: one scheduled to cancel
// at its period end or on a date stays active with its period and next bill
// date, and writes no event or audit row. The index on cancel_at goes with
// its column, and the billing_failed_for comment names next_billing_at. A
// webhook endpoint that listed only the two events is deleted with its list
// kept, and every other list loses the names. The Down puts the columns back,
// empty, with their index, and leaves the endpoints as the Up left them, and
// Up runs again.
func TestRemoveScheduledCancel_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_remove_scheduled_cancel")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, removeScheduledCancelVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", removeScheduledCancelVersion, err)
	}
	if err := deadContractExec(ctx, db, removeScheduledCancelSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Every column of every subscription that the Up keeps.
	const kept = `SELECT string_agg(concat_ws(' ', id, status, started_at, canceled_at,
			current_billing_period_start, current_billing_period_end, next_billing_at, billing_anchor_day, updated_at), E'\n' ORDER BY id)
		FROM subscriptions`
	keptBefore := noDraftScalar[string](t, db, kept)
	const written = `SELECT (SELECT count(*) FROM audit_log) + (SELECT count(*) FROM webhook_outbox)`
	writtenBefore := noDraftScalar[int](t, db, written)

	if err := up(ctx, dsn, filesBefore(t, removeScheduledCancelVersion+1)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if got := noDraftScalar[string](t, db, kept); got != keptBefore {
		t.Errorf("Up changed a subscription:\nbefore %s\nafter  %s", keptBefore, got)
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions
		WHERE id IN ('sub_period_end', 'sub_dated') AND status = 'active'`); n != 2 {
		t.Errorf("after Up, %d of the 2 subscriptions that had a scheduled cancel are active, want 2", n)
	}
	if got := noDraftScalar[int](t, db, written); got != writtenBefore {
		t.Errorf("Up wrote %d audit or outbox rows, want none", got-writtenBefore)
	}
	for _, col := range []string{"cancel_at", "cancel_at_period_end"} {
		if columnExists(t, db, "subscriptions", col) {
			t.Errorf("after Up, subscriptions.%s still exists", col)
		}
	}
	if indexExists(t, db, "idx_subscriptions_cancel_at") {
		t.Error("after Up, idx_subscriptions_cancel_at still exists")
	}
	const comment = `SELECT col_description('subscriptions'::regclass,
		(SELECT attnum FROM pg_attribute WHERE attrelid = 'subscriptions'::regclass AND attname = 'billing_failed_for'))`
	if got := noDraftScalar[string](t, db, comment); !strings.Contains(got, "equals next_billing_at on an active row;") || strings.Contains(got, "cancel_at") {
		t.Errorf("after Up, the billing_failed_for comment is %q, want it to name next_billing_at only", got)
	}

	// One line per endpoint: its list, whether it is active, whether it is
	// deleted.
	const endpoints = `SELECT string_agg(id || ' ' || events::text || ' ' || active || ' ' || (deleted_at IS NOT NULL), E'\n' ORDER BY id)
		FROM webhook_endpoints`
	const wantEndpoints = `whe_all ["*"] true false
whe_both ["subscription.cancel_scheduled", "subscription.cancel_cleared"] false true
whe_family ["subscription.*"] true false
whe_mixed ["subscription.created", "invoice.paid"] true false
whe_one ["subscription.cancel_cleared"] false true`
	if got := noDraftScalar[string](t, db, endpoints); got != wantEndpoints {
		t.Errorf("after Up, the webhook endpoints are:\n%s\nwant:\n%s", got, wantEndpoints)
	}

	if _, err := down(ctx, dsn, migrationFiles(), removeScheduledCancelVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", removeScheduledCancelVersion-1, err)
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions
		WHERE cancel_at IS NOT NULL OR cancel_at_period_end`); n != 0 {
		t.Errorf("after the Down, %d subscriptions have a cancel scheduled, want 0: the columns come back empty", n)
	}
	if !indexExists(t, db, "idx_subscriptions_cancel_at") {
		t.Error("after the Down, idx_subscriptions_cancel_at is missing")
	}
	if got := noDraftScalar[string](t, db, kept); got != keptBefore {
		t.Errorf("the Down changed a subscription:\nbefore %s\nafter  %s", keptBefore, got)
	}
	if got := noDraftScalar[string](t, db, endpoints); got != wantEndpoints {
		t.Errorf("after the Down, the webhook endpoints are not as the Up left them:\n%s", got)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
