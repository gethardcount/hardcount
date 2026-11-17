package auth_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

// These tests run auth.Service on the real api_keys table, so its row-level
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

func (k keyRig) list(t *testing.T, ctx context.Context) []domain.APIKey {
	t.Helper()
	keys, _, err := k.svc.ListKeys(ctx, auth.ListFilter{TenantID: k.tenantID, Limit: 100})
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	return keys
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

// stored returns the hash and salt the table holds for key id.
func (k keyRig) stored(t *testing.T, id string) (hash, salt string) {
	t.Helper()
	k.bypass(t, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT key_hash, key_salt FROM api_keys WHERE id = $1`, id).Scan(&hash, &salt)
	})
	return hash, salt
}

type auditRow struct {
	action, resourceID, label string
	metadata                  map[string]any
	rawMetadata               string
}

// auditRows returns the tenant's api_key audit rows, oldest first.
func (k keyRig) auditRows(t *testing.T) []auditRow {
	t.Helper()
	var out []auditRow
	k.bypass(t, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT action, resource_id, resource_label, metadata FROM audit_log
			WHERE tenant_id = $1 AND resource_type = 'api_key' ORDER BY created_at, id`, k.tenantID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r auditRow
			var meta []byte
			if err := rows.Scan(&r.action, &r.resourceID, &r.label, &meta); err != nil {
				return err
			}
			r.rawMetadata = string(meta)
			if err := json.Unmarshal(meta, &r.metadata); err != nil {
				return fmt.Errorf("decode metadata %s: %w", meta, err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out
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

// A new key is its type-and-mode prefix and 64 hex characters, and the table
// stores it as SHA-256(salt + key) with a salt.
func TestCreateKey_StoresASaltedHashOfTheKey(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Backend Key", KeyType: auth.KeyTypeSecret})

	if !strings.HasPrefix(result.RawKey, "hc_secret_live_") || len(result.RawKey) != len("hc_secret_live_")+64 {
		t.Errorf("raw key %q…: want hc_secret_live_ and 64 hex characters", result.RawKey[:16])
	}
	if !result.Key.Livemode || result.Key.KeyType != "secret" || result.Key.TenantID != k.tenantID {
		t.Errorf("stored key: livemode %v, key_type %q, tenant %q; want a live secret key of %s",
			result.Key.Livemode, result.Key.KeyType, result.Key.TenantID, k.tenantID)
	}

	keyHash, keySalt := k.stored(t, result.Key.ID)
	salt, err := hex.DecodeString(keySalt)
	if err != nil || len(salt) == 0 {
		t.Fatalf("stored salt %q: want hex bytes (%v)", keySalt, err)
	}
	hash := sha256.Sum256(append(salt, []byte(result.RawKey)...))
	if keyHash != hex.EncodeToString(hash[:]) {
		t.Error("the stored hash is not SHA-256(salt + raw key)")
	}
}

// No key_type means secret, a key_type that is not a key type and a missing
// name are refused, and expires_at must be after now.
func TestCreateKey_ChecksNameTypeAndExpiry(t *testing.T) {
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

// A key's lookup prefix with another secret is refused.
func TestValidateKey_RefusesAWrongSecret(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Real", KeyType: auth.KeyTypeSecret})
	fake := result.Key.KeyPrefix + strings.Repeat("f", len(result.RawKey)-len(result.Key.KeyPrefix))
	if fake == result.RawKey {
		t.Skip("astronomically unlikely collision")
	}
	_, err := k.svc.ValidateKey(liveCtx(t), fake)
	wantInvalidKey(t, err, "the key's prefix with another secret")
}

// A stored key can have an empty salt and a hash of SHA-256(key) (hashKey).
// It validates, as a live key.
func TestValidateKey_AnUnsaltedSetupKeyStillValidates(t *testing.T) {
	k := newKeyRig(t)
	raw := "hc_secret_live_" + strings.Repeat("5e7a", 16)
	sum := sha256.Sum256([]byte(raw))
	// No request makes a key with no salt, so the row is inserted here, live
	// by the transaction's app.livemode.
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

// A key whose expires_at has passed is refused.
func TestValidateKey_RefusesAnExpiredKey(t *testing.T) {
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

// A failed last_used_at write does not refuse the key: no auth decision reads
// the column, so ValidateKey logs the failure and returns the key.
//
// The failure is real. Another transaction holds the key's row lock, so the
// lookup (a plain read, which takes no lock) succeeds while the UPDATE waits on
// the lock until the request's context ends: the write fails because the
// request's time ran out, as it does under lock contention in production.
func TestValidateKey_AFailedLastUsedWriteDoesNotRefuseTheKey(t *testing.T) {
	k := newKeyRig(t)
	created := k.create(t, testCtx(t), auth.CreateKeyInput{Name: "CI"})

	locker, err := k.db.BeginTx(t.Context(), postgres.TxBypass, "")
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
		key, err := k.svc.ValidateKey(reqCtx, created.RawKey)
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
	var used sql.NullTime
	k.bypass(t, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT last_used_at FROM api_keys WHERE id = $1`, created.Key.ID).Scan(&used)
	})
	if used.Valid {
		t.Fatal("last_used_at was written: the test did not make the write fail")
	}
}

// Another tenant's revoke or rotate does not find the key, and the key still
// validates.
func TestRevokeAndRotateKey_AnotherTenantFindsNoKey(t *testing.T) {
	k := newKeyRig(t)
	other := testutil.CreateTestTenant(t, k.db, "Other")
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "Test", KeyType: auth.KeyTypeSecret})
	_, revokeErr := k.svc.RevokeKey(liveCtx(t), other, result.Key.ID)
	_, rotateErr := k.svc.RotateKey(liveCtx(t), other, result.Key.ID, auth.RotateKeyInput{})
	for name, err := range map[string]error{"revoke": revokeErr, "rotate": rotateErr} {
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s from another tenant: got %v, want not found", name, err)
		}
	}
	if _, err := k.svc.ValidateKey(liveCtx(t), result.RawKey); err != nil {
		t.Fatalf("the key stopped validating after another tenant's revoke and rotate: %v", err)
	}
}

// A rotation's grace period is 0 to 604800 seconds (7 days).
func TestRotateKey_RefusesAGraceOutOfRange(t *testing.T) {
	k := newKeyRig(t)
	result := k.create(t, liveCtx(t), auth.CreateKeyInput{Name: "K", KeyType: auth.KeyTypeSecret})
	for name, grace := range map[string]int64{"negative": -1, "over the maximum": 604801} {
		t.Run(name, func(t *testing.T) {
			_, err := k.svc.RotateKey(liveCtx(t), k.tenantID, result.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: grace})
			wantValidation(t, err, "expires_in_seconds")
		})
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

// A rotation copies the old key's name, type and expiry as they are, and
// checks none of them again: create's checks are for a create request. The
// old key's name here has surrounding spaces and is longer than a new key's
// may be, and the replacement keeps it unchanged.
func TestRotateKey_CopiesTheOldKeyAsItIs(t *testing.T) {
	k := newKeyRig(t)
	ctx := testCtx(t)
	expiresAt := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	created := k.create(t, ctx, auth.CreateKeyInput{Name: "CI", KeyType: auth.KeyTypeIngest, ExpiresAt: &expiresAt})
	stored := "  " + strings.Repeat("n", 120) + "  "
	k.bypass(t, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE api_keys SET name = $1 WHERE id = $2`, stored, created.Key.ID)
		return err
	})

	rotated, err := k.svc.RotateKey(ctx, k.tenantID, created.Key.ID, auth.RotateKeyInput{})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got := rotated.NewKey.Name; got != stored {
		t.Errorf("the replacement's name is %q, want the old key's %q", got, stored)
	}
	if got := rotated.NewKey.KeyType; got != string(auth.KeyTypeIngest) {
		t.Errorf("the replacement's type is %s, want ingest", got)
	}
	if got := rotated.NewKey.ExpiresAt; got == nil || !got.Equal(expiresAt) {
		t.Errorf("the replacement expires at %v, want the old key's %v", got, expiresAt)
	}
}

// blockingAudit holds the change's transaction open inside its audit write,
// after the change's own writes, until release is closed; then it writes the
// real row. It is used for one change.
type blockingAudit struct {
	inner   auth.AuditWriter
	entered chan struct{}
	release chan struct{}
}

func (b blockingAudit) LogInTxFields(ctx context.Context, tx *sql.Tx, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error {
	close(b.entered)
	<-b.release
	return b.inner.LogInTxFields(ctx, tx, action, resourceType, resourceID, resourceLabel, metadata)
}

// Two immediate rotations of one key at once make one replacement. The first
// locks the key's row and holds its transaction open; the second waits on
// the lock, then finds the key revoked and answers invalid state (409).
func TestRotateKey_ConcurrentRotationsMakeOneReplacement(t *testing.T) {
	k := newKeyRig(t)
	ctx := liveCtx(t)
	original := k.create(t, ctx, auth.CreateKeyInput{Name: "Deploy"})

	held := blockingAudit{inner: audit.NewLogger(k.db), entered: make(chan struct{}), release: make(chan struct{})}
	released := false
	release := func() {
		if !released {
			released = true
			close(held.release)
		}
	}
	defer release()

	type outcome struct {
		res auth.RotateKeyResult
		err error
	}
	rotate := func(svc *auth.Service, done chan<- outcome) {
		res, err := svc.RotateKey(ctx, k.tenantID, original.Key.ID, auth.RotateKeyInput{})
		done <- outcome{res, err}
	}
	firstDone, secondDone := make(chan outcome, 1), make(chan outcome, 1)
	go rotate(auth.NewService(k.db, held), firstDone)
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first rotation did not reach its audit write in 10s")
	}
	go rotate(k.svc, secondDone)
	testutil.WaitForLockWaiter(t, t.Context(), testutil.AdminPool(t), "api_keys")
	release()

	wait := func(done <-chan outcome, which string) outcome {
		t.Helper()
		select {
		case o := <-done:
			return o
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s rotation did not return in 10s", which)
			return outcome{}
		}
	}
	first, second := wait(firstDone, "first"), wait(secondDone, "second")
	if first.err != nil {
		t.Fatalf("first rotation: %v", first.err)
	}
	if !errors.Is(second.err, errs.ErrInvalidState) {
		t.Fatalf("second rotation: got %v, want invalid state (the key is revoked)", second.err)
	}

	if listed := k.list(t, ctx); len(listed) != 2 {
		t.Fatalf("got %d keys, want the original and one replacement: %+v", len(listed), listed)
	}
	var rotations int
	for _, r := range k.auditRows(t) {
		if r.action == domain.AuditActionRotate {
			rotations++
		}
	}
	if rotations != 1 {
		t.Errorf("got %d rotate audit rows, want 1", rotations)
	}
}

var errAuditDown = errors.New("injected audit failure")

// failingAudit fails every write, as an audit_log insert that fails would.
type failingAudit struct{}

func (failingAudit) LogInTxFields(context.Context, *sql.Tx, string, string, string, string, map[string]any) error {
	return errAuditDown
}

// A key change and its audit row commit together: a create, revoke or
// rotation whose row cannot be written is not made, and every key still
// validates.
func TestKeyChange_AFailedAuditRowLeavesNoChange(t *testing.T) {
	k := newKeyRig(t)
	ctx := liveCtx(t)
	failing := auth.NewService(k.db, failingAudit{})
	toRevoke, toRotate, toRotateWithGrace := k.create(t, ctx, auth.CreateKeyInput{Name: "Revoke"}),
		k.create(t, ctx, auth.CreateKeyInput{Name: "Rotate"}), k.create(t, ctx, auth.CreateKeyInput{Name: "Rotate with grace"})
	before := k.list(t, ctx)

	for name, change := range map[string]func() error{
		"create": func() error {
			_, err := failing.CreateKey(ctx, k.tenantID, auth.CreateKeyInput{Name: "Never stored"})
			return err
		},
		"revoke": func() error {
			_, err := failing.RevokeKey(ctx, k.tenantID, toRevoke.Key.ID)
			return err
		},
		"rotate": func() error {
			_, err := failing.RotateKey(ctx, k.tenantID, toRotate.Key.ID, auth.RotateKeyInput{})
			return err
		},
		"rotate with grace": func() error {
			_, err := failing.RotateKey(ctx, k.tenantID, toRotateWithGrace.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: 3600})
			return err
		},
	} {
		if err := change(); !errors.Is(err, errAuditDown) {
			t.Errorf("%s with a failing audit write: got %v, want the audit error", name, err)
		}
	}

	if after := k.list(t, ctx); !reflect.DeepEqual(before, after) {
		t.Fatalf("a change whose audit row failed was made:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, key := range []auth.CreateKeyResult{toRevoke, toRotate, toRotateWithGrace} {
		if _, err := k.svc.ValidateKey(ctx, key.RawKey); err != nil {
			t.Errorf("key %s stopped validating after a failed change: %v", key.Key.Name, err)
		}
	}
}

// Each change writes one row, on the change's transaction: create on the key,
// revoke on the key, rotate on the replacement with the old key's id. A
// refused change writes none, and no row holds a key's raw value, hash or
// salt.
func TestKeyChange_WritesItsAuditRow(t *testing.T) {
	k := newKeyRig(t)
	ctx := testCtx(t)
	created := k.create(t, ctx, auth.CreateKeyInput{Name: "CI", KeyType: auth.KeyTypeIngest})
	// A rotation with a grace, then one that revokes the old key at once: each
	// row says what happened to the old key.
	rotated, err := k.svc.RotateKey(ctx, k.tenantID, created.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: 60})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	rotatedNow, err := k.svc.RotateKey(ctx, k.tenantID, rotated.NewKey.ID, auth.RotateKeyInput{})
	if err != nil {
		t.Fatalf("rotate at once: %v", err)
	}
	if _, err := k.svc.RevokeKey(ctx, k.tenantID, rotatedNow.NewKey.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Refused: the key is already revoked.
	if _, err := k.svc.RevokeKey(ctx, k.tenantID, rotatedNow.NewKey.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("revoke a revoked key: got %v, want not found", err)
	}
	if _, err := k.svc.RotateKey(ctx, k.tenantID, rotatedNow.NewKey.ID, auth.RotateKeyInput{}); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("rotate a revoked key: got %v, want invalid state", err)
	}

	stamp := func(at *time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
	want := []auditRow{
		{action: domain.AuditActionCreate, resourceID: created.Key.ID, label: "CI", metadata: map[string]any{"key_type": "ingest", "livemode": false}},
		{action: domain.AuditActionRotate, resourceID: rotated.NewKey.ID, label: "CI", metadata: map[string]any{"key_type": "ingest", "old_key_id": created.Key.ID,
			"old_key_outcome": "expires", "old_key_stops_at": stamp(rotated.OldKey.ExpiresAt)}},
		{action: domain.AuditActionRotate, resourceID: rotatedNow.NewKey.ID, label: "CI", metadata: map[string]any{"key_type": "ingest", "old_key_id": rotated.NewKey.ID,
			"old_key_outcome": "revoked", "old_key_stops_at": stamp(rotatedNow.OldKey.RevokedAt)}},
		{action: domain.AuditActionRevoke, resourceID: rotatedNow.NewKey.ID, label: "CI", metadata: map[string]any{"key_type": "ingest"}},
	}
	got := k.auditRows(t)
	if len(got) != len(want) {
		t.Fatalf("got %d api_key audit rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].action != want[i].action || got[i].resourceID != want[i].resourceID || got[i].label != want[i].label {
			t.Errorf("row %d: got %s on %s (%q), want %s on %s (%q)", i,
				got[i].action, got[i].resourceID, got[i].label, want[i].action, want[i].resourceID, want[i].label)
		}
		if !reflect.DeepEqual(got[i].metadata, want[i].metadata) {
			t.Errorf("row %d (%s): metadata %v, want exactly %v", i, want[i].action, got[i].metadata, want[i].metadata)
		}
	}

	secrets := map[string]string{"the first raw key": created.RawKey, "the replacement's raw key": rotated.RawKey, "the second replacement's raw key": rotatedNow.RawKey}
	for _, id := range []string{created.Key.ID, rotated.NewKey.ID, rotatedNow.NewKey.ID} {
		secrets[id+"'s hash"], secrets[id+"'s salt"] = k.stored(t, id)
	}
	for i, r := range got {
		for what, secret := range secrets {
			if secret != "" && (strings.Contains(r.rawMetadata, secret) || strings.Contains(r.label, secret)) {
				t.Errorf("row %d (%s) holds %s", i, r.action, what)
			}
		}
	}
}
