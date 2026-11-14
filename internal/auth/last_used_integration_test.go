package auth

import (
	"database/sql"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A request that authenticates with a key has written the key's
// last_used_at by the time ValidateKey returns: the write is part of the
// request, not left to run after it.
func TestValidateKey_WritesLastUsedBeforeItReturns(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := NewService(NewPostgresStore(db))
	ctx := postgres.WithLivemode(t.Context(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Last Used")
	created, err := svc.CreateKey(ctx, tenantID, CreateKeyInput{Name: "CI"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := svc.ValidateKey(t.Context(), created.RawKey); err != nil {
		t.Fatalf("validate: %v", err)
	}

	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var used sql.NullTime
	if err := tx.QueryRow(`SELECT last_used_at FROM api_keys WHERE id = $1`, created.Key.ID).Scan(&used); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if !used.Valid {
		t.Error("last_used_at is not written when ValidateKey returns")
	}
}
