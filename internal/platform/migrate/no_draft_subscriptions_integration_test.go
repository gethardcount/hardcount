package migrate

import (
	"context"
	"database/sql"
	"testing"
)

// noDraftVersion is 00235_no_draft_subscriptions.sql; 00236 validates the
// CHECK it narrows.
const noDraftVersion = 235

// noDraftSeed is two accounts. The first has two drafts (one scheduled to
// cancel), a trialing, an active and a canceled subscription, and four
// webhook endpoints: one that lists subscription.activated among other
// events, one that lists it alone, and two that do not name it. The second
// account has one draft.
const noDraftSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_nd', 'No draft'), ('ten_nd2', 'No draft 2');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES
	('cus_nd', 'ten_nd', 'nd', 'ND'), ('cus_nd2', 'ten_nd2', 'nd2', 'ND2');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, canceled_at, cancel_at_period_end) VALUES
	('sub_draft', 'ten_nd', 'draft', 'Draft', 'cus_nd', 'draft', NULL, false),
	('sub_draft_scheduled', 'ten_nd', 'draft-scheduled', 'Draft', 'cus_nd', 'draft', NULL, true),
	('sub_draft_2', 'ten_nd2', 'draft', 'Draft', 'cus_nd2', 'draft', NULL, false),
	('sub_trialing', 'ten_nd', 'trialing', 'Trialing', 'cus_nd', 'trialing', NULL, false),
	('sub_active', 'ten_nd', 'active', 'Active', 'cus_nd', 'active', NULL, true),
	('sub_canceled', 'ten_nd', 'canceled', 'Canceled', 'cus_nd', 'canceled', '2026-01-01T00:00:00Z', false);
INSERT INTO webhook_endpoints (id, tenant_id, url, secret_encrypted, secret_last4, events) VALUES
	('whe_mixed', 'ten_nd', 'https://example.test/mixed', 'x', 'xxxx', '["subscription.created", "subscription.activated", "invoice.paid"]'),
	('whe_only', 'ten_nd', 'https://example.test/only', 'x', 'xxxx', '["subscription.activated"]'),
	('whe_family', 'ten_nd', 'https://example.test/family', 'x', 'xxxx', '["subscription.*"]'),
	('whe_all', 'ten_nd', 'https://example.test/all', 'x', 'xxxx', '["*"]');
`

// noDraftScalar returns the one value q selects, read with row-level
// security bypassed.
func noDraftScalar[T any](t *testing.T, db *sql.DB, q string) T {
	t.Helper()
	var v T
	err := withBypass(context.Background(), db, func(tx *sql.Tx) error {
		return tx.QueryRow(q).Scan(&v)
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// TestNoDraftSubscriptions_UpAndDown: 00235 cancels every draft subscription
// in every account, clears its cancel schedule as a cancel does, and changes
// no other subscription. After it and 00236 the status CHECK is validated and
// refuses draft, and the column has no default. A webhook endpoint that
// listed only subscription.activated is deleted with its list kept, and every
// other list loses the name. The Down puts the default and the old CHECK back
// and leaves the rows as the Up left them, and Up runs again.
func TestNoDraftSubscriptions_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_no_draft_round")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, noDraftVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", noDraftVersion, err)
	}
	if err := deadContractExec(ctx, db, noDraftSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const others = `SELECT string_agg(to_jsonb(s)::text, ' ' ORDER BY id) FROM subscriptions s
		WHERE id IN ('sub_trialing', 'sub_active', 'sub_canceled')`
	othersBefore := noDraftScalar[string](t, db, others)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions
		WHERE id IN ('sub_draft', 'sub_draft_scheduled', 'sub_draft_2')
		  AND status = 'canceled' AND canceled_at IS NOT NULL
		  AND cancel_at IS NULL AND NOT cancel_at_period_end`); n != 3 {
		t.Errorf("after Up, %d of the 3 drafts are canceled with canceled_at set and no cancel schedule, want 3", n)
	}
	if got := noDraftScalar[string](t, db, others); got != othersBefore {
		t.Errorf("Up changed a subscription that was not a draft:\nbefore %s\nafter  %s", othersBefore, got)
	}
	const toDraft = `UPDATE subscriptions SET status = 'draft' WHERE id = 'sub_active'`
	if got := refusedBy(t, db, toDraft); got != "subscriptions_status_check" {
		t.Errorf("after Up, status draft: refused by %q, want subscriptions_status_check", got)
	}
	if d := columnDefault(t, db, "subscriptions", "status"); d != "" {
		t.Errorf("after Up, subscriptions.status has the default %q, want none: a default must not be a value the CHECK refuses", d)
	}
	if !noDraftScalar[bool](t, db, `SELECT convalidated FROM pg_constraint WHERE conname = 'subscriptions_status_check'`) {
		t.Error("after Up, subscriptions_status_check is not validated")
	}

	// One line per endpoint: its list, whether it is active, whether it is
	// deleted.
	const endpoints = `SELECT string_agg(id || ' ' || events::text || ' ' || active || ' ' || (deleted_at IS NOT NULL), E'\n' ORDER BY id)
		FROM webhook_endpoints`
	const wantEndpoints = `whe_all ["*"] true false
whe_family ["subscription.*"] true false
whe_mixed ["subscription.created", "invoice.paid"] true false
whe_only ["subscription.activated"] false true`
	if got := noDraftScalar[string](t, db, endpoints); got != wantEndpoints {
		t.Errorf("after Up, the webhook endpoints are:\n%s\nwant:\n%s", got, wantEndpoints)
	}

	if _, err := down(ctx, dsn, migrationFiles(), noDraftVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", noDraftVersion-1, err)
	}
	if got := refusedBy(t, db, toDraft); got != "" {
		t.Errorf("after the Down, status draft: refused by %q, want accepted", got)
	}
	if d := columnDefault(t, db, "subscriptions", "status"); d != "'draft'::text" {
		t.Errorf("after the Down, subscriptions.status has the default %q, want 'draft'::text", d)
	}
	if n := noDraftScalar[int](t, db, `SELECT count(*) FROM subscriptions WHERE status = 'draft'`); n != 0 {
		t.Errorf("after the Down, %d subscriptions are drafts again, want 0", n)
	}
	if got := noDraftScalar[string](t, db, endpoints); got != wantEndpoints {
		t.Errorf("after the Down, the webhook endpoints are not as the Up left them:\n%s", got)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
