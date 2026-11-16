package auth_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// These tests run the auth.Service on the real api_keys table, so its row-level
// security (tenant and mode), its livemode trigger and its constraints apply
// as they do in production.

type keyRig struct {
	svc      *auth.Service
	db       *postgres.DB
	tenantID string
}

func newKeyRig(t *testing.T) keyRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	return keyRig{svc: auth.NewService(db, audit.NewLogger(db)), db: db, tenantID: testutil.CreateTestTenant(t, db, "API Keys")}
}

func liveCtx(t *testing.T) context.Context { return postgres.WithLivemode(t.Context(), true) }
func testCtx(t *testing.T) context.Context { return postgres.WithLivemode(t.Context(), false) }

func (k keyRig) create(t *testing.T, ctx context.Context, in auth.CreateKeyInput) auth.CreateKeyResult {
	t.Helper()
	res, err := k.svc.CreateKey(ctx, k.tenantID, in)
	if err != nil {
		t.Fatalf("create key %q: %v", in.Name, err)
	}
	return res
}

// bypass runs fn in a transaction outside row-level security, to read a
// stored column or to make a row state the API cannot make.
func (k keyRig) bypass(t *testing.T, fn func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := k.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := fn(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func wantValidation(t *testing.T, err error, field string) {
	t.Helper()
	if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != field {
		t.Fatalf("got %v, want a validation error on %s", err, field)
	}
}

func wantInvalidKey(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatalf("%s: got %v, want auth.ErrInvalidKey", what, err)
	}
}

func TestCreateKey_Secret(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Backend Key", KeyType: auth.KeyTypeSecret})

	if !strings.HasPrefix(result.RawKey, "hc_secret_live_") {
		t.Errorf("raw key should start with hc_secret_live_, got prefix: %q", result.RawKey[:16])
	}
	if len(result.RawKey) != len("hc_secret_live_")+64 {
		t.Errorf("raw key length: got %d, want %d", len(result.RawKey), len("hc_secret_live_")+64)
	}
	if !result.Key.Livemode || result.Key.KeyType != "secret" || result.Key.TenantID != k.tenantID {
		t.Errorf("stored key: livemode %v, key_type %q, tenant %q; want a live secret key of %s",
			result.Key.Livemode, result.Key.KeyType, result.Key.TenantID, k.tenantID)
	}

	// The stored hash is SHA-256(salt + raw key).
	var keyHash, keySalt string
	k.bypass(t, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT key_hash, key_salt FROM api_keys WHERE id = $1`, result.Key.ID).Scan(&keyHash, &keySalt)
	})
	salt, err := hex.DecodeString(keySalt)
	if err != nil || len(salt) == 0 {
		t.Fatalf("stored salt %q: want hex bytes (%v)", keySalt, err)
	}
	hash := sha256.Sum256(append(salt, []byte(result.RawKey)...))
	if keyHash != hex.EncodeToString(hash[:]) {
		t.Error("the stored hash is not SHA-256(salt + raw key)")
	}
}

// The create input rules: no key_type means secret, a key_type that is not a
// stored type and a missing name are refused, and expires_at must be after
// now.
func TestCreateKey_InputRules(t *testing.T) {
	k := newKeyRig(t)
	past, now, future := time.Now().Add(-24*time.Hour), time.Now(), time.Now().Add(30*24*time.Hour)
	for _, c := range []struct {
		name      string
		in        auth.CreateKeyInput
		wantField string // the field of the validation error; "" when the create succeeds
	}{
		{"no key_type", auth.CreateKeyInput{Name: "Default"}, ""},
		{"an unknown key_type", auth.CreateKeyInput{Name: "Bad", KeyType: "admin"}, "key_type"},
		{"no name", auth.CreateKeyInput{}, "name"},
		{"expires_at in the past", auth.CreateKeyInput{Name: "Past", ExpiresAt: &past}, "expires_at"},
		{"expires_at now", auth.CreateKeyInput{Name: "Now", ExpiresAt: &now}, "expires_at"},
		{"expires_at in the future", auth.CreateKeyInput{Name: "Future", ExpiresAt: &future}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := k.svc.CreateKey(liveCtx(t), k.tenantID, c.in)
			if c.wantField != "" {
				wantValidation(t, err, c.wantField)
				return
			}
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if res.Key.KeyType != "secret" {
				t.Errorf("key_type = %q, want secret", res.Key.KeyType)
			}
		})
	}
}

func TestValidateKey_WrongSecret(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Real", KeyType: auth.KeyTypeSecret})
	// The same lookup prefix with a different secret.
	fake := result.Key.KeyPrefix + strings.Repeat("f", len(result.RawKey)-len(result.Key.KeyPrefix))
	if fake == result.RawKey {
		t.Skip("astronomically unlikely collision")
	}
	_, err := k.svc.ValidateKey(liveCtx(t), fake)
	wantInvalidKey(t, err, "the key's prefix with another secret")
}

// A string that is not a key is refused before any lookup: one of the four
// type-and-mode prefixes, then exactly 64 lowercase hex characters. The
// service's store is down, so a refusal that came from a lookup would be an
// outage error, not ErrInvalidKey; a well-formed key does reach the store.
func TestValidateKey_RefusesAStringThatIsNotAKeyBeforeAnyLookup(t *testing.T) {
	down := testutil.OpenTracedDB(t, nil)
	_ = down.Pool.Close()
	keys := auth.NewService(down, audit.NewLogger(down))
	secret := strings.Repeat("0123456789abcdef", 4)

	for name, raw := range map[string]string{
		"no mode (the older format)":   "hc_secret_" + secret,
		"63 hex characters":            "hc_secret_live_" + secret[1:],
		"65 hex characters":            "hc_secret_live_" + secret + "a",
		"uppercase hex":                "hc_ingest_test_" + strings.ToUpper(secret),
		"a character that is not hex":  "hc_secret_test_" + secret[:63] + "g",
		"the removed platform type":    "hc_platform_live_" + secret,
		"the removed publishable type": "hc_pub_live_" + secret,
	} {
		if _, err := keys.ValidateKey(t.Context(), raw); !errors.Is(err, auth.ErrInvalidKey) {
			t.Errorf("%s: got %v, want ErrInvalidKey without a lookup", name, err)
		}
	}
	for _, raw := range []string{"hc_secret_live_" + secret, "hc_ingest_test_" + secret} {
		if _, err := keys.ValidateKey(t.Context(), raw); err == nil || errors.Is(err, auth.ErrInvalidKey) {
			t.Errorf("%s…: got %v, want the lookup's outage error", raw[:15], err)
		}
	}
}

// A key the setup command wrote in v0.9.3 or earlier has an empty salt and a
// hash of SHA-256(key); it still validates, as a live key.
func TestValidateKey_AnUnsaltedSetupKeyStillValidates(t *testing.T) {
	k := newKeyRig(t)
	raw := "hc_secret_live_" + strings.Repeat("5e7a", 16)
	sum := sha256.Sum256([]byte(raw))
	// The row as the v0.9.3 setup command wrote it: no salt, mode from the
	// session setting.
	k.bypass(t, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`SELECT set_config('app.livemode', 'on', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id)
			VALUES ('hc_key_setup093', $1, $2, 'secret', 'Bootstrap Key (Live)', $3)`,
			raw[:len("hc_secret_live_")+12], hex.EncodeToString(sum[:]), k.tenantID)
		return err
	})
	key, err := k.svc.ValidateKey(t.Context(), raw)
	if err != nil {
		t.Fatalf("validate an unsalted setup key: %v", err)
	}
	if key.ID != "hc_key_setup093" || !key.Livemode {
		t.Errorf("validated %s (live=%v), want hc_key_setup093, live", key.ID, key.Livemode)
	}
}

// A key whose expires_at has passed does not validate.
func TestValidateKey_Expired(t *testing.T) {
	k := newKeyRig(t)
	soon := time.Now().Add(time.Hour)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Expiring", KeyType: auth.KeyTypeSecret, ExpiresAt: &soon})
	// CreateKey refuses an expiry in the past, so the row is aged here.
	k.bypass(t, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, result.Key.ID)
		return err
	})
	_, err := k.svc.ValidateKey(liveCtx(t), result.RawKey)
	wantInvalidKey(t, err, "an expired key")
}

func TestRevokeKey_WrongTenant(t *testing.T) {
	k := newKeyRig(t)
	other := testutil.CreateTestTenant(t, k.db, "Other")
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Test", KeyType: auth.KeyTypeSecret})
	if _, err := k.svc.RevokeKey(liveCtx(t), other, result.Key.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("revoke from another tenant: got %v, want not found", err)
	}
	if _, err := k.svc.ValidateKey(liveCtx(t), result.RawKey); err != nil {
		t.Fatalf("the key stopped validating after another tenant's revoke: %v", err)
	}
}

// A rotation's grace period is 0 to MaxRotationGraceSeconds.
func TestRotateKey_RefusesAGraceOutOfRange(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "K", KeyType: auth.KeyTypeSecret})
	for name, grace := range map[string]int64{"negative": -1, "over the maximum": auth.MaxRotationGraceSeconds + 1} {
		t.Run(name, func(t *testing.T) {
			_, err := k.svc.RotateKey(liveCtx(t), k.tenantID, result.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: grace})
			wantValidation(t, err, "expires_in_seconds")
		})
	}
}

func TestRotateKey_WrongTenant(t *testing.T) {
	k := newKeyRig(t)
	other := testutil.CreateTestTenant(t, k.db, "Other")
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "K", KeyType: auth.KeyTypeSecret})
	if _, err := k.svc.RotateKey(liveCtx(t), other, result.Key.ID, auth.RotateKeyInput{}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("rotate from another tenant: got %v, want not found", err)
	}
}

// A key is rotated in its own mode, and its replacement is in that mode. From
// the other mode the key is not found: row-level security filters by mode.
func TestRotateKey_InTheKeysOwnMode(t *testing.T) {
	k := newKeyRig(t)
	original := k.create(t, testCtx(t), auth.CreateKeyInput{Name: "Sandbox", KeyType: auth.KeyTypeSecret})

	if _, err := k.svc.RotateKey(liveCtx(t), k.tenantID, original.Key.ID, auth.RotateKeyInput{}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("rotate a test-mode key in live mode: got %v, want not found", err)
	}
	rotated, err := k.svc.RotateKey(testCtx(t), k.tenantID, original.Key.ID, auth.RotateKeyInput{})
	if err != nil {
		t.Fatalf("rotate in test mode: %v", err)
	}
	if rotated.NewKey.Livemode || !strings.HasPrefix(rotated.RawKey, "hc_secret_test_") {
		t.Errorf("replacement livemode %v, raw key %q…; want a test-mode key", rotated.NewKey.Livemode, rotated.RawKey[:20])
	}
}

// A request that authenticates with a key has written the key's
// last_used_at by the time ValidateKey returns: the write is part of the
// request, not left to run after it.
func TestValidateKey_WritesLastUsedBeforeItReturns(t *testing.T) {
	db := testutil.SetupTestDB(t)
	svc := auth.NewService(db, audit.NewLogger(db))
	ctx := postgres.WithLivemode(t.Context(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Last Used")
	created, err := svc.CreateKey(ctx, tenantID, auth.CreateKeyInput{Name: "CI"})
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
	svc := auth.NewService(db, audit.NewLogger(db))
	ctx := postgres.WithLivemode(t.Context(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Last Used Fails")
	created, err := svc.CreateKey(ctx, tenantID, auth.CreateKeyInput{Name: "CI"})
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
