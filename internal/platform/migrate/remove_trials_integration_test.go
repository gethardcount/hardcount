package migrate

import (
	"context"
	"strings"
	"testing"
)

// removeTrialsVersion is 00249_remove_trials.sql; 00250 validates the CHECK
// it narrows.
const removeTrialsVersion = 249

// removeTrialsSeed is two accounts. The first has a trialing subscription
// scheduled to cancel at its period end, one scheduled on a date, one with no
// schedule, an active subscription that once trialed (scheduled to cancel)
// and a canceled one, and five webhook endpoints: one that lists a trial
// event among other events, one that lists one trial event alone, one that
// lists both alone, and two that name neither. The second account has one
// trialing subscription.
const removeTrialsSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_rt', 'Remove trials'), ('ten_rt2', 'Remove trials 2');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES
	('cus_rt', 'ten_rt', 'rt', 'RT'), ('cus_rt2', 'ten_rt2', 'rt2', 'RT2');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status,
		trial_start_at, trial_end_at, started_at, activated_at, canceled_at, cancel_at, cancel_at_period_end) VALUES
	('sub_trial_period_end', 'ten_rt', 'trial-period-end', 'Trial', 'cus_rt', 'trialing',
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-11-01T00:00:00Z', NULL, NULL, NULL, true),
	('sub_trial_dated', 'ten_rt', 'trial-dated', 'Trial', 'cus_rt', 'trialing',
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-11-01T00:00:00Z', NULL, NULL, '2026-12-01T00:00:00Z', false),
	('sub_trial', 'ten_rt', 'trial', 'Trial', 'cus_rt', 'trialing',
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-11-01T00:00:00Z', NULL, NULL, NULL, false),
	('sub_trial_2', 'ten_rt2', 'trial', 'Trial', 'cus_rt2', 'trialing',
		'2026-11-01T00:00:00Z', '2026-12-01T00:00:00Z', '2026-11-01T00:00:00Z', NULL, NULL, NULL, false),
	('sub_active', 'ten_rt', 'active', 'Active', 'cus_rt', 'active',
		'2026-09-01T00:00:00Z', '2026-10-01T00:00:00Z', '2026-09-01T00:00:00Z', '2026-10-01T00:00:00Z', NULL, NULL, true),
	('sub_canceled', 'ten_rt', 'canceled', 'Canceled', 'cus_rt', 'canceled',
		NULL, NULL, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z', NULL, false);
INSERT INTO webhook_endpoints (id, tenant_id, url, secret_encrypted, secret_last4, events) VALUES
	('whe_mixed', 'ten_rt', 'https://example.test/mixed', 'x', 'xxxx', '["subscription.created", "subscription.trial_ended", "invoice.paid"]'),
	('whe_one', 'ten_rt', 'https://example.test/one', 'x', 'xxxx', '["subscription.trial_extended"]'),
	('whe_both', 'ten_rt', 'https://example.test/both', 'x', 'xxxx', '["subscription.trial_ended", "subscription.trial_extended"]'),
	('whe_family', 'ten_rt', 'https://example.test/family', 'x', 'xxxx', '["subscription.*"]'),
	('whe_all', 'ten_rt', 'https://example.test/all', 'x', 'xxxx', '["*"]');
`

// TestRemoveTrials_UpAndDown: 00249 cancels every trialing subscription in
// every account and clears its cancel schedule as a cancel does, writes no
// event or audit row, and changes no other subscription. After it and 00250
// the status CHECK is validated and refuses trialing, the trial columns and
// activated_at are gone, and the billing_failed_for comment no longer names
// trialing. A webhook endpoint that listed only trial events is deleted with
// its list kept, and every other list loses the names. The Down puts the
// columns back, empty, with the old CHECK, and leaves the rows as the Up left
// them, and Up runs again.
func TestRemoveTrials_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_remove_trials_round")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, removeTrialsVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", removeTrialsVersion, err)
	}
	if err := deadContractExec(ctx, db, removeTrialsSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The columns the Up keeps, of the subscriptions it must not change.
	const others = `SELECT string_agg(concat_ws(' ', id, status, started_at, canceled_at, cancel_at, cancel_at_period_end, updated_at), E'\n' ORDER BY id)
		FROM subscriptions WHERE id IN ('sub_active', 'sub_canceled')`
	othersBefore := noDraftScalar[string](t, db, others)
	const written = `SELECT (SELECT count(*) FROM audit_log) + (SELECT count(*) FROM webhook_outbox)`
	writtenBefore := noDraftScalar[int](t, db, written)

	// Up through this migration and its validate (00250) only.
	if err := up(ctx, dsn, filesBefore(t, removeTrialsVersion+2)); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions
		WHERE id IN ('sub_trial_period_end', 'sub_trial_dated', 'sub_trial', 'sub_trial_2')
		  AND status = 'canceled' AND canceled_at IS NOT NULL
		  AND cancel_at IS NULL AND NOT cancel_at_period_end`); n != 4 {
		t.Errorf("after Up, %d of the 4 trialing subscriptions are canceled with canceled_at set and no cancel schedule, want 4", n)
	}
	if got := noDraftScalar[string](t, db, others); got != othersBefore {
		t.Errorf("Up changed a subscription that was not trialing:\nbefore %s\nafter  %s", othersBefore, got)
	}
	if got := noDraftScalar[int](t, db, written); got != writtenBefore {
		t.Errorf("Up wrote %d audit or outbox rows, want none", got-writtenBefore)
	}
	const toTrialing = `UPDATE subscriptions SET status = 'trialing' WHERE id = 'sub_active'`
	if got := refusedBy(t, db, toTrialing); got != "subscriptions_status_check" {
		t.Errorf("after Up, status trialing: refused by %q, want subscriptions_status_check", got)
	}
	if !noDraftScalar[bool](t, db, `SELECT convalidated FROM pg_constraint WHERE conname = 'subscriptions_status_check'`) {
		t.Error("after Up, subscriptions_status_check is not validated")
	}
	for _, col := range []string{"trial_start_at", "trial_end_at", "activated_at"} {
		if columnExists(t, db, "subscriptions", col) {
			t.Errorf("after Up, subscriptions.%s still exists", col)
		}
	}
	const comment = `SELECT col_description('subscriptions'::regclass,
		(SELECT attnum FROM pg_attribute WHERE attrelid = 'subscriptions'::regclass AND attname = 'billing_failed_for'))`
	if got := noDraftScalar[string](t, db, comment); !strings.Contains(got, "on an active row;") || strings.Contains(got, "trialing") {
		t.Errorf("after Up, the billing_failed_for comment is %q, want it to name active rows only", got)
	}

	// One line per endpoint: its list, whether it is active, whether it is
	// deleted.
	const endpoints = `SELECT string_agg(id || ' ' || events::text || ' ' || active || ' ' || (deleted_at IS NOT NULL), E'\n' ORDER BY id)
		FROM webhook_endpoints`
	const wantEndpoints = `whe_all ["*"] true false
whe_both ["subscription.trial_ended", "subscription.trial_extended"] false true
whe_family ["subscription.*"] true false
whe_mixed ["subscription.created", "invoice.paid"] true false
whe_one ["subscription.trial_extended"] false true`
	if got := noDraftScalar[string](t, db, endpoints); got != wantEndpoints {
		t.Errorf("after Up, the webhook endpoints are:\n%s\nwant:\n%s", got, wantEndpoints)
	}

	if _, err := down(ctx, dsn, migrationFiles(), removeTrialsVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", removeTrialsVersion-1, err)
	}
	if got := refusedBy(t, db, toTrialing); got != "" {
		t.Errorf("after the Down, status trialing: refused by %q, want accepted", got)
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions
		WHERE trial_start_at IS NOT NULL OR trial_end_at IS NOT NULL OR activated_at IS NOT NULL`); n != 0 {
		t.Errorf("after the Down, %d subscriptions have a trial column or activated_at set, want 0: the columns come back empty", n)
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions WHERE status = 'trialing'`); n != 0 {
		t.Errorf("after the Down, %d subscriptions are trialing again, want 0", n)
	}
	if got := noDraftScalar[string](t, db, endpoints); got != wantEndpoints {
		t.Errorf("after the Down, the webhook endpoints are not as the Up left them:\n%s", got)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
