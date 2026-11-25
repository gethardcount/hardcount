package migrate

import (
	"context"
	"strings"
	"testing"
)

// removeCancelAtVersion is 00252_remove_cancel_at.sql.
const removeCancelAtVersion = 252

// removeCancelAtSeed is two accounts whose subscriptions are all in the
// period [Nov 1, Dec 1). The first has an active subscription scheduled on
// its period's end with cancel_at, one scheduled on a date in a later period,
// one on a date inside its period that came without the cancel running, one
// flagged to cancel at its period's end, one with no schedule, and a canceled
// one that still has a cancel_at on its period's end. The second account has
// one subscription scheduled on its period's end with cancel_at.
const removeCancelAtSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_rc', 'Remove cancel_at'), ('ten_rc2', 'Remove cancel_at 2');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES
	('cus_rc', 'ten_rc', 'rc', 'RC'), ('cus_rc2', 'ten_rc2', 'rc2', 'RC2');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, started_at, canceled_at,
		current_billing_period_start, current_billing_period_end, next_billing_at, cancel_at, cancel_at_period_end) VALUES
	('sub_on_period_end', 'ten_rc', 'on-period-end', 'S', 'cus_rc', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', false),
	('sub_later_date', 'ten_rc', 'later-date', 'S', 'cus_rc', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', '2027-01-16T00:00:00Z', false),
	('sub_date_inside', 'ten_rc', 'date-inside', 'S', 'cus_rc', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-11-16T00:00:00Z', false),
	('sub_flagged', 'ten_rc', 'flagged', 'S', 'cus_rc', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', NULL, true),
	('sub_plain', 'ten_rc', 'plain', 'S', 'cus_rc', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', NULL, false),
	('sub_canceled', 'ten_rc', 'canceled', 'S', 'cus_rc', 'canceled', '2026-10-01T00:00:00Z', '2026-11-10T00:00:00Z',
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', false),
	('sub_on_period_end_2', 'ten_rc2', 'on-period-end', 'S', 'cus_rc2', 'active', '2026-10-01T00:00:00Z', NULL,
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-12-01T00:00:00Z', false);
`

// TestRemoveCancelAt_UpAndDown: 00252 flags cancel_at_period_end on every
// active subscription, in every account, whose cancel_at is its period's end.
// Every other schedule on a date is dropped and its subscription goes on
// billing, with its status, period and next bill date as they were. A
// subscription already flagged, one with no schedule and a canceled one are
// unchanged. No row's updated_at moves, and no event or audit row is written.
// After it the column and its index are gone and the billing_failed_for
// comment names next_billing_at. The Down puts the column back, empty, with
// its index and the old comment, and leaves the flags as the Up left them,
// and Up runs again.
func TestRemoveCancelAt_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_remove_cancel_at_round")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, removeCancelAtVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", removeCancelAtVersion, err)
	}
	if err := deadContractExec(ctx, db, removeCancelAtSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Every column the Up keeps but the flag, of every subscription.
	const kept = `SELECT string_agg(concat_ws(' ', id, status, started_at, canceled_at,
			current_billing_period_start, current_billing_period_end, next_billing_at, updated_at), E'\n' ORDER BY id)
		FROM subscriptions`
	keptBefore := noDraftScalar[string](t, db, kept)
	const written = `SELECT (SELECT count(*) FROM audit_log) + (SELECT count(*) FROM webhook_outbox)`
	writtenBefore := noDraftScalar[int](t, db, written)

	if err := up(ctx, dsn, filesBefore(t, removeCancelAtVersion+1)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	const flags = `SELECT string_agg(id || ' ' || cancel_at_period_end, E'\n' ORDER BY id) FROM subscriptions`
	const wantFlags = `sub_canceled false
sub_date_inside false
sub_flagged true
sub_later_date false
sub_on_period_end true
sub_on_period_end_2 true
sub_plain false`
	if got := noDraftScalar[string](t, db, flags); got != wantFlags {
		t.Errorf("after Up, cancel_at_period_end is:\n%s\nwant:\n%s", got, wantFlags)
	}
	if got := noDraftScalar[string](t, db, kept); got != keptBefore {
		t.Errorf("Up changed a column other than the flag:\nbefore %s\nafter  %s", keptBefore, got)
	}
	if got := noDraftScalar[int](t, db, written); got != writtenBefore {
		t.Errorf("Up wrote %d audit or outbox rows, want none", got-writtenBefore)
	}
	if columnExists(t, db, "subscriptions", "cancel_at") {
		t.Error("after Up, subscriptions.cancel_at still exists")
	}
	if indexExists(t, db, "idx_subscriptions_cancel_at") {
		t.Error("after Up, idx_subscriptions_cancel_at still exists")
	}
	const comment = `SELECT col_description('subscriptions'::regclass,
		(SELECT attnum FROM pg_attribute WHERE attrelid = 'subscriptions'::regclass AND attname = 'billing_failed_for'))`
	if got := noDraftScalar[string](t, db, comment); !strings.Contains(got, "equals next_billing_at on an active row;") || strings.Contains(got, "cancel_at") {
		t.Errorf("after Up, the billing_failed_for comment is %q, want it to name next_billing_at alone", got)
	}

	if _, err := down(ctx, dsn, migrationFiles(), removeCancelAtVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", removeCancelAtVersion-1, err)
	}
	if !columnExists(t, db, "subscriptions", "cancel_at") || !indexExists(t, db, "idx_subscriptions_cancel_at") {
		t.Fatal("after the Down, subscriptions.cancel_at or its index is missing")
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions WHERE cancel_at IS NOT NULL`); n != 0 {
		t.Errorf("after the Down, %d subscriptions have cancel_at set, want 0: the column comes back empty", n)
	}
	if got := noDraftScalar[string](t, db, flags); got != wantFlags {
		t.Errorf("after the Down, cancel_at_period_end is not as the Up left it:\n%s", got)
	}
	if got := noDraftScalar[string](t, db, comment); !strings.Contains(got, "cancel_at END") {
		t.Errorf("after the Down, the billing_failed_for comment is %q, want the one before the Up", got)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
