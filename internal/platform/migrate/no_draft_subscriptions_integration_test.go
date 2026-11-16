package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// noDraftVersion is 00235_no_draft_subscriptions.sql; 00236 validates the
// CHECK it narrows.
const noDraftVersion = 235

// noDraftSeed is one account with an active subscription and four webhook
// endpoints: one that lists subscription.activated among other events, one
// that lists it alone, and two that do not name it.
const noDraftSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_nd', 'No draft');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_nd', 'ten_nd', 'nd', 'ND');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status) VALUES ('sub_nd', 'ten_nd', 'nd', 'ND', 'cus_nd', 'active');
INSERT INTO webhook_endpoints (id, tenant_id, url, secret_encrypted, secret_last4, events) VALUES
	('whe_mixed', 'ten_nd', 'https://example.test/mixed', 'x', 'xxxx', '["subscription.created", "subscription.activated", "invoice.paid"]'),
	('whe_only', 'ten_nd', 'https://example.test/only', 'x', 'xxxx', '["subscription.activated"]'),
	('whe_family', 'ten_nd', 'https://example.test/family', 'x', 'xxxx', '["subscription.*"]'),
	('whe_all', 'ten_nd', 'https://example.test/all', 'x', 'xxxx', '["*"]');
`

// endpointIs reports whether the endpoint's stored list equals events and
// its active flag equals active.
func endpointIs(t *testing.T, db *sql.DB, id, events string, active bool) bool {
	t.Helper()
	var ok bool
	err := withBypass(context.Background(), db, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT events = $2::jsonb AND active = $3 FROM webhook_endpoints WHERE id = $1`, id, events, active).Scan(&ok)
	})
	if err != nil {
		t.Fatalf("read endpoint %s: %v", id, err)
	}
	return ok
}

// TestNoDraftSubscriptions_StopsWhileADraftExists: a draft bills nothing, and
// only the release being upgraded from can start or cancel one. So 00235
// stops while a subscription is a draft and changes nothing. The error says
// how many there are, names the first five with their tenant, and says what
// to do, with a runbook section that exists. Once none is a draft it runs,
// and the CHECK refuses a new one.
func TestNoDraftSubscriptions_StopsWhileADraftExists(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_no_draft_refuse")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, noDraftVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", noDraftVersion, err)
	}
	exec := func(q string) {
		t.Helper()
		if err := deadContractExec(ctx, db, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(noDraftSeed)
	exec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status)
		SELECT 'sub_draft_' || g, 'ten_nd', 'draft-' || g, 'Draft', 'cus_nd', 'draft' FROM generate_series(1, 7) g`)

	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("Up ran over draft subscriptions; it must stop and name them")
	}
	t.Logf("the refusal: %v", err)
	for _, want := range []string{
		"this database has 7 of them",
		"sub_draft_1 (tenant ten_nd)",
		"sub_draft_5 (tenant ten_nd)",
		"and 2 more",
		"Start the release you are upgrading from, activate or cancel each draft subscription, then run the upgrade again",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "sub_draft_6") {
		t.Errorf("the refusal names more than the first five drafts:\n%v", err)
	}
	assertRunbookSectionExists(t, err.Error())
	if d := columnDefault(t, db, "subscriptions", "status"); d != "'draft'::text" {
		t.Errorf("the refused Up left subscriptions.status with the default %q, want 'draft'::text", d)
	}
	if !endpointIs(t, db, "whe_only", `["subscription.activated"]`, true) {
		t.Error("the refused Up changed a webhook endpoint")
	}

	exec(`UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE status = 'draft'`)
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up with no draft subscription: %v", err)
	}
	if got := refusedBy(t, db, `INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status)
		VALUES ('sub_new_draft', 'ten_nd', 'new-draft', 'Draft', 'cus_nd', 'draft')`); got != "subscriptions_status_check" {
		t.Errorf("after Up, a draft insert: refused by %q, want subscriptions_status_check", got)
	}
}

// TestNoDraftSubscriptions_UpAndDown: after 00235 and 00236 the status CHECK
// is validated and refuses draft, every other status still goes in, and the
// column has no default. A webhook endpoint's list loses
// subscription.activated, and an endpoint that listed nothing else is turned
// off. The Down puts the default and the old CHECK back and leaves the
// endpoint lists as the Up left them, and Up runs again.
func TestNoDraftSubscriptions_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_no_draft_round")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, noDraftVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", noDraftVersion, err)
	}
	if err := deadContractExec(ctx, db, noDraftSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	const toDraft = `UPDATE subscriptions SET status = 'draft' WHERE id = 'sub_nd'`
	if got := refusedBy(t, db, toDraft); got != "subscriptions_status_check" {
		t.Errorf("after Up, status draft: refused by %q, want subscriptions_status_check", got)
	}
	for _, status := range []string{"trialing", "canceled", "active"} {
		if got := refusedBy(t, db, `UPDATE subscriptions SET status = '`+status+`' WHERE id = 'sub_nd'`); got != "" {
			t.Errorf("after Up, status %s: refused by %q, want accepted", status, got)
		}
	}
	if d := columnDefault(t, db, "subscriptions", "status"); d != "" {
		t.Errorf("after Up, subscriptions.status has the default %q, want none: a default must not be a value the CHECK refuses", d)
	}
	var validated bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = 'subscriptions_status_check'`).Scan(&validated); err != nil || !validated {
		t.Errorf("subscriptions_status_check validated = %v (%v), want true", validated, err)
	}

	endpoints := []struct {
		id, events string
		active     bool
	}{
		{"whe_mixed", `["subscription.created", "invoice.paid"]`, true},
		// An empty list matches no event, so the endpoint is off.
		{"whe_only", `[]`, false},
		{"whe_family", `["subscription.*"]`, true},
		{"whe_all", `["*"]`, true},
	}
	for _, e := range endpoints {
		if !endpointIs(t, db, e.id, e.events, e.active) {
			t.Errorf("after Up, endpoint %s is not %s with active %v", e.id, e.events, e.active)
		}
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
	for _, e := range endpoints {
		if !endpointIs(t, db, e.id, e.events, e.active) {
			t.Errorf("after the Down, endpoint %s is not as the Up left it", e.id)
		}
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if got := refusedBy(t, db, toDraft); got != "subscriptions_status_check" {
		t.Errorf("after the second Up, status draft: refused by %q, want subscriptions_status_check", got)
	}
}
