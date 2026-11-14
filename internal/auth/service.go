// Package auth issues and validates API keys, decides what each key type may
// do, and carries the authenticated principal (tenant, key, user and key
// type) in the request context.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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

// Service creates, validates, rotates and revokes API keys. A change to a
// key (create, revoke, rotate) and its audit row commit in one transaction,
// or neither does.
type Service struct {
	db       *postgres.DB
	store    store
	audit    AuditWriter
	lastUsed lastUsedDebounce
}

// apiKeyResourceType is the resource_type of a key's audit rows.
const apiKeyResourceType = "api_key"

// AuditWriter writes an audit row on the caller's transaction, so the change
// and its row commit or roll back together. *audit.Logger is the one
// implementation; auth cannot import audit, which imports auth for the actor.
type AuditWriter interface {
	LogInTxFields(ctx context.Context, tx *sql.Tx, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// lastUsedTouchInterval is how often, at most, last_used_at is written for
// one key. The column feeds "Last used 3 minutes ago" on the API-keys page and
// nothing else. No auth decision reads it, so minute granularity is fine.
//
// It is not written on every request, because then every request of one key
// would update the same row. A row lock passes to the next writer in about
// one round trip (~1.75 ms within one availability zone). So one key could
// not go past ~570 requests/s on any hardware, and more connections would
// only add waiters.
const lastUsedTouchInterval = time.Minute

// NewService returns a Service on db that writes each key change's audit row
// with audit. It panics on a nil audit: a key change with no audit row would
// be invisible on the operator's Audit Log page.
func NewService(db *postgres.DB, audit AuditWriter) *Service {
	if audit == nil {
		panic("auth.NewService: audit writer is required")
	}
	return &Service{db: db, store: store{db: db}, audit: audit}
}

// inTenantTx runs fn in a transaction scoped to tenantID and the mode ctx
// carries, and commits it when fn succeeds.
func (s *Service) inTenantTx(ctx context.Context, tenantID string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// auditKeyChange writes the audit row of a change to key on tx: the key's id
// and name, and metadata, which never holds the raw key.
func (s *Service) auditKeyChange(ctx context.Context, tx *sql.Tx, action string, key domain.APIKey, metadata map[string]any) error {
	return s.audit.LogInTxFields(ctx, tx, action, apiKeyResourceType, key.ID, key.Name, metadata)
}

// lastUsedDebounce decides when a key's last_used_at is due for a write: at
// most once per lastUsedTouchInterval per key, in this process.
type lastUsedDebounce struct {
	last sync.Map // key id → *atomic.Int64: unix nanoseconds of the last claimed write
}

// due reports whether a write of key id's last_used_at is due at now, and
// claims it. Under a concurrent burst exactly one caller wins an interval:
// the per-key time is advanced with a compare-and-swap, so simultaneous
// requests make one write, not one each.
func (d *lastUsedDebounce) due(id string, now time.Time) bool {
	v, _ := d.last.LoadOrStore(id, new(atomic.Int64))
	last := v.(*atomic.Int64)
	prev := last.Load()
	if prev != 0 && now.UnixNano()-prev < int64(lastUsedTouchInterval) {
		return false
	}
	return last.CompareAndSwap(prev, now.UnixNano())
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
// than secret or ingest, and an expires_at that is not in the future. The
// key and its audit row commit together.
func (s *Service) CreateKey(ctx context.Context, tenantID string, input CreateKeyInput) (CreateKeyResult, error) {
	var result CreateKeyResult
	err := s.inTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		result, err = s.CreateKeyTx(ctx, tx, tenantID, input)
		return err
	})
	if err != nil {
		return CreateKeyResult{}, err
	}
	return result, nil
}

// CreateKeyTx is CreateKey on a transaction the caller owns, for the setup
// command, which creates the account and its first keys in one transaction:
// the key and its audit row commit with tx. It sets tx's app.livemode to the
// mode ctx carries, so the key and its audit row are in that mode, and the
// setting stays for the rest of tx. tx must set app.tenant_id to tenantID.
func (s *Service) CreateKeyTx(ctx context.Context, tx *sql.Tx, tenantID string, input CreateKeyInput) (CreateKeyResult, error) {
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return CreateKeyResult{}, err
	}
	mode := "off"
	if live {
		mode = "on"
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', $1, true)`, mode); err != nil {
		return CreateKeyResult{}, fmt.Errorf("set the key's mode: %w", err)
	}
	result, err := s.checkMintAndInsert(ctx, tx, tenantID, input)
	if err != nil {
		return CreateKeyResult{}, err
	}
	if err := s.auditKeyChange(ctx, tx, domain.AuditActionCreate, result.Key, map[string]any{
		"key_type": result.Key.KeyType,
		"livemode": result.Key.Livemode,
	}); err != nil {
		return CreateKeyResult{}, err
	}
	return result, nil
}

// statusAt is the one rule for a key's status at now: revoked once revoked;
// otherwise expired at or after its expires_at; otherwise active. Only an
// active key authenticates (ValidateKey) or can be rotated (RotateKey).
func statusAt(key domain.APIKey, now time.Time) string {
	switch {
	case key.RevokedAt != nil:
		return domain.APIKeyStatusRevoked
	case key.ExpiresAt != nil && !now.Before(*key.ExpiresAt):
		return domain.APIKeyStatusExpired
	default:
		return domain.APIKeyStatusActive
	}
}

// hashKey returns the stored hash of raw: hex(SHA-256(salt || raw)). Every
// new key has a fresh 16-byte salt. A key the setup command wrote in v0.9.3
// or earlier has an empty salt and still validates.
func hashKey(salt []byte, raw string) string {
	sum := sha256.Sum256(append(append([]byte{}, salt...), raw...))
	return hex.EncodeToString(sum[:])
}

// checkMintAndInsert checks input, mints a key for tenantID in the mode that
// ctx carries, and inserts it on tx.
func (s *Service) checkMintAndInsert(ctx context.Context, tx *sql.Tx, tenantID string, input CreateKeyInput) (CreateKeyResult, error) {
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
	prefix := keyPrefix(keyType, livemode)
	rawKey := prefix + secretHex

	// DB prefix: full mode-aware prefix + first N hex chars (indexed lookup)
	dbPrefix := prefix + secretHex[:keyPrefixLen]

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return CreateKeyResult{}, fmt.Errorf("generate salt: %w", err)
	}

	key, err := s.store.insert(ctx, tx, domain.APIKey{
		ID:        postgres.NewID("hc_key"),
		KeyPrefix: dbPrefix,
		KeyType:   string(keyType),
		Livemode:  livemode,
		Name:      name,
		TenantID:  tenantID,
		ExpiresAt: input.ExpiresAt,
	}, storedHash{salt: hex.EncodeToString(salt), hash: hashKey(salt, rawKey)})
	if err != nil {
		return CreateKeyResult{}, err
	}

	return CreateKeyResult{Key: key, RawKey: rawKey}, nil
}

// ErrInvalidKey marks a definitive credential rejection: a bad format, no
// matching key, a hash mismatch, or a key that is expired or revoked. Middlewares map it, and
// only it, to the generic 401. Any other ValidateKey error means no verdict
// was reached (a store or database failure), and it must surface as a
// retryable 5xx. During a database outage, a 401 would tell every
// integrator that their key went bad and to start rotating credentials,
// which is false about the key (#560). A 503 for every caller reveals
// nothing about any one key, so ADR-026's rule against key enumeration
// holds.
var ErrInvalidKey = errors.New("invalid api key")

// ValidateKey returns the key rawKey is, or ErrInvalidKey. rawKey must have a
// key's shape (lookupPrefix) and match a stored key's hash, and the key must
// be active (statusAt). It writes the key's last_used_at at most
// once per lastUsedTouchInterval; a failed write is logged and does not refuse
// the key.
func (s *Service) ValidateKey(ctx context.Context, rawKey string) (domain.APIKey, error) {
	// A string that is not a key is refused before any lookup.
	rawKey = strings.TrimSpace(rawKey)
	dbPrefix, ok := lookupPrefix(rawKey)
	if !ok {
		return domain.APIKey{}, fmt.Errorf("invalid key format: %w", ErrInvalidKey)
	}

	key, stored, err := s.store.getByPrefix(ctx, dbPrefix)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.APIKey{}, ErrInvalidKey
		}
		// The lookup reached no verdict. This is an infrastructure failure,
		// not a credential one, and callers answer it with a 5xx.
		return domain.APIKey{}, fmt.Errorf("api key lookup: %w", err)
	}

	salt, err := hex.DecodeString(stored.salt)
	if err != nil {
		// api_keys_salt_shape_check refuses such a row, so this is a fault in
		// the store, not a verdict on the key.
		return domain.APIKey{}, fmt.Errorf("stored key %s has a salt that is not hex: %w", key.ID, err)
	}
	if subtle.ConstantTimeCompare([]byte(hashKey(salt, rawKey)), []byte(stored.hash)) != 1 {
		return domain.APIKey{}, ErrInvalidKey
	}

	if key.Status != domain.APIKeyStatusActive {
		return domain.APIKey{}, fmt.Errorf("api key %s: %w", key.Status, ErrInvalidKey)
	}

	// Write last_used_at, at most once per key per lastUsedTouchInterval, in
	// the request. A failed write is logged and does not refuse the key: no
	// auth decision reads the column.
	now := time.Now().UTC()
	if s.lastUsed.due(key.ID, now) {
		if err := s.store.touchLastUsed(ctx, key.ID, now); err != nil {
			slog.WarnContext(ctx, "write api key last_used_at", "key_id", key.ID, "error", err)
		}
	}

	return key, nil
}

// RevokeKey revokes the tenant's key with id and returns the revoked key. The
// revocation and its audit row commit together. A key that is not found, or
// already revoked, returns errs.ErrNotFound.
func (s *Service) RevokeKey(ctx context.Context, tenantID, id string) (domain.APIKey, error) {
	var key domain.APIKey
	err := s.inTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		if key, err = s.store.revoke(ctx, tx, id); err != nil {
			return err
		}
		return s.auditKeyChange(ctx, tx, domain.AuditActionRevoke, key, map[string]any{
			"key_type": key.KeyType,
		})
	})
	if err != nil {
		return domain.APIKey{}, err
	}
	return key, nil
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
// rotated: create a new key instead. The replacement, the old key's change
// and the audit row commit in one transaction, with the old key's row locked
// from the first read.
func (s *Service) RotateKey(ctx context.Context, tenantID, id string, input RotateKeyInput) (RotateKeyResult, error) {
	if input.ExpiresInSeconds < 0 {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds", "must be >= 0")
	}
	if input.ExpiresInSeconds > MaxRotationGraceSeconds {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds",
			fmt.Sprintf("must be <= %d (7 days)", MaxRotationGraceSeconds))
	}

	var result RotateKeyResult
	err := s.inTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		// Row-level security finds the key only in the caller's mode, so the
		// replacement, minted on the same transaction, is in the old key's mode.
		old, err := s.store.getForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.Status != domain.APIKeyStatusActive {
			return errs.InvalidState("cannot rotate a " + old.Status + " key")
		}

		// The replacement takes the old key's name, type and expires_at. A
		// rotation is a credential swap, not a reconfiguration.
		created, err := s.checkMintAndInsert(ctx, tx, tenantID, CreateKeyInput{
			Name:      old.Name,
			KeyType:   KeyType(old.KeyType),
			ExpiresAt: old.ExpiresAt,
		})
		if err != nil {
			return fmt.Errorf("mint replacement key: %w", err)
		}

		var oldAfter domain.APIKey
		if input.ExpiresInSeconds == 0 {
			// The old key stops authenticating on the next ValidateKey. A
			// request already in flight under it completes.
			oldAfter, err = s.store.revoke(ctx, tx, id)
		} else {
			graceUntil := time.Now().UTC().Add(time.Duration(input.ExpiresInSeconds) * time.Second)
			oldAfter, err = s.store.scheduleExpiry(ctx, tx, id, graceUntil)
		}
		if err != nil {
			return fmt.Errorf("retire old key: %w", err)
		}

		if err := s.auditKeyChange(ctx, tx, domain.AuditActionRotate, created.Key, map[string]any{
			"key_type":   created.Key.KeyType,
			"old_key_id": id,
		}); err != nil {
			return err
		}
		result = RotateKeyResult{OldKey: oldAfter, NewKey: created.Key, RawKey: created.RawKey}
		return nil
	})
	if err != nil {
		return RotateKeyResult{}, err
	}
	return result, nil
}

// ListKeys returns one page of the keys that filter selects, and how many
// match.
func (s *Service) ListKeys(ctx context.Context, filter ListFilter) ([]domain.APIKey, int, error) {
	return s.store.list(ctx, filter)
}
