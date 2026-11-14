package migrate

import (
	"context"
	"testing"
)

const deleteKeptShowOnceAnswersVersion = 229

// 00229 deletes the kept answers of the requests whose answer carries a
// secret shown only once (an API key's create and rotate, a webhook
// endpoint's create and secret rotation, a cost dashboard token's rotation,
// a card setup session), and keeps every other kept answer, near misses
// included. It runs as an owner without BYPASSRLS, as a least-privilege
// install does, so the file's own bypass is what lets it see the rows.
func TestDeleteKeptShowOnceAnswers(t *testing.T) {
	const name = "hardcount_migrate_show_once_answers"
	_, db := scratchDB(t, name)
	ownerDSN := leastPrivilegeOwner(t, name)
	ctx := context.Background()
	if err := up(ctx, ownerDSN, filesBefore(t, deleteKeptShowOnceAnswersVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", deleteKeptShowOnceAnswersVersion, err)
	}
	exec := func(livemode, q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', $1, true)`, livemode); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", oneLine(q), err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	exec("on", `INSERT INTO tenants (id, name) VALUES ('ten_ak', 'ten_ak')`)
	answers := []struct {
		key, livemode, method, path string
		kept                        bool
	}{
		{"key-create-live", "on", "POST", "/v1/api-keys", false},
		{"key-create-test", "off", "POST", "/v1/api-keys", false},
		{"key-create-slash", "off", "POST", "/v1/api-keys/", false},
		{"key-rotate", "off", "POST", "/v1/api-keys/hc_key_a/rotate", false},
		{"endpoint-create", "off", "POST", "/v1/webhook-endpoints/endpoints", false},
		{"endpoint-rotate", "off", "POST", "/v1/webhook-endpoints/endpoints/hc_whe_a/rotate-secret", false},
		{"dashboard-rotate", "off", "POST", "/v1/customers/hc_cus_a/rotate-cost-dashboard-token", false},
		{"setup-session", "off", "POST", "/v1/customers/hc_cus_a/payment-methods/setup-session", false},
		// Near misses, kept.
		{"customer-create", "off", "POST", "/v1/customers", true},
		{"endpoint-test", "off", "POST", "/v1/webhook-endpoints/endpoints/hc_whe_a/test", true},
		{"key-rotate-extra", "off", "POST", "/v1/api-keys/hc_key_a/rotate/extra", true},
		{"key-create-patch", "off", "PATCH", "/v1/api-keys", true},
		{"payment-methods", "off", "POST", "/v1/customers/hc_cus_a/payment-methods", true},
	}
	for _, a := range answers {
		exec(a.livemode, `INSERT INTO idempotency_keys (tenant_id, key, http_method, http_path, status_code, response_body)
			VALUES ('ten_ak', $1, $2, $3, 201, convert_to('{"raw_key":"hc_secret_test_0000"}', 'UTF8'))`, a.key, a.method, a.path)
	}

	if err := up(ctx, ownerDSN, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, a := range answers {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM idempotency_keys WHERE key = $1`, a.key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if got := n == 1; got != a.kept {
			t.Errorf("%s (%s %s): kept %v after the migration, want %v", a.key, a.method, a.path, got, a.kept)
		}
	}
}
