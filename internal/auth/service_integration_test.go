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

// Publishable keys were removed before v0.9.0: the type is refused on
// create, and a key with the old hc_pub_ prefix is an unknown key.
func TestCreateKey_RefusesRemovedPublishableType(t *testing.T) {
	k := newKeyRig(t)
	_, err := k.svc.CreateKey(liveCtx(t), k.tenantID, auth.CreateKeyInput{Name: "Frontend Key", KeyType: auth.KeyType("publishable")})
	wantValidation(t, err, "key_type")
}

func TestValidateKey_OldPublishablePrefixIsInvalid(t *testing.T) {
	k := newKeyRig(t)
	for _, raw := range []string{"hc_pub_live_0123456789abcdef0123", "hc_pub_test_0123456789abcdef0123"} {
		_, err := k.svc.ValidateKey(t.Context(), raw)
		wantInvalidKey(t, err, raw)
	}
}

// An ingest key (ADR-133) is issued like a secret key, in the caller's
// mode, with its own prefix, and validates back to its type.
func TestCreateKey_Ingest(t *testing.T) {
	k := newKeyRig(t)
	for _, live := range []bool{true, false} {
		ctx := auth.WithKeyType(postgres.WithLivemode(t.Context(), live), auth.KeyTypeSession)
		result := k.create(t, ctx, auth.CreateKeyInput{Name: "LiteLLM proxy", KeyType: auth.KeyTypeIngest})
		want := "hc_ingest_test_"
		if live {
			want = "hc_ingest_live_"
		}
		if !strings.HasPrefix(result.RawKey, want) || !strings.HasPrefix(result.Key.KeyPrefix, want) {
			t.Errorf("live=%v: raw key %q, prefix %q, want both to start with %s", live, result.RawKey[:len(want)], result.Key.KeyPrefix, want)
		}
		if result.Key.KeyType != "ingest" || result.Key.Livemode != live {
			t.Errorf("live=%v: stored key_type %q livemode %v, want ingest in the caller's mode", live, result.Key.KeyType, result.Key.Livemode)
		}
		got, err := k.svc.ValidateKey(ctx, result.RawKey)
		if err != nil {
			t.Fatalf("validate the ingest key: %v", err)
		}
		if got.KeyType != "ingest" || got.ID != result.Key.ID {
			t.Errorf("validated %+v, want the ingest key %s", got, result.Key.ID)
		}
	}
}

// Platform keys and /v1/tenants are gone (ADR-133): the type is refused on
// create from every caller, and a raw key with the old hc_platform_ prefix is
// an unknown key. No platform row can exist to match it: the table's CHECK
// allows only secret and ingest.
func TestPlatformKeyType_RefusedAtCreateAndParse(t *testing.T) {
	k := newKeyRig(t)
	for name, ctx := range map[string]context.Context{
		"no key type":            liveCtx(t),
		"secret key":             auth.WithKeyType(liveCtx(t), auth.KeyTypeSecret),
		"session":                auth.WithKeyType(liveCtx(t), auth.KeyTypeSession),
		"old platform principal": auth.WithKeyType(liveCtx(t), auth.KeyType("platform")),
	} {
		_, err := k.svc.CreateKey(ctx, k.tenantID, auth.CreateKeyInput{Name: "Platform", KeyType: auth.KeyType("platform")})
		if !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: create with key_type platform: got %v, want a validation error", name, err)
		}
	}
	var stored int
	k.bypass(t, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM api_keys WHERE tenant_id = $1`, k.tenantID).Scan(&stored)
	})
	if stored != 0 {
		t.Fatalf("a refused create stored %d keys", stored)
	}

	_, err := k.svc.ValidateKey(t.Context(), "hc_platform_live_"+strings.Repeat("ab", 32))
	wantInvalidKey(t, err, "hc_platform_live_…")
	if p := auth.KeyType("platform").TypePrefix(); p != "" {
		t.Errorf("platform TypePrefix = %q, want none", p)
	}
}

func TestCreateKey_DefaultsToSecret(t *testing.T) {
	k := newKeyRig(t)
	if got := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Default"}).Key.KeyType; got != "secret" {
		t.Errorf("default key_type: got %q, want secret", got)
	}
}

func TestCreateKey_InvalidType(t *testing.T) {
	k := newKeyRig(t)
	_, err := k.svc.CreateKey(liveCtx(t), k.tenantID, auth.CreateKeyInput{Name: "Bad", KeyType: "admin"})
	wantValidation(t, err, "key_type")
}

func TestCreateKey_MissingName(t *testing.T) {
	k := newKeyRig(t)
	_, err := k.svc.CreateKey(liveCtx(t), k.tenantID, auth.CreateKeyInput{})
	wantValidation(t, err, "name")
}

// A key cannot be created already expired: expires_at must be after now.
func TestCreateKey_PastExpiresAt(t *testing.T) {
	k := newKeyRig(t)
	past, now := time.Now().Add(-24*time.Hour), time.Now()
	for name, at := range map[string]*time.Time{"past expires_at": &past, "expires_at == now": &now} {
		_, err := k.svc.CreateKey(liveCtx(t), k.tenantID, auth.CreateKeyInput{Name: name, ExpiresAt: at})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "expires_at" {
			t.Errorf("%s: got %v, want a validation error on expires_at", name, err)
		}
	}
	future := time.Now().Add(30 * 24 * time.Hour)
	k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "future key", ExpiresAt: &future})
	k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "no expiry"})
}

func TestValidateKey_AllTypes(t *testing.T) {
	k := newKeyRig(t)
	for _, kt := range auth.StoredKeyTypes {
		t.Run(string(kt), func(t *testing.T) {
			result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Test " + string(kt), KeyType: kt})
			key, err := k.svc.ValidateKey(liveCtx(t), result.RawKey)
			if err != nil {
				t.Fatalf("validate %s key: %v", kt, err)
			}
			if key.TenantID != k.tenantID || key.KeyType != string(kt) {
				t.Errorf("validated tenant %q, key_type %q; want %s, %s", key.TenantID, key.KeyType, k.tenantID, kt)
			}
		})
	}
}

func TestValidateKey_InvalidFormat(t *testing.T) {
	k := newKeyRig(t)
	for _, raw := range []string{"", "sk_live_invalid", "hc_secret_tooshort", "hc_unknown_" + strings.Repeat("ab", 32)} {
		_, err := k.svc.ValidateKey(t.Context(), raw)
		wantInvalidKey(t, err, raw)
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

func TestValidateKey_Revoked(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Revokable", KeyType: auth.KeyTypeSecret})
	if _, err := k.svc.RevokeKey(liveCtx(t), k.tenantID, result.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err := k.svc.ValidateKey(liveCtx(t), result.RawKey)
	wantInvalidKey(t, err, "a revoked key")
}

func TestCreateKey_TestModeFromCtx(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, testCtx(t), auth.CreateKeyInput{Name: "Sandbox", KeyType: auth.KeyTypeSecret})
	if !strings.HasPrefix(result.RawKey, "hc_secret_test_") {
		t.Errorf("test-mode key should start with hc_secret_test_, got %q", result.RawKey[:20])
	}
	if result.Key.Livemode {
		t.Error("key should be test-mode")
	}
	// ValidateKey needs no mode: the stored key gives it.
	key, err := k.svc.ValidateKey(t.Context(), result.RawKey)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if key.Livemode {
		t.Error("validated key should be test-mode")
	}
}

// A key in the older format, with no mode infix (hc_secret_<hex>), validates
// and is live.
func TestValidateKey_LegacyFormat(t *testing.T) {
	k := newKeyRig(t)
	rawKey := "hc_secret_" + strings.Repeat("ab", 32)
	salt := []byte("legacytestsalt16")
	hash := sha256.Sum256(append(salt, []byte(rawKey)...))
	// The row as the older minter wrote it.
	k.bypass(t, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`SELECT set_config('app.livemode', 'on', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO api_keys (id, key_prefix, key_hash, key_salt, key_type, name, tenant_id)
			VALUES ('hc_key_legacy', $1, $2, $3, 'secret', 'Legacy', $4)`,
			"hc_secret_"+strings.Repeat("ab", 6), hex.EncodeToString(hash[:]), hex.EncodeToString(salt), k.tenantID)
		return err
	})
	key, err := k.svc.ValidateKey(t.Context(), rawKey)
	if err != nil {
		t.Fatalf("legacy key validate: %v", err)
	}
	if !key.Livemode {
		t.Error("legacy key should be classified as live")
	}
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

// The default rotation: the old key stops validating as soon as rotate
// returns, the new key validates, and the new key has the old one's type,
// name, mode and expires_at.
func TestRotateKey_ImmediateRevoke(t *testing.T) {
	k := newKeyRig(t)
	expiry := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Second)
	original := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Prod deploy", KeyType: auth.KeyTypeSecret, ExpiresAt: &expiry})

	rotated, err := k.svc.RotateKey(liveCtx(t), k.tenantID, original.Key.ID, auth.RotateKeyInput{})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.NewKey.ID == original.Key.ID {
		t.Fatal("new key must have a distinct ID")
	}
	if rotated.NewKey.Name != original.Key.Name || rotated.NewKey.KeyType != original.Key.KeyType || rotated.NewKey.Livemode != original.Key.Livemode {
		t.Errorf("new key name %q, type %q, livemode %v; want the old key's %q, %q, %v",
			rotated.NewKey.Name, rotated.NewKey.KeyType, rotated.NewKey.Livemode, original.Key.Name, original.Key.KeyType, original.Key.Livemode)
	}
	if rotated.NewKey.ExpiresAt == nil || !rotated.NewKey.ExpiresAt.Equal(expiry) {
		t.Errorf("new key expires_at: got %v, want %v", rotated.NewKey.ExpiresAt, expiry)
	}
	if rotated.OldKey.RevokedAt == nil {
		t.Fatal("old key should be revoked after immediate rotation")
	}
	if rotated.RawKey == "" || rotated.RawKey == original.RawKey {
		t.Fatal("rotation must return a new raw key, once")
	}

	_, err = k.svc.ValidateKey(liveCtx(t), original.RawKey)
	wantInvalidKey(t, err, "the old key after rotation")
	valid, err := k.svc.ValidateKey(liveCtx(t), rotated.RawKey)
	if err != nil {
		t.Fatalf("new key validation: %v", err)
	}
	if valid.ID != rotated.NewKey.ID {
		t.Errorf("new key ID mismatch: got %q, want %q", valid.ID, rotated.NewKey.ID)
	}
}

// A rotation with a grace period: the old key stays valid for the window and
// the new key is valid at once, so both authenticate inside it.
func TestRotateKey_WithGracePeriod(t *testing.T) {
	k := newKeyRig(t)
	original := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "App", KeyType: auth.KeyTypeSecret})

	rotated, err := k.svc.RotateKey(liveCtx(t), k.tenantID, original.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.OldKey.RevokedAt != nil {
		t.Fatal("grace-period rotation must not set revoked_at on the old key")
	}
	if rotated.OldKey.ExpiresAt == nil {
		t.Fatal("grace-period rotation must set expires_at on the old key")
	}
	if until := time.Until(*rotated.OldKey.ExpiresAt); until < 50*time.Minute || until > 70*time.Minute {
		t.Errorf("old key expires_at should be ~1 hour out, got %v", until)
	}
	if _, err := k.svc.ValidateKey(liveCtx(t), original.RawKey); err != nil {
		t.Errorf("old key should still validate in grace window: %v", err)
	}
	if _, err := k.svc.ValidateKey(liveCtx(t), rotated.RawKey); err != nil {
		t.Errorf("new key should validate immediately: %v", err)
	}
}

func TestRotateKey_RejectsNegativeGrace(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "K", KeyType: auth.KeyTypeSecret})
	_, err := k.svc.RotateKey(liveCtx(t), k.tenantID, result.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: -1})
	wantValidation(t, err, "expires_in_seconds")
}

func TestRotateKey_RejectsExcessiveGrace(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "K", KeyType: auth.KeyTypeSecret})
	_, err := k.svc.RotateKey(liveCtx(t), k.tenantID, result.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: auth.MaxRotationGraceSeconds + 1})
	wantValidation(t, err, "expires_in_seconds")
}

func TestRotateKey_RejectsAlreadyRevoked(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "K", KeyType: auth.KeyTypeSecret})
	if _, err := k.svc.RevokeKey(liveCtx(t), k.tenantID, result.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := k.svc.RotateKey(liveCtx(t), k.tenantID, result.Key.ID, auth.RotateKeyInput{}); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("rotate a revoked key: got %v, want invalid state", err)
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
