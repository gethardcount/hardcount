// Package auth issues and validates API keys, decides what each key type may
// do, and carries the authenticated principal (tenant, key, user and key
// type) in the request context.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

const (
	keyPrefixLen = 12 // hex chars used for DB prefix lookup
	keySecretLen = 32 // random bytes = 64 hex chars
)

// Service creates, validates, rotates and revokes API keys.
type Service struct {
	store Store

	// lastTouched debounces the last_used_at write per key: unix-nano of the
	// last touch this process issued, keyed by api key id. See touchLastUsed.
	lastTouched sync.Map // map[string]*atomic.Int64
	// now is the clock, injectable for the debounce tests.
	now func() time.Time
}

// lastUsedTouchInterval is how often, at most, last_used_at is written for
// one key. The column feeds "Last used 3 minutes ago" on the API-keys page and
// nothing else. No auth decision reads it, so minute granularity is fine.
//
// It is not written on every request, because then every request of one key
// would update the same row. A row lock passes to the next writer in about
// one round trip (~1.75 ms within one availability zone). So one key could
// not go past ~570 requests/s on any hardware, and more connections would
// only add waiters (#818).
const lastUsedTouchInterval = time.Minute

// NewService returns a Service backed by store.
func NewService(store Store) *Service {
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// touchLastUsed writes last_used_at for key id at most once per
// lastUsedTouchInterval per process. Exactly one caller wins a given interval
// even under a concurrent burst: the per-key timestamp is advanced with a
// compare-and-swap, so N simultaneous requests produce one UPDATE, not N
// goroutines queueing on one row.
func (s *Service) touchLastUsed(id string) {
	nowNs := s.now().UnixNano()
	v, _ := s.lastTouched.LoadOrStore(id, new(atomic.Int64))
	last := v.(*atomic.Int64)
	prev := last.Load()
	if prev != 0 && nowNs-prev < int64(lastUsedTouchInterval) {
		return
	}
	if !last.CompareAndSwap(prev, nowNs) {
		return // another request in this instant won the interval
	}
	go func() { _ = s.store.TouchLastUsed(context.Background(), id, time.Unix(0, nowNs).UTC()) }()
}

// CreateKeyResult contains the key record and the raw key, which is shown
// once.
type CreateKeyResult struct {
	Key    domain.APIKey `json:"key"`
	RawKey string        `json:"raw_key"`
}

// CreateKeyInput is the request to create a key. An empty KeyType means a
// secret key.
type CreateKeyInput struct {
	Name      string     `json:"name"`
	KeyType   KeyType    `json:"key_type"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CreateKey mints a key for tenantID, in the mode that ctx carries, and
// returns it with its raw value. It refuses an empty name, a key type other
// than secret or ingest, and an expires_at that is not in the future.
func (s *Service) CreateKey(ctx context.Context, tenantID string, input CreateKeyInput) (CreateKeyResult, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return CreateKeyResult{}, errs.Required("name")
	}

	keyType := input.KeyType
	if keyType == "" {
		keyType = KeyTypeSecret
	}
	// Only the stored types can be issued (ADR-133). Each holds no more
	// than the secret key or session that mints it (PermAPIKeyWrite), so
	// key_type from the request body cannot raise the caller's access.
	if keyType != KeyTypeSecret && keyType != KeyTypeIngest {
		return CreateKeyResult{}, errs.Invalid("key_type", "must be one of: secret, ingest")
	}

	// Reject an expires_at in the past. A key that has already expired at
	// creation is dead on arrival: every auth attempt fails with `api key
	// expired`. Stripe also rejects a past expiry on credentials. API keys
	// belong to the tenant and are used outside any customer's test clock
	// (simulated time), so the wall clock is the right reference.
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now().UTC()) {
		return CreateKeyResult{}, errs.Invalid("expires_at",
			"must be in the future — a key that expires at or before now is dead on arrival")
	}

	// The mode comes from the caller's ctx, as in Stripe: a test-mode key
	// creates test-mode keys, and a live-mode key creates live ones. No path
	// creates a key in the other mode. To switch mode, the caller must
	// authenticate in that mode.
	livemode, err := postgres.Livemode(ctx)
	if err != nil {
		return CreateKeyResult{}, err
	}

	// Generate raw key: prefix + random hex
	secret := make([]byte, keySecretLen)
	if _, err := rand.Read(secret); err != nil {
		return CreateKeyResult{}, fmt.Errorf("generate key: %w", err)
	}
	secretHex := hex.EncodeToString(secret)
	prefix := KeyPrefix(keyType, livemode)
	rawKey := prefix + secretHex

	// DB prefix: full mode-aware prefix + first N hex chars (indexed lookup)
	dbPrefix := prefix + secretHex[:keyPrefixLen]

	// Generate a 16-byte random salt for this key
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return CreateKeyResult{}, fmt.Errorf("generate salt: %w", err)
	}
	saltHex := hex.EncodeToString(salt)

	// Hash: SHA-256(salt + rawKey)
	hash := sha256.Sum256(append(salt, []byte(rawKey)...))
	hashHex := hex.EncodeToString(hash[:])

	id := postgres.NewID("hc_key")

	key, err := s.store.Create(ctx, domain.APIKey{
		ID:        id,
		KeyPrefix: dbPrefix,
		KeyHash:   hashHex,
		KeySalt:   saltHex,
		KeyType:   string(keyType),
		Livemode:  livemode,
		Name:      name,
		TenantID:  tenantID,
		ExpiresAt: input.ExpiresAt,
	})
	if err != nil {
		return CreateKeyResult{}, err
	}

	return CreateKeyResult{Key: key, RawKey: rawKey}, nil
}

// ErrInvalidKey marks a definitive credential rejection: a bad format, no
// matching key, a hash mismatch or an expired key. Middlewares map it, and
// only it, to the generic 401. Any other ValidateKey error means no verdict
// was reached (a store or database failure), and it must surface as a
// retryable 5xx. During a database outage, a 401 would tell every
// integrator that their key went bad and to start rotating credentials,
// which is false about the key (#560). A 503 for every caller reveals
// nothing about any one key, so ADR-026's rule against key enumeration
// holds.
var ErrInvalidKey = errors.New("invalid api key")

// ValidateKey looks up a key by its prefix, verifies the hash and checks
// expiry.
//
// It accepts three prefix forms, in priority order:
//  1. "hc_{type}_live_..."
//  2. "hc_{type}_test_..."
//  3. "hc_{type}_..." (the older form with no mode, treated as live)
//
// Form 3 keeps keys issued before the mode infix working. Newly issued keys
// always use form 1 or 2.
func (s *Service) ValidateKey(ctx context.Context, rawKey string) (domain.APIKey, error) {
	rawKey = strings.TrimSpace(rawKey)

	var fullPrefix string
	for _, kt := range StoredKeyTypes {
		typeP := kt.TypePrefix()
		if !strings.HasPrefix(rawKey, typeP) {
			continue
		}
		after := rawKey[len(typeP):]
		switch {
		case strings.HasPrefix(after, "live_"):
			fullPrefix = typeP + "live_"
		case strings.HasPrefix(after, "test_"):
			fullPrefix = typeP + "test_"
		default:
			// The older format has no mode infix: treat it as live.
			fullPrefix = typeP
		}
		break
	}
	if fullPrefix == "" {
		return domain.APIKey{}, fmt.Errorf("invalid key format: %w", ErrInvalidKey)
	}

	secretPart := strings.TrimPrefix(rawKey, fullPrefix)
	if len(secretPart) < keyPrefixLen {
		return domain.APIKey{}, fmt.Errorf("invalid key format: %w", ErrInvalidKey)
	}

	dbPrefix := fullPrefix + secretPart[:keyPrefixLen]

	key, err := s.store.GetByPrefix(ctx, dbPrefix)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.APIKey{}, ErrInvalidKey
		}
		// The lookup reached no verdict. This is an infrastructure failure,
		// not a credential one, and callers answer it with a 5xx.
		return domain.APIKey{}, fmt.Errorf("api key lookup: %w", err)
	}

	// Verify hash using the stored salt
	salt, err := hex.DecodeString(key.KeySalt)
	if err != nil {
		return domain.APIKey{}, fmt.Errorf("undecodable key salt: %w", ErrInvalidKey)
	}
	hash := sha256.Sum256(append(salt, []byte(rawKey)...))
	hashHex := hex.EncodeToString(hash[:])
	if subtle.ConstantTimeCompare([]byte(hashHex), []byte(key.KeyHash)) != 1 {
		return domain.APIKey{}, ErrInvalidKey
	}

	// Check expiration
	if key.ExpiresAt != nil && time.Now().UTC().After(*key.ExpiresAt) {
		return domain.APIKey{}, fmt.Errorf("api key expired: %w", ErrInvalidKey)
	}

	// Touch last_used_at, debounced to at most once per key per interval
	// (#818).
	s.touchLastUsed(key.ID)

	return key, nil
}

// RevokeKey revokes the tenant's key with id and returns the revoked key.
func (s *Service) RevokeKey(ctx context.Context, tenantID, id string) (domain.APIKey, error) {
	return s.store.Revoke(ctx, tenantID, id)
}

// MaxRotationGraceSeconds caps how long a caller can keep the old key alive
// during a rotation. Seven days matches the longest grace window Stripe
// offers in its dashboard "roll key" flow. It is long enough for a scheduled
// deploy cycle, and short enough that a forgotten rotation still ends on its
// own.
const MaxRotationGraceSeconds = 7 * 24 * 60 * 60

// RotateKeyInput sets how a rotation treats the old key. ExpiresInSeconds=0
// revokes the old key at once. A positive value keeps it valid for that many
// seconds past now, or until its own expires_at if that comes first, which
// gives deployed clients time to swap credentials without a hard cutover.
type RotateKeyInput struct {
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
}

// RotateKeyResult returns both sides of a rotation. RawKey is the new key's
// plaintext. It is shown once, and never stored or returned on a later
// fetch. OldKey's ExpiresAt (a grace window) or RevokedAt (immediate) shows
// which path the caller took.
type RotateKeyResult struct {
	OldKey domain.APIKey `json:"old_key"`
	NewKey domain.APIKey `json:"new_key"`
	RawKey string        `json:"raw_key"`
}

// RotateKey issues a replacement for key `id`, keeping its name, type,
// livemode and any configured expires_at. The old key is revoked at once
// (grace 0), or set to expire after input.ExpiresInSeconds seconds; a grace
// never moves an earlier expires_at later. A revoked or expired key cannot be
// rotated: create a new key instead.
func (s *Service) RotateKey(ctx context.Context, tenantID, id string, input RotateKeyInput) (RotateKeyResult, error) {
	if input.ExpiresInSeconds < 0 {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds", "must be >= 0")
	}
	if input.ExpiresInSeconds > MaxRotationGraceSeconds {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds",
			fmt.Sprintf("must be <= %d (7 days)", MaxRotationGraceSeconds))
	}

	old, err := s.store.Get(ctx, tenantID, id)
	if err != nil {
		return RotateKeyResult{}, err
	}
	if old.RevokedAt != nil {
		return RotateKeyResult{}, errs.InvalidState("cannot rotate a revoked key")
	}
	// The same rule CreateKey applies to a new key's expires_at: at or
	// before now is expired.
	if old.ExpiresAt != nil && !time.Now().UTC().Before(*old.ExpiresAt) {
		return RotateKeyResult{}, errs.InvalidState("cannot rotate an expired key")
	}

	// Mint the replacement through the same CreateKey path, so livemode
	// handling, hashing and prefix generation stay in one place. The new key
	// takes the old key's name, type and expires_at. A rotation is a
	// credential swap, not a reconfiguration.
	createInput := CreateKeyInput{
		Name:      old.Name,
		KeyType:   KeyType(old.KeyType),
		ExpiresAt: old.ExpiresAt,
	}
	// The new key must have the old key's livemode. postgres.Livemode(ctx)
	// depends on the request ctx, which the caller controls. So ctx is set to
	// the old key's mode, and a live key rotated by a test-mode caller still
	// gets a live replacement.
	created, err := s.CreateKey(postgres.WithLivemode(ctx, old.Livemode), tenantID, createInput)
	if err != nil {
		return RotateKeyResult{}, fmt.Errorf("mint replacement key: %w", err)
	}

	var oldAfter domain.APIKey
	if input.ExpiresInSeconds == 0 {
		// Immediate revocation: the old key stops authenticating on the next
		// ValidateKey call. A request already in flight under the old key
		// completes, and later ones fail.
		oldAfter, err = s.store.Revoke(ctx, tenantID, id)
	} else {
		graceUntil := time.Now().UTC().Add(time.Duration(input.ExpiresInSeconds) * time.Second)
		oldAfter, err = s.store.ScheduleExpiry(ctx, tenantID, id, graceUntil)
	}
	if err != nil {
		// The replacement is already stored, but its secret is never
		// returned, so nobody can use it. The old key is unchanged and still
		// works. The caller retries the rotation and revokes the leftover
		// replacement, which shows in the key list. Its id goes in the error
		// for the log.
		//
		// The wording avoids the phrases that respond.FromError's
		// message-text fallback matches (" must be ", " is required", ...).
		// So a store failure here is logged and answered as a 500, not sent
		// to the client as a 422 carrying this text.
		return RotateKeyResult{}, fmt.Errorf("retire old key after creating replacement %s: %w", created.Key.ID, err)
	}

	return RotateKeyResult{
		OldKey: oldAfter,
		NewKey: created.Key,
		RawKey: created.RawKey,
	}, nil
}

// ListKeys returns one page of the keys that filter selects, and how many
// match.
func (s *Service) ListKeys(ctx context.Context, filter ListFilter) ([]domain.APIKey, int, error) {
	return s.store.List(ctx, filter)
}
