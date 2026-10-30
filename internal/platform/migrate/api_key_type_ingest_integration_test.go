package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// apiKeyTypeIngestVersion is 00221_api_key_type_ingest.sql (ADR-133).
const apiKeyTypeIngestVersion = 221

const apiKeySeedTenant = `INSERT INTO tenants (id, name) VALUES ('ten_ak', 'API key types')`

// apiKeyRow inserts one key of the given type for ten_ak.
func apiKeyRow(id, keyType string) string {
	return `INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id)
		VALUES ('` + id + `', 'hc_` + keyType + `_test_` + id + `', 'h_` + id + `', '` + keyType + `', 'Key', 'ten_ak')`
}

// keyTypeAccepted reports whether api_keys takes a row of keyType, in a
// transaction that is rolled back, so the probe leaves no row.
func keyTypeAccepted(t *testing.T, db *sql.DB, keyType string) bool {
	t.Helper()
	err := withBypass(context.Background(), db, func(tx *sql.Tx) error {
		_, err := tx.Exec(apiKeyRow("hc_key_probe", keyType))
		return err
	})
	if err != nil && !strings.Contains(err.Error(), "api_keys_key_type_check") {
		t.Fatalf("insert a %s key: %v", keyType, err)
	}
	return err == nil
}

// TestAPIKeyTypeIngest_UpAndDown: after 00221 api_keys takes secret and
// ingest keys and refuses platform (and publishable, removed in 00217), with
// a validated CHECK. The Down puts back the CHECK 00218 validated (secret
// and platform), and Up runs again.
//
// Mutation: an Up that keeps 'platform' in the CHECK, or leaves 'ingest'
// out, turns this red.
func TestAPIKeyTypeIngest_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_ak_ingest_migrate")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := deadContractExec(ctx, db, apiKeySeedTenant); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := map[string]bool{"secret": true, "ingest": true, "platform": false, "publishable": false}
	for kt, ok := range want {
		if got := keyTypeAccepted(t, db, kt); got != ok {
			t.Errorf("after 00221, a %s key accepted = %v, want %v", kt, got, ok)
		}
	}
	var validated bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = 'api_keys_key_type_check'`).Scan(&validated); err != nil || !validated {
		t.Errorf("api_keys_key_type_check validated = %v (%v), want true", validated, err)
	}

	if _, err := down(ctx, dsn, migrationFiles(), apiKeyTypeIngestVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", apiKeyTypeIngestVersion-1, err)
	}
	want = map[string]bool{"secret": true, "platform": true, "ingest": false, "publishable": false}
	for kt, ok := range want {
		if got := keyTypeAccepted(t, db, kt); got != ok {
			t.Errorf("after the Down, a %s key accepted = %v, want %v", kt, got, ok)
		}
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if !keyTypeAccepted(t, db, "ingest") || keyTypeAccepted(t, db, "platform") {
		t.Error("the second Up did not apply")
	}
}

// TestAPIKeyTypeIngest_RefusesPlatformKeys: no route ever issued a platform
// key, so a database that holds one had it written by hand. 00221 stops
// before changing anything, counts the rows, and points at the runbook
// section, which must exist. It never deletes a key. Once the rows are
// gone, it runs.
//
// Mutation: drop the count from the Up, and the upgrade either deletes
// nothing and fails on the bare CHECK (no count, no runbook link) or, with a
// DELETE, succeeds and loses the rows: both turn this red.
func TestAPIKeyTypeIngest_RefusesPlatformKeys(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_ak_ingest_migrate_refuse")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, apiKeyTypeIngestVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", apiKeyTypeIngestVersion, err)
	}
	if err := deadContractExec(ctx, db, apiKeySeedTenant+`;
		`+apiKeyRow("hc_key_plat_1", "platform")+`;
		`+apiKeyRow("hc_key_plat_2", "platform")+`;
		`+apiKeyRow("hc_key_sec", "secret")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("the Up succeeded over two platform keys")
	}
	const section = "docs/ops/runbook/migration-failed-midway.md#00221-found-platform-api-keys"
	for _, w := range []string{"migration 00221", "holds 2 platform key(s)", section} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	assertRunbookSectionExists(t, err.Error())
	if gooseRows(t, db, apiKeyTypeIngestVersion) != 0 {
		t.Error("the refused Up recorded 00221")
	}
	var n int
	if err := withBypass(ctx, db, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM api_keys WHERE key_type = 'platform'`).Scan(&n)
	}); err != nil || n != 2 {
		t.Errorf("platform keys after the refused Up: %d (%v), want both kept", n, err)
	}
	if keyTypeAccepted(t, db, "ingest") {
		t.Error("the refused Up left the new CHECK in place")
	}

	if err := deadContractExec(ctx, db, `DELETE FROM api_keys WHERE key_type = 'platform'`); err != nil {
		t.Fatalf("delete the platform keys: %v", err)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up after the platform keys are gone: %v", err)
	}
	if !keyTypeAccepted(t, db, "ingest") {
		t.Error("the Up did not apply once the platform keys were gone")
	}
}
