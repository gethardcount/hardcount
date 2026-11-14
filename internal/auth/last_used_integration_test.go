package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
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

// A failed last_used_at write does not refuse the key: no auth decision reads
// the column, so ValidateKey logs the failure and returns the key.
//
// The failure is real. Another transaction holds the key's row lock, so the
// lookup (a plain read, which takes no lock) succeeds while the UPDATE waits on
// the lock until the request's context ends: the write fails because the
// request's time ran out, as it does under lock contention in production.
func TestValidateKey_AFailedLastUsedWriteDoesNotRefuseTheKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := NewService(NewPostgresStore(db))
	ctx := postgres.WithLivemode(t.Context(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Last Used Fails")
	created, err := svc.CreateKey(ctx, tenantID, CreateKeyInput{Name: "CI"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	locker, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	defer postgres.Rollback(locker)
	if _, err := locker.Exec(`SELECT 1 FROM api_keys WHERE id = $1 FOR UPDATE`, created.Key.ID); err != nil {
		t.Fatalf("lock the key's row: %v", err)
	}

	type result struct {
		key domain.APIKey
		err error
	}
	done := make(chan result, 1)
	go func() {
		reqCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		key, err := svc.ValidateKey(reqCtx, created.RawKey)
		done <- result{key, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ValidateKey did not return in 15s, though its context ends after 2s: the write waits past the request")
	}
	if got.err != nil {
		t.Fatalf("ValidateKey refused the key when its last_used_at write failed: %v", got.err)
	}
	if got.key.ID != created.Key.ID {
		t.Fatalf("ValidateKey returned key %q, want %q", got.key.ID, created.Key.ID)
	}

	// The write did fail: the column is still empty once the lock is gone.
	postgres.Rollback(locker)
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var used sql.NullTime
	if err := tx.QueryRow(`SELECT last_used_at FROM api_keys WHERE id = $1`, created.Key.ID).Scan(&used); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if used.Valid {
		t.Fatal("last_used_at was written: the test did not make the write fail")
	}
}
