package migrate

import (
	"context"
	"testing"
)

const deleteStoredAPIKeyAnswersVersion = 229

// 00229 deletes the kept answers of the requests that issue an API key, a
// create and a rotate, which held the key's secret, and keeps every other
// kept answer. It runs as an owner without BYPASSRLS, as a least-privilege
// install does, so the file's own bypass is what lets it see the rows.
func TestDeleteStoredAPIKeyAnswers(t *testing.T) {
	const name = "hardcount_migrate_api_key_answers"
	_, db := scratchDB(t, name)
	ownerDSN := leastPrivilegeOwner(t, name)
	ctx := context.Background()
	if err := up(ctx, ownerDSN, filesBefore(t, deleteStoredAPIKeyAnswersVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", deleteStoredAPIKeyAnswersVersion, err)
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
		key, livemode, path string
		kept                bool
	}{
		{"create-live", "on", "/v1/api-keys", false},
		{"create-test", "off", "/v1/api-keys", false},
		{"rotate", "off", "/v1/api-keys/hc_key_a/rotate", false},
		{"customer", "off", "/v1/customers", true},
	}
	for _, a := range answers {
		exec(a.livemode, `INSERT INTO idempotency_keys (tenant_id, key, http_method, http_path, status_code, response_body)
			VALUES ('ten_ak', $1, 'POST', $2, 201, convert_to('{"raw_key":"hc_secret_test_0000"}', 'UTF8'))`, a.key, a.path)
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
			t.Errorf("%s (%s): kept %v after the migration, want %v", a.key, a.path, got, a.kept)
		}
	}
}
