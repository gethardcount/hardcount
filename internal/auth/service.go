// Package auth issues API keys and decides who may do what. Service manages
// the keys, and Handler serves their routes.
// Caller is who a request acts as, a dashboard session or a key, and
// RequirePermission refuses a caller what its route does not let it do.
package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Service creates, lists, validates, rotates and revokes API keys. A change
// to a key (create, revoke, rotate) and its audit row commit in one
// transaction, or neither does.
type Service struct {
	db       *postgres.DB
	store    store
	audit    AuditWriter
	lastUsed lastUsedDebounce
}

// AuditWriter writes an audit row on the caller's transaction, so the change
// and its row commit or roll back together. *audit.Logger implements it;
// auth cannot import audit, which imports auth.
type AuditWriter interface {
	LogInTxFields(ctx context.Context, tx *sql.Tx, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// NewService returns a Service on db that writes each key change's audit row
// with audit. It panics on a nil audit: a key change with no audit row would
// be invisible on the operator's Audit Log page.
func NewService(db *postgres.DB, audit AuditWriter) *Service {
	if audit == nil {
		panic("auth.NewService: audit writer is required")
	}
	return &Service{db: db, store: store{db: db}, audit: audit}
}

// auditKeyChange writes the audit row of a change to key on tx, labelled with
// the key's id and name. metadata must never hold the key, its hash or its
// salt.
func (s *Service) auditKeyChange(ctx context.Context, tx *sql.Tx, action string, key domain.APIKey, metadata map[string]any) error {
	return s.audit.LogInTxFields(ctx, tx, action, "api_key", key.ID, key.Name, metadata)
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

// CreateKeyResult is a new key and its raw value. No later answer can show
// the raw value: the table holds only its hash, and the routes that answer
// with it are withShowOnce (internal/api/route_table.go), so the
// idempotency layer keeps no copy.
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
// returns it with its raw value. checkCreateInput says what it refuses.
func (s *Service) CreateKey(ctx context.Context, tenantID string, input CreateKeyInput) (CreateKeyResult, error) {
	var result CreateKeyResult
	err := s.db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		var err error
		result, err = s.CreateKeyTx(ctx, tx, tenantID, input)
		return err
	})
	if err != nil {
		return CreateKeyResult{}, err
	}
	return result, nil
}

// CreateKeyTx is CreateKey on the caller's transaction, for account setup
// (tenant.RunBootstrap). The caller gives the mode twice:
//   - tx has app.tenant_id set to tenantID and app.livemode set: the row and
//     its audit row take their mode from tx;
//   - ctx carries the same mode (postgres.WithLivemode): the key's prefix
//     takes it from ctx.
//
// api_keys_prefix_shape_check refuses a key where the two differ.
func (s *Service) CreateKeyTx(ctx context.Context, tx *sql.Tx, tenantID string, input CreateKeyInput) (CreateKeyResult, error) {
	k, err := checkCreateInput(input, time.Now().UTC())
	if err != nil {
		return CreateKeyResult{}, err
	}
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return CreateKeyResult{}, err
	}
	result, err := s.mintAndInsert(ctx, tx, tenantID, k, live)
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

// maxKeyNameLen is the most characters a new key's name may have.
const maxKeyNameLen = 100

// checkCreateInput checks a create request and returns the key to mint: a
// name of 1 to maxKeyNameLen characters once trimmed, a key type (secret
// when empty), and an expires_at after now. now is the machine's clock, not
// the request's time: money work reads the request's time, and a key's
// expiry records when something happens in the world.
func checkCreateInput(input CreateKeyInput, now time.Time) (CreateKeyInput, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return CreateKeyInput{}, errs.Required("name")
	}
	if utf8.RuneCountInString(name) > maxKeyNameLen {
		return CreateKeyInput{}, errs.Invalid("name", fmt.Sprintf("must be at most %d characters", maxKeyNameLen))
	}

	keyType := input.KeyType
	if keyType == "" {
		keyType = KeyTypeSecret
	}
	// key_type only has to name a type: every caller that can mint a key
	// holds at least what an ingest key holds, so no type raises access.
	if !slices.Contains(keyTypes, keyType) {
		names := make([]string, len(keyTypes))
		for i, kt := range keyTypes {
			names[i] = string(kt)
		}
		return CreateKeyInput{}, errs.Invalid("key_type", "must be one of: "+strings.Join(names, ", "))
	}

	if input.ExpiresAt != nil && !input.ExpiresAt.After(now) {
		return CreateKeyInput{}, errs.Invalid("expires_at", "must be in the future")
	}
	return CreateKeyInput{Name: name, KeyType: keyType, ExpiresAt: input.ExpiresAt}, nil
}

// mintAndInsert mints k for tenantID in mode livemode and inserts it on tx.
func (s *Service) mintAndInsert(ctx context.Context, tx *sql.Tx, tenantID string, k CreateKeyInput, livemode bool) (CreateKeyResult, error) {
	rawKey, lookup, hash, err := mintKey(k.KeyType, livemode)
	if err != nil {
		return CreateKeyResult{}, err
	}

	key, err := s.store.insert(ctx, tx, domain.APIKey{
		ID:        postgres.NewID("hc_key"),
		KeyPrefix: lookup,
		KeyType:   string(k.KeyType),
		Name:      k.Name,
		TenantID:  tenantID,
		ExpiresAt: k.ExpiresAt,
	}, hash)
	if err != nil {
		return CreateKeyResult{}, err
	}

	return CreateKeyResult{Key: key, RawKey: rawKey}, nil
}

// ErrInvalidKey is ValidateKey's verdict that a string is not a usable key:
// the only ValidateKey error that may answer 401. Any other is a store
// failure that says nothing about the key (ADR-026).
var ErrInvalidKey = errors.New("invalid api key")

// ValidateKey returns the active key rawKey is. It returns ErrInvalidKey when
// rawKey does not have a key's shape (lookupPrefix), matches no stored key,
// or names a key that is not active (statusAt). It writes the key's
// last_used_at as lastUsedDebounce allows; a failed write is logged and does
// not refuse the key.
func (s *Service) ValidateKey(ctx context.Context, rawKey string) (domain.APIKey, error) {
	lookup, ok := lookupPrefix(rawKey)
	if !ok {
		return domain.APIKey{}, fmt.Errorf("invalid key format: %w", ErrInvalidKey)
	}

	key, stored, err := s.store.getByPrefix(ctx, lookup)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return domain.APIKey{}, ErrInvalidKey
		}
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

	// getByPrefix set Status as it read the row: scanKey, in postgres.go,
	// applies statusAt at the server's clock.
	if key.Status != domain.APIKeyStatusActive {
		return domain.APIKey{}, fmt.Errorf("api key %s: %w", key.Status, ErrInvalidKey)
	}

	now := time.Now().UTC()
	if s.lastUsed.due(key.ID, now) {
		if err := s.store.touchLastUsed(ctx, key.ID, now); err != nil {
			slog.WarnContext(ctx, "write api key last_used_at", "key_id", key.ID, "error", err)
		}
	}

	return key, nil
}

// lastUsedTouchInterval is the least time between one process's writes of a
// key's last_used_at: no auth decision reads it, and each write locks the
// key's row.
const lastUsedTouchInterval = time.Minute

// lastUsedDebounce decides when a key's last_used_at is due for a write: at
// most once per lastUsedTouchInterval per key, in this process.
type lastUsedDebounce struct {
	last sync.Map // key id → *atomic.Int64: unix nanoseconds of the last claimed write
}

// due reports whether a write of key id's last_used_at is due at now, and
// claims it. The claim is a compare-and-swap, so of concurrent callers only
// one wins a due write.
func (d *lastUsedDebounce) due(id string, now time.Time) bool {
	v, _ := d.last.LoadOrStore(id, new(atomic.Int64))
	last := v.(*atomic.Int64)
	prev := last.Load()
	if prev != 0 && now.UnixNano()-prev < int64(lastUsedTouchInterval) {
		return false
	}
	return last.CompareAndSwap(prev, now.UnixNano())
}

// RevokeKey revokes the tenant's key with id, in the mode ctx carries, and
// returns the revoked key. A key that is not found, or already revoked,
// returns errs.ErrNotFound. A caller may revoke the key it calls with, and
// the last key: an operator can still sign in and make a new one (ADR-011).
func (s *Service) RevokeKey(ctx context.Context, tenantID, id string) (domain.APIKey, error) {
	var key domain.APIKey
	err := s.db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
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

// maxRotationGrace caps how long a rotation keeps the old key working: long
// enough for a scheduled deploy, short enough that a forgotten rotation still
// ends on its own.
const maxRotationGrace = 7 * 24 * time.Hour

// RotateKeyInput is the request to rotate a key. ExpiresInSeconds is the old
// key's grace (RotateKey).
type RotateKeyInput struct {
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
}

// RotateKeyResult is the old key as it now stands, the new key, and the new
// key's raw value.
type RotateKeyResult struct {
	OldKey domain.APIKey `json:"old_key"`
	NewKey domain.APIKey `json:"new_key"`
	RawKey string        `json:"raw_key"`
}

// RotateKey replaces the tenant's key with id, in the mode ctx carries, with
// a new key of the same name, type, mode and expires_at. A grace of 0
// revokes the old key; a grace of up to maxRotationGrace sets it to expire
// that many seconds from now, or at its own expires_at if that is sooner
// (scheduleExpiry, in postgres.go, does it in SQL). A grace out of range returns a validation error, a key
// not found errs.ErrNotFound, and a key that is not active
// errs.ErrInvalidState.
func (s *Service) RotateKey(ctx context.Context, tenantID, id string, input RotateKeyInput) (RotateKeyResult, error) {
	maxGraceSeconds := int64(maxRotationGrace / time.Second)
	if input.ExpiresInSeconds < 0 {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds", "must be 0 or more")
	}
	if input.ExpiresInSeconds > maxGraceSeconds {
		return RotateKeyResult{}, errs.Invalid("expires_in_seconds",
			fmt.Sprintf("must be at most %d seconds (7 days)", maxGraceSeconds))
	}

	var result RotateKeyResult
	err := s.db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		// Row-level security finds the key only in the caller's mode, so
		// old.Livemode is the transaction's mode, and the replacement, minted
		// on the same transaction, is in it.
		old, err := s.store.getForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.Status != domain.APIKeyStatusActive {
			return errs.InvalidState("cannot rotate a " + old.Status + " key")
		}

		// A rotation swaps the credential and changes nothing else, so the old
		// key's name, type and expires_at are not checked again.
		created, err := s.mintAndInsert(ctx, tx, tenantID, CreateKeyInput{
			Name:      old.Name,
			KeyType:   KeyType(old.KeyType),
			ExpiresAt: old.ExpiresAt,
		}, old.Livemode)
		if err != nil {
			return fmt.Errorf("mint replacement key: %w", err)
		}

		var oldAfter domain.APIKey
		var outcome string
		var stopsAt *time.Time
		if input.ExpiresInSeconds == 0 {
			oldAfter, err = s.store.revoke(ctx, tx, id)
			outcome, stopsAt = "revoked", oldAfter.RevokedAt
		} else {
			graceUntil := time.Now().UTC().Add(time.Duration(input.ExpiresInSeconds) * time.Second)
			oldAfter, err = s.store.scheduleExpiry(ctx, tx, id, graceUntil)
			outcome, stopsAt = "expires", oldAfter.ExpiresAt
		}
		if err != nil {
			return fmt.Errorf("retire old key: %w", err)
		}

		if err := s.auditKeyChange(ctx, tx, domain.AuditActionRotate, created.Key, map[string]any{
			"key_type":         created.Key.KeyType,
			"old_key_id":       id,
			"old_key_outcome":  outcome,
			"old_key_stops_at": stopsAt.UTC().Format(time.RFC3339Nano),
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

// ListKeys returns one page of filter's keys in the mode ctx carries, newest
// first, and how many keys there are.
func (s *Service) ListKeys(ctx context.Context, filter ListFilter) ([]domain.APIKey, int, error) {
	return s.store.list(ctx, filter)
}
