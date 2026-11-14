package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The Go parser (lookupPrefix) and the table's CHECK
// (api_keys_prefix_shape_check) state one shape for a key: each stored prefix
// below is accepted by both or refused by both. The parser reads the raw key,
// the prefix followed by the rest of a 64-character secret.
func TestKeyShape_ParserAndTableAgree(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Key Shape")
	rest := strings.Repeat("0", 2*secretBytes-lookupHexLen)

	for i, c := range []struct {
		stored string
		valid  bool
	}{
		{"hc_secret_live_0123456789ab", true},
		{"hc_secret_test_0123456789ab", true},
		{"hc_ingest_live_0123456789ab", true},
		{"hc_ingest_test_0123456789ab", true},
		{"hc_secret_0123456789ab", false},        // no mode
		{"hc_secret_prod_0123456789ab", false},   // not a mode
		{"hc_secret_live_0123456789a", false},    // 11 hex characters
		{"hc_secret_live_ABCDEF012345", false},   // uppercase
		{"hc_secret_live_0123456789ag", false},   // not hex
		{"hc_platform_live_0123456789ab", false}, // a removed type
		{"hc_pub_live_0123456789ab", false},      // a removed type
	} {
		parsed, parsedOK := lookupPrefix(c.stored + rest)
		if parsedOK && parsed != c.stored {
			t.Errorf("%s: the parser's lookup prefix is %q", c.stored, parsed)
		}
		if parsedOK != c.valid {
			t.Errorf("%s: the parser accepts = %v, want %v", c.stored, parsedOK, c.valid)
		}
		if got := tableAcceptsPrefix(t, db, tenantID, i, c.stored); got != c.valid {
			t.Errorf("%s: the table accepts = %v, want %v", c.stored, got, c.valid)
		}
	}
}

// tableAcceptsPrefix inserts a key with prefix, whose type and mode are read
// from the prefix as the CHECK reads them, in a transaction it rolls back. It
// reports whether the row went in, and fails the test on any refusal but the
// prefix CHECK's.
func tableAcceptsPrefix(t *testing.T, db *postgres.DB, tenantID string, i int, prefix string) bool {
	t.Helper()
	parts := strings.Split(prefix, "_")
	keyType, mode := "secret", "off"
	if len(parts) > 1 && parts[1] == "ingest" {
		keyType = "ingest"
	}
	if len(parts) > 2 && parts[2] == "live" {
		mode = "on"
	}
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`SELECT set_config('app.livemode', $1, true)`, mode); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id)
		VALUES ($1, $2, $3, $4, 'Shape', $5)`, "hc_key_shape_"+string(rune('a'+i)), prefix, strings.Repeat("ab", 32), keyType, tenantID)
	var pg *pgconn.PgError
	switch {
	case err == nil:
		return true
	case errors.As(err, &pg) && pg.ConstraintName == "api_keys_prefix_shape_check":
		return false
	default:
		t.Fatalf("insert %s: %v", prefix, err)
		return false
	}
}
