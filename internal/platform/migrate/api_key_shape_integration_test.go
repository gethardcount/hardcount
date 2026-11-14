package migrate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// apiKeyShapeVersion is 00230_api_key_shape.sql; 00231 validates its CHECKs.
const (
	apiKeyShapeVersion         = 230
	apiKeyShapeValidateVersion = 231
)

var (
	shapeHash = strings.Repeat("ab", 32)
	shapeSalt = strings.Repeat("cd", 16)
)

// shapeRow inserts one key of ten_ak with the given columns.
func shapeRow(id, prefix, hash, salt, keyType, name string) string {
	return `INSERT INTO api_keys (id, key_prefix, key_hash, key_salt, key_type, name, tenant_id)
		VALUES ('` + id + `', '` + prefix + `', '` + hash + `', '` + salt + `', '` + keyType + `', '` + name + `', 'ten_ak')`
}

// refusedBy runs q in a live-mode transaction that is rolled back and
// returns the constraint that refused the row, or "" when it went in.
func refusedBy(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	err := withBypass(context.Background(), db, func(tx *sql.Tx) error {
		_, err := tx.Exec(q)
		return err
	})
	if err == nil {
		return ""
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.ConstraintName == "" {
		t.Fatalf("%s: %v, want a constraint violation or success", q, err)
	}
	return pg.ConstraintName
}

// TestAPIKeyShape_UpAndDown: after 00230 and 00231 api_keys takes a key in
// Hardcount's shape, salted or not, and refuses each other shape with the
// constraint that states it; the four CHECKs are validated, and key_type and
// livemode have no default. The Down takes the shape away and puts back the
// defaults and the old lookup index, and Up runs again.
func TestAPIKeyShape_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_api_key_shape")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	kept := shapeRow("hc_key_kept", "hc_secret_live_000000000000", shapeHash, shapeSalt, "secret", "Kept")
	if err := deadContractExec(ctx, db, apiKeySeedTenant+";\n"+kept); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct{ name, q, refusedBy string }{
		{"a salted live secret key", shapeRow("hc_key_a", "hc_secret_live_111111111111", shapeHash, shapeSalt, "secret", "A"), ""},
		{"an unsalted live ingest key", shapeRow("hc_key_b", "hc_ingest_live_222222222222", shapeHash, "", "ingest", "B"), ""},
		{"no mode in the prefix", shapeRow("hc_key_c", "hc_secret_333333333333", shapeHash, shapeSalt, "secret", "C"), "api_keys_prefix_shape_check"},
		{"a test prefix on a live row", shapeRow("hc_key_d", "hc_secret_test_444444444444", shapeHash, shapeSalt, "secret", "D"), "api_keys_prefix_shape_check"},
		{"an ingest prefix on a secret row", shapeRow("hc_key_e", "hc_ingest_live_555555555555", shapeHash, shapeSalt, "secret", "E"), "api_keys_prefix_shape_check"},
		{"uppercase hex in the prefix", shapeRow("hc_key_f", "hc_secret_live_ABCDEF012345", shapeHash, shapeSalt, "secret", "F"), "api_keys_prefix_shape_check"},
		{"a hash that is not 64 hex", shapeRow("hc_key_g", "hc_secret_live_666666666666", shapeHash[1:], shapeSalt, "secret", "G"), "api_keys_hash_shape_check"},
		{"a salt that is not 32 hex", shapeRow("hc_key_h", "hc_secret_live_777777777777", shapeHash, shapeSalt[1:], "secret", "H"), "api_keys_salt_shape_check"},
		{"a blank name", shapeRow("hc_key_i", "hc_secret_live_888888888888", shapeHash, shapeSalt, "secret", "  "), "api_keys_name_check"},
		{"a prefix another key has", shapeRow("hc_key_j", "hc_secret_live_000000000000", shapeHash, shapeSalt, "secret", "J"), "api_keys_key_prefix_key"},
	}
	for _, c := range cases {
		if got := refusedBy(t, db, c.q); got != c.refusedBy {
			t.Errorf("after 00231, %s: refused by %q, want %q", c.name, got, c.refusedBy)
		}
	}
	for _, c := range []string{"api_keys_prefix_shape_check", "api_keys_hash_shape_check", "api_keys_salt_shape_check", "api_keys_name_check"} {
		var validated bool
		if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = $1`, c).Scan(&validated); err != nil || !validated {
			t.Errorf("%s validated = %v (%v), want true", c, validated, err)
		}
	}
	for _, col := range []string{"key_type", "livemode"} {
		if d := columnDefault(t, db, "api_keys", col); d != "" {
			t.Errorf("api_keys.%s has the default %q, want none", col, d)
		}
	}
	if indexExists(t, db, "idx_api_keys_prefix") || !indexExists(t, db, "api_keys_key_prefix_key") {
		t.Error("00230 did not replace idx_api_keys_prefix with the unique api_keys_key_prefix_key")
	}

	if _, err := down(ctx, dsn, migrationFiles(), apiKeyShapeVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", apiKeyShapeVersion-1, err)
	}
	for _, c := range cases {
		if got := refusedBy(t, db, c.q); got != "" {
			t.Errorf("after the Down, %s: refused by %q, want accepted", c.name, got)
		}
	}
	for _, col := range []string{"key_type", "livemode"} {
		if columnDefault(t, db, "api_keys", col) == "" {
			t.Errorf("after the Down, api_keys.%s has no default", col)
		}
	}
	if !indexExists(t, db, "idx_api_keys_prefix") {
		t.Error("the Down did not put back idx_api_keys_prefix")
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	if got := refusedBy(t, db, cases[2].q); got != "api_keys_prefix_shape_check" {
		t.Errorf("after the second Up, no mode in the prefix: refused by %q", got)
	}
}

// TestAPIKeyShape_RefusesKeysOfAnotherShape: every key Hardcount wrote has
// the shape, so a key that does not was written by hand. 00231 stops before
// validating anything, counts the rows, and points at the runbook section,
// which must exist. It never deletes a key. Once the rows are gone, it runs.
func TestAPIKeyShape_RefusesKeysOfAnotherShape(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_api_key_shape_refuse")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, apiKeyShapeVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", apiKeyShapeVersion, err)
	}
	if err := deadContractExec(ctx, db, apiKeySeedTenant+`;
		`+shapeRow("hc_key_good", "hc_secret_live_000000000000", shapeHash, shapeSalt, "secret", "Good")+`;
		`+shapeRow("hc_key_nomode", "hc_secret_111111111111", shapeHash, shapeSalt, "secret", "No mode")+`;
		`+shapeRow("hc_key_blank", "hc_secret_live_222222222222", shapeHash, "", "secret", " ")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("the Up succeeded over two keys of another shape")
	}
	const section = "docs/ops/runbook/migration-failed-midway.md#00231-found-api-keys-of-another-shape"
	for _, w := range []string{"migration 00231", "2 key(s)", section} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	assertRunbookSectionExists(t, err.Error())
	if gooseRows(t, db, apiKeyShapeValidateVersion) != 0 {
		t.Error("the refused Up recorded 00231")
	}
	var n int
	if err := withBypass(ctx, db, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM api_keys`).Scan(&n)
	}); err != nil || n != 3 {
		t.Errorf("keys after the refused Up: %d (%v), want all 3 kept", n, err)
	}

	if err := deadContractExec(ctx, db, `DELETE FROM api_keys WHERE id IN ('hc_key_nomode', 'hc_key_blank')`); err != nil {
		t.Fatalf("delete the keys of another shape: %v", err)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up after those keys are gone: %v", err)
	}
	var validated bool
	if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = 'api_keys_prefix_shape_check'`).Scan(&validated); err != nil || !validated {
		t.Errorf("api_keys_prefix_shape_check validated = %v (%v), want true", validated, err)
	}
}

// TestAPIKeyShape_RefusesSharedPrefixes: Hardcount draws each prefix at
// random, so two keys with one prefix were written by hand. 00230 stops
// before changing anything and points at the runbook section; it does not
// choose which key to delete. Once one of them is gone, it runs.
func TestAPIKeyShape_RefusesSharedPrefixes(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_api_key_shape_shared")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, apiKeyShapeVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", apiKeyShapeVersion, err)
	}
	if err := deadContractExec(ctx, db, apiKeySeedTenant+`;
		`+shapeRow("hc_key_one", "hc_secret_live_000000000000", shapeHash, shapeSalt, "secret", "One")+`;
		`+shapeRow("hc_key_two", "hc_secret_live_000000000000", shapeHash, shapeSalt, "secret", "Two")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("the Up succeeded over two keys that share a prefix")
	}
	const section = "docs/ops/runbook/migration-failed-midway.md#00230-found-api-keys-that-share-a-prefix"
	for _, w := range []string{"migration 00230", "1 prefix(es)", section} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not say %q", err, w)
		}
	}
	assertRunbookSectionExists(t, err.Error())
	if gooseRows(t, db, apiKeyShapeVersion) != 0 {
		t.Error("the refused Up recorded 00230")
	}
	if !indexExists(t, db, "idx_api_keys_prefix") {
		t.Error("the refused Up changed the lookup index")
	}

	if err := deadContractExec(ctx, db, `DELETE FROM api_keys WHERE id = 'hc_key_two'`); err != nil {
		t.Fatalf("delete one of the keys: %v", err)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up after the shared prefix is gone: %v", err)
	}
}
