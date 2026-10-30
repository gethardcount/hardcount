package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

type memStore struct {
	mu   sync.RWMutex
	keys map[string]domain.APIKey
}

func newMemStore() *memStore {
	return &memStore{keys: make(map[string]domain.APIKey)}
}

func (m *memStore) Create(_ context.Context, key domain.APIKey) (domain.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key.CreatedAt = time.Now().UTC()
	m.keys[key.ID] = key
	return key, nil
}

func (m *memStore) Get(_ context.Context, tenantID, id string) (domain.APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID {
		return domain.APIKey{}, errs.ErrNotFound
	}
	return k, nil
}

func (m *memStore) ScheduleExpiry(_ context.Context, tenantID, id string, expiresAt time.Time) (domain.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.TenantID != tenantID || k.RevokedAt != nil {
		return domain.APIKey{}, errs.ErrNotFound
	}
	k.ExpiresAt = &expiresAt
	m.keys[id] = k
	return k, nil
}

func (m *memStore) GetByPrefix(_ context.Context, prefix string) (domain.APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, k := range m.keys {
		if k.KeyPrefix == prefix && k.RevokedAt == nil {
			return k, nil
		}
	}
	return domain.APIKey{}, errs.ErrNotFound
}

// Revoke flips revoked_at on the target key. Per ADR-011 the
// last-active-secret/platform safeguard is gone (sessions are
// user-bound, no tenant orphan path).
func (m *memStore) Revoke(_ context.Context, tenantID, id string) (domain.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.keys[id]
	if !ok || target.TenantID != tenantID || target.RevokedAt != nil {
		return domain.APIKey{}, errs.ErrNotFound
	}
	now := time.Now().UTC()
	target.RevokedAt = &now
	m.keys[id] = target
	return target, nil
}

func (m *memStore) List(_ context.Context, filter ListFilter) ([]domain.APIKey, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.APIKey
	for _, k := range m.keys {
		if k.TenantID == filter.TenantID {
			result = append(result, k)
		}
	}
	return result, len(result), nil
}

func (m *memStore) TouchLastUsed(_ context.Context, id string, usedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return errs.ErrNotFound
	}
	k.LastUsedAt = &usedAt
	m.keys[id] = k
	return nil
}

func TestCreateKey_Secret(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	result, err := svc.CreateKey(ctx, "tenant1", CreateKeyInput{
		Name:    "Backend Key",
		KeyType: KeyTypeSecret,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(result.RawKey, "hc_secret_live_") {
		t.Errorf("raw key should start with hc_secret_live_, got prefix: %q", result.RawKey[:16])
	}
	if len(result.RawKey) != len("hc_secret_live_")+64 {
		t.Errorf("raw key length: got %d, want %d", len(result.RawKey), len("hc_secret_live_")+64)
	}
	if !result.Key.Livemode {
		t.Error("key should be live by default")
	}
	if result.Key.KeyType != "secret" {
		t.Errorf("key_type: got %q, want secret", result.Key.KeyType)
	}
	if result.Key.TenantID != "tenant1" {
		t.Errorf("tenant_id: got %q", result.Key.TenantID)
	}

	// Verify salted hash: SHA-256(salt + rawKey)
	if result.Key.KeySalt == "" {
		t.Fatal("key salt should not be empty")
	}
	salt, err := hex.DecodeString(result.Key.KeySalt)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	hash := sha256.Sum256(append(salt, []byte(result.RawKey)...))
	if result.Key.KeyHash != hex.EncodeToString(hash[:]) {
		t.Error("salted hash mismatch")
	}
}

// Publishable keys were removed before v0.9.0: the type is refused on
// create, and a key with the old hc_pub_ prefix is an unknown key.
func TestCreateKey_RefusesRemovedPublishableType(t *testing.T) {
	svc := NewService(newMemStore())

	_, err := svc.CreateKey(postgres.WithLivemode(context.Background(), true), "tenant1", CreateKeyInput{
		Name:    "Frontend Key",
		KeyType: KeyType("publishable"),
	})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("create with key_type publishable: got %v, want a validation error", err)
	}
}

func TestValidateKey_OldPublishablePrefixIsInvalid(t *testing.T) {
	svc := NewService(newMemStore())

	for _, raw := range []string{"hc_pub_live_0123456789abcdef0123", "hc_pub_test_0123456789abcdef0123"} {
		if _, err := svc.ValidateKey(postgres.WithLivemode(context.Background(), true), raw); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateKey(%q): got %v, want ErrInvalidKey", raw, err)
		}
	}
}

// An ingest key (ADR-133) is issued like a secret key, in the caller's
// mode, with its own prefix, and validates back to its type.
func TestCreateKey_Ingest(t *testing.T) {
	svc := NewService(newMemStore())

	for _, live := range []bool{true, false} {
		ctx := WithKeyType(postgres.WithLivemode(context.Background(), live), KeyTypeSession)
		result, err := svc.CreateKey(ctx, "tenant1", CreateKeyInput{Name: "LiteLLM proxy", KeyType: KeyTypeIngest})
		if err != nil {
			t.Fatalf("create an ingest key (live=%v): %v", live, err)
		}
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
		got, err := svc.ValidateKey(ctx, result.RawKey)
		if err != nil {
			t.Fatalf("validate the ingest key: %v", err)
		}
		if got.KeyType != "ingest" || got.ID != result.Key.ID {
			t.Errorf("validated %+v, want the ingest key %s", got, result.Key.ID)
		}
	}
}

// Platform keys and /v1/tenants are gone (ADR-133): the type is refused on
// create from every caller, and a raw key with the old hc_platform_ prefix
// is an unknown key, even when a row with that prefix and a matching hash
// is in the store. Mutation: put "platform" back in the type check or in
// StoredKeyTypes, and this fails.
func TestPlatformKeyType_RefusedAtCreateAndParse(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)

	for name, ctx := range map[string]context.Context{
		"no key type":            postgres.WithLivemode(context.Background(), true),
		"secret key":             WithKeyType(postgres.WithLivemode(context.Background(), true), KeyTypeSecret),
		"session":                WithKeyType(postgres.WithLivemode(context.Background(), true), KeyTypeSession),
		"old platform principal": WithKeyType(postgres.WithLivemode(context.Background(), true), KeyType("platform")),
	} {
		_, err := svc.CreateKey(ctx, "tenant1", CreateKeyInput{Name: "Platform", KeyType: KeyType("platform")})
		if !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: create with key_type platform: got %v, want a validation error", name, err)
		}
	}
	if len(store.keys) != 0 {
		t.Fatalf("a refused create stored %d keys", len(store.keys))
	}

	// A platform-shaped row, hashed the way CreateKey hashes, is never
	// looked up: the parser does not know the prefix.
	raw := "hc_platform_live_" + strings.Repeat("ab", 32)
	salt := []byte("0123456789abcdef")
	hash := sha256.Sum256(append(salt, []byte(raw)...))
	store.keys["hc_key_plat"] = domain.APIKey{
		ID: "hc_key_plat", KeyPrefix: raw[:len("hc_platform_live_")+keyPrefixLen], KeyHash: hex.EncodeToString(hash[:]),
		KeySalt: hex.EncodeToString(salt), KeyType: "platform", Livemode: true, TenantID: "tenant1",
	}
	if _, err := svc.ValidateKey(context.Background(), raw); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("ValidateKey(hc_platform_live_…): got %v, want ErrInvalidKey", err)
	}
	if p := KeyType("platform").TypePrefix(); p != "" {
		t.Errorf("platform TypePrefix = %q, want none", p)
	}
}

func TestCreateKey_DefaultsToSecret(t *testing.T) {
	svc := NewService(newMemStore())

	result, _ := svc.CreateKey(postgres.WithLivemode(context.Background(), true), "t1", CreateKeyInput{Name: "Default"})
	if result.Key.KeyType != "secret" {
		t.Errorf("default key_type: got %q, want secret", result.Key.KeyType)
	}
}

func TestCreateKey_InvalidType(t *testing.T) {
	svc := NewService(newMemStore())

	_, err := svc.CreateKey(context.Background(), "t1", CreateKeyInput{
		Name:    "Bad",
		KeyType: "admin",
	})
	if err == nil {
		t.Fatal("expected error for invalid key type")
	}
}

func TestCreateKey_MissingName(t *testing.T) {
	svc := NewService(newMemStore())
	_, err := svc.CreateKey(context.Background(), "t1", CreateKeyInput{})
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

// TestCreateKey_PastExpiresAt locks in the 2026-05-24 fix: an API key
// with expires_at in the past or exactly at now is DOA. Industry parity
// (Stripe rejects past expiry on credentials).
func TestCreateKey_PastExpiresAt(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	t.Run("past expires_at rejected", func(t *testing.T) {
		past := time.Now().Add(-24 * time.Hour)
		_, err := svc.CreateKey(ctx, "t1", CreateKeyInput{
			Name:      "DOA key",
			ExpiresAt: &past,
		})
		if err == nil {
			t.Fatal("expected error on past expires_at")
		}
	})

	t.Run("expires_at == now rejected", func(t *testing.T) {
		nowT := time.Now()
		_, err := svc.CreateKey(ctx, "t1", CreateKeyInput{
			Name:      "expires now",
			ExpiresAt: &nowT,
		})
		if err == nil {
			t.Fatal("expected error on expires_at == now")
		}
	})

	t.Run("future expires_at accepted", func(t *testing.T) {
		future := time.Now().Add(30 * 24 * time.Hour)
		_, err := svc.CreateKey(ctx, "t1", CreateKeyInput{
			Name:      "future key",
			ExpiresAt: &future,
		})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("nil expires_at accepted (no expiry)", func(t *testing.T) {
		_, err := svc.CreateKey(ctx, "t1", CreateKeyInput{
			Name:      "perma key",
			ExpiresAt: nil,
		})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestValidateKey_AllTypes(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	for _, kt := range StoredKeyTypes {
		t.Run(string(kt), func(t *testing.T) {
			result, _ := svc.CreateKey(ctx, "tenant1", CreateKeyInput{
				Name:    "Test " + string(kt),
				KeyType: kt,
			})

			key, err := svc.ValidateKey(ctx, result.RawKey)
			if err != nil {
				t.Fatalf("validate %s key: %v", kt, err)
			}
			if key.TenantID != "tenant1" {
				t.Errorf("tenant_id: got %q", key.TenantID)
			}
			if key.KeyType != string(kt) {
				t.Errorf("key_type: got %q, want %q", key.KeyType, kt)
			}
		})
	}
}

func TestValidateKey_InvalidFormat(t *testing.T) {
	svc := NewService(newMemStore())

	cases := []string{
		"",
		"sk_live_invalid",
		"hc_secret_tooshort",
		"hc_unknown_" + strings.Repeat("ab", 32),
	}
	for _, raw := range cases {
		_, err := svc.ValidateKey(context.Background(), raw)
		if err == nil {
			t.Errorf("expected error for key %q", raw)
		}
	}
}

func TestValidateKey_WrongSecret(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	result, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "Real", KeyType: KeyTypeSecret})

	// Same prefix length but different secret
	fake := "hc_secret_live_" + strings.Repeat("ff", 32)
	if fake == result.RawKey {
		t.Skip("astronomically unlikely collision")
	}

	_, err := svc.ValidateKey(ctx, fake)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestValidateKey_Expired(t *testing.T) {
	svc := NewService(newMemStore())
	past := time.Now().UTC().Add(-24 * time.Hour)

	result, _ := svc.CreateKey(context.Background(), "t1", CreateKeyInput{
		Name:      "Expired",
		KeyType:   KeyTypeSecret,
		ExpiresAt: &past,
	})

	_, err := svc.ValidateKey(context.Background(), result.RawKey)
	if err == nil {
		t.Fatal("expected error for expired key")
	}
}

func TestValidateKey_Revoked(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	// Create a spare so the last-key safeguard doesn't refuse the revoke.
	if _, err := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "Spare", KeyType: KeyTypeSecret}); err != nil {
		t.Fatalf("create spare: %v", err)
	}
	result, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "Revokable", KeyType: KeyTypeSecret})
	if _, err := svc.RevokeKey(ctx, "t1", result.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err := svc.ValidateKey(ctx, result.RawKey)
	if err == nil {
		t.Fatal("expected error for revoked key")
	}
}

func TestCreateKey_TestModeFromCtx(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), false)

	result, err := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "Sandbox", KeyType: KeyTypeSecret})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(result.RawKey, "hc_secret_test_") {
		t.Errorf("test-mode key should start with hc_secret_test_, got %q", result.RawKey[:20])
	}
	if result.Key.Livemode {
		t.Error("key should be test-mode")
	}

	// ValidateKey on the same raw key recovers livemode=false.
	key, err := svc.ValidateKey(context.Background(), result.RawKey)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if key.Livemode {
		t.Error("validated key should be test-mode")
	}
}

func TestValidateKey_LegacyFormat(t *testing.T) {
	// Pre-FEAT-8 keys have no mode infix (hc_secret_<hex>). They must still
	// validate, and must be classified as live.
	store := newMemStore()
	svc := NewService(store)

	// Seed a legacy-format key directly.
	rawKey := "hc_secret_" + strings.Repeat("ab", 32)
	dbPrefix := "hc_secret_" + strings.Repeat("ab", 6) // first 12 hex chars
	salt := []byte("legacytestsalt16")
	saltHex := hex.EncodeToString(salt)
	hash := sha256.Sum256(append(salt, []byte(rawKey)...))
	store.keys["legacy1"] = domain.APIKey{
		ID:        "legacy1",
		KeyPrefix: dbPrefix,
		KeyHash:   hex.EncodeToString(hash[:]),
		KeySalt:   saltHex,
		KeyType:   string(KeyTypeSecret),
		Livemode:  true,
		TenantID:  "t1",
	}

	key, err := svc.ValidateKey(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("legacy key validate: %v", err)
	}
	if !key.Livemode {
		t.Error("legacy key should be classified as live")
	}
}

func TestRevokeKey_WrongTenant(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	result, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "Test", KeyType: KeyTypeSecret})
	_, err := svc.RevokeKey(ctx, "wrong_tenant", result.Key.ID)
	if err == nil {
		t.Fatal("expected error revoking from wrong tenant")
	}
}

// TestRotateKey_ImmediateRevoke covers the default rotation flow: the old key
// stops validating as soon as rotate returns, the new key validates, and the
// new key inherits the old's type/name/livemode/expires_at.
func TestRotateKey_ImmediateRevoke(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	expiry := time.Now().UTC().Add(90 * 24 * time.Hour)
	original, err := svc.CreateKey(ctx, "t1", CreateKeyInput{
		Name:      "Prod deploy",
		KeyType:   KeyTypeSecret,
		ExpiresAt: &expiry,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rotated, err := svc.RotateKey(ctx, "t1", original.Key.ID, RotateKeyInput{})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if rotated.NewKey.ID == original.Key.ID {
		t.Fatal("new key must have a distinct ID")
	}
	if rotated.NewKey.Name != original.Key.Name {
		t.Errorf("new key name: got %q, want %q", rotated.NewKey.Name, original.Key.Name)
	}
	if rotated.NewKey.KeyType != original.Key.KeyType {
		t.Errorf("new key type: got %q, want %q", rotated.NewKey.KeyType, original.Key.KeyType)
	}
	if rotated.NewKey.Livemode != original.Key.Livemode {
		t.Errorf("new key livemode: got %v, want %v", rotated.NewKey.Livemode, original.Key.Livemode)
	}
	if rotated.NewKey.ExpiresAt == nil || !rotated.NewKey.ExpiresAt.Equal(expiry) {
		t.Errorf("new key expires_at: got %v, want %v", rotated.NewKey.ExpiresAt, expiry)
	}
	if rotated.OldKey.RevokedAt == nil {
		t.Fatal("old key should be revoked after immediate rotation")
	}
	if rotated.RawKey == "" {
		t.Fatal("raw key must be returned once on rotation")
	}
	if rotated.RawKey == original.RawKey {
		t.Fatal("new raw key must differ from old raw key")
	}

	if _, err := svc.ValidateKey(ctx, original.RawKey); err == nil {
		t.Fatal("old key should fail validation after rotation")
	}
	valid, err := svc.ValidateKey(ctx, rotated.RawKey)
	if err != nil {
		t.Fatalf("new key validation: %v", err)
	}
	if valid.ID != rotated.NewKey.ID {
		t.Errorf("new key ID mismatch: got %q, want %q", valid.ID, rotated.NewKey.ID)
	}
}

// TestRotateKey_WithGracePeriod covers the zero-downtime rotation flow: the
// old key stays valid for the grace window, the new key is immediately valid,
// and both can authenticate concurrently within the window.
func TestRotateKey_WithGracePeriod(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	original, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "App", KeyType: KeyTypeSecret})

	rotated, err := svc.RotateKey(ctx, "t1", original.Key.ID, RotateKeyInput{ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if rotated.OldKey.RevokedAt != nil {
		t.Fatal("grace-period rotation must not set revoked_at on the old key")
	}
	if rotated.OldKey.ExpiresAt == nil {
		t.Fatal("grace-period rotation must set expires_at on the old key")
	}
	until := time.Until(*rotated.OldKey.ExpiresAt)
	if until < 50*time.Minute || until > 70*time.Minute {
		t.Errorf("old key expires_at should be ~1 hour out, got %v", until)
	}

	// Both keys authenticate during the grace window.
	if _, err := svc.ValidateKey(ctx, original.RawKey); err != nil {
		t.Errorf("old key should still validate in grace window: %v", err)
	}
	if _, err := svc.ValidateKey(ctx, rotated.RawKey); err != nil {
		t.Errorf("new key should validate immediately: %v", err)
	}
}

func TestRotateKey_RejectsNegativeGrace(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	result, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "K", KeyType: KeyTypeSecret})

	_, err := svc.RotateKey(ctx, "t1", result.Key.ID, RotateKeyInput{ExpiresInSeconds: -1})
	if err == nil {
		t.Fatal("expected validation error for negative grace")
	}
}

func TestRotateKey_RejectsExcessiveGrace(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	result, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "K", KeyType: KeyTypeSecret})

	_, err := svc.RotateKey(ctx, "t1", result.Key.ID, RotateKeyInput{ExpiresInSeconds: MaxRotationGraceSeconds + 1})
	if err == nil {
		t.Fatal("expected validation error for grace > 7 days")
	}
}

func TestRotateKey_RejectsAlreadyRevoked(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	// Spare so the last-key safeguard allows the revoke below.
	if _, err := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "Spare", KeyType: KeyTypeSecret}); err != nil {
		t.Fatalf("create spare: %v", err)
	}
	result, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "K", KeyType: KeyTypeSecret})
	if _, err := svc.RevokeKey(ctx, "t1", result.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err := svc.RotateKey(ctx, "t1", result.Key.ID, RotateKeyInput{})
	if err == nil {
		t.Fatal("expected error rotating an already-revoked key")
	}
}

func TestRotateKey_WrongTenant(t *testing.T) {
	svc := NewService(newMemStore())
	ctx := postgres.WithLivemode(context.Background(), true)

	result, _ := svc.CreateKey(ctx, "t1", CreateKeyInput{Name: "K", KeyType: KeyTypeSecret})

	_, err := svc.RotateKey(ctx, "wrong_tenant", result.Key.ID, RotateKeyInput{})
	if err == nil {
		t.Fatal("expected error rotating from wrong tenant")
	}
}

// TestRotateKey_PreservesTestmode guards against a subtle regression: if the
// caller's ctx is live-mode but the rotated key is test-mode, the mint path
// must still produce a test-mode replacement. Losing the mode here would
// issue a live key silently, which is the worst class of mode leak — a
// dashboard-issued "rotate" button should never mint a cross-mode key.
func TestRotateKey_PreservesTestmode(t *testing.T) {
	svc := NewService(newMemStore())

	testCtx := postgres.WithLivemode(context.Background(), false)
	original, _ := svc.CreateKey(testCtx, "t1", CreateKeyInput{Name: "Sandbox", KeyType: KeyTypeSecret})
	if original.Key.Livemode {
		t.Fatal("seed should be test-mode")
	}

	// Caller context is LIVE here — the mismatch is intentional.
	liveCtx := postgres.WithLivemode(context.Background(), true)
	rotated, err := svc.RotateKey(liveCtx, "t1", original.Key.ID, RotateKeyInput{})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.NewKey.Livemode {
		t.Fatal("rotated key must stay test-mode regardless of caller ctx mode")
	}
	if !strings.HasPrefix(rotated.RawKey, "hc_secret_test_") {
		t.Errorf("test-mode raw key prefix expected, got %q...", rotated.RawKey[:20])
	}
}

// (recordingSessionRevoker / fan-out tests deleted in ADR-011 —
// dashboard sessions are user-bound now, so no API-key revoke fan-out
// to test.)

// (TestRevokeKey_Safeguard / cookie-session fan-out tests deleted in
// ADR-011 — the last-active-secret/platform safeguard, SessionRevoker
// interface, and the fan-out call site are all gone with user-bound
// sessions. RevokeKey is now a thin store passthrough.)
