package auth_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// A key change and its audit row commit together: a change whose row cannot
// be written is not made, and a change that is refused writes no row.

var errAuditDown = errors.New("injected audit failure")

// failingAudit fails every write, as an audit_log insert that fails would.
type failingAudit struct{}

func (failingAudit) LogInTxFields(context.Context, *sql.Tx, string, string, string, string, map[string]any) error {
	return errAuditDown
}

func TestKeyChange_AFailedAuditRowLeavesNoChange(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Audit Shared Fate")
	ctx := postgres.WithLivemode(t.Context(), true)
	keys := auth.NewService(db, audit.NewLogger(db))
	failing := auth.NewService(db, failingAudit{})

	create := func(name string) auth.CreateKeyResult {
		t.Helper()
		res, err := keys.CreateKey(ctx, tenantID, auth.CreateKeyInput{Name: name})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return res
	}
	listed := func() []domain.APIKey {
		t.Helper()
		got, _, err := keys.ListKeys(ctx, auth.ListFilter{TenantID: tenantID, Limit: 100})
		if err != nil {
			t.Fatalf("list keys: %v", err)
		}
		return got
	}
	toRevoke, toRotate, toRotateWithGrace := create("Revoke"), create("Rotate"), create("Rotate with grace")
	before := listed()

	for name, change := range map[string]func() error{
		"create": func() error {
			_, err := failing.CreateKey(ctx, tenantID, auth.CreateKeyInput{Name: "Never stored"})
			return err
		},
		"revoke": func() error {
			_, err := failing.RevokeKey(ctx, tenantID, toRevoke.Key.ID)
			return err
		},
		"rotate": func() error {
			_, err := failing.RotateKey(ctx, tenantID, toRotate.Key.ID, auth.RotateKeyInput{})
			return err
		},
		"rotate with grace": func() error {
			_, err := failing.RotateKey(ctx, tenantID, toRotateWithGrace.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: 3600})
			return err
		},
	} {
		if err := change(); !errors.Is(err, errAuditDown) {
			t.Errorf("%s with a failing audit write: got %v, want the audit error", name, err)
		}
	}

	if after := listed(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a change whose audit row failed was made:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, k := range []auth.CreateKeyResult{toRevoke, toRotate, toRotateWithGrace} {
		if _, err := keys.ValidateKey(ctx, k.RawKey); err != nil {
			t.Errorf("key %s stopped validating after a failed change: %v", k.Key.Name, err)
		}
	}
}

type auditRow struct {
	action, resourceID, label string
	metadata                  map[string]any
	rawMetadata               string
}

func apiKeyAuditRows(t *testing.T, db *postgres.DB, tenantID string) []auditRow {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.Query(`SELECT action, resource_id, resource_label, metadata FROM audit_log
		WHERE tenant_id = $1 AND resource_type = 'api_key' ORDER BY created_at, id`, tenantID)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var meta []byte
		if err := rows.Scan(&r.action, &r.resourceID, &r.label, &meta); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		r.rawMetadata = string(meta)
		if err := json.Unmarshal(meta, &r.metadata); err != nil {
			t.Fatalf("decode metadata %s: %v", meta, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Each change writes one row, on the change's transaction: create on the key,
// revoke on the key, rotate on the replacement with the old key's id. A
// refused change writes none, and no row holds a key's raw value, hash or
// salt.
func TestKeyChange_WritesItsAuditRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Audit Rows")
	ctx := postgres.WithLivemode(t.Context(), false)
	keys := auth.NewService(db, audit.NewLogger(db))

	created, err := keys.CreateKey(ctx, tenantID, auth.CreateKeyInput{Name: "CI", KeyType: auth.KeyTypeIngest})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rotated, err := keys.RotateKey(ctx, tenantID, created.Key.ID, auth.RotateKeyInput{ExpiresInSeconds: 60})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := keys.RevokeKey(ctx, tenantID, rotated.NewKey.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Refused: the key is already revoked.
	if _, err := keys.RevokeKey(ctx, tenantID, rotated.NewKey.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("revoke a revoked key: got %v, want not found", err)
	}
	if _, err := keys.RotateKey(ctx, tenantID, rotated.NewKey.ID, auth.RotateKeyInput{}); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("rotate a revoked key: got %v, want invalid state", err)
	}

	want := []auditRow{
		{action: domain.AuditActionCreate, resourceID: created.Key.ID, label: "CI", metadata: map[string]any{"key_type": "ingest", "livemode": false}},
		{action: domain.AuditActionRotate, resourceID: rotated.NewKey.ID, label: "CI", metadata: map[string]any{"key_type": "ingest", "old_key_id": created.Key.ID}},
		{action: domain.AuditActionRevoke, resourceID: rotated.NewKey.ID, label: "CI", metadata: map[string]any{"key_type": "ingest"}},
	}
	got := apiKeyAuditRows(t, db, tenantID)
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

	secrets := map[string]string{"the first raw key": created.RawKey, "the replacement's raw key": rotated.RawKey}
	for _, k := range []auth.CreateKeyResult{created, {Key: rotated.NewKey}} {
		hash, salt := storedHashAndSalt(t, db, k.Key.ID)
		secrets[k.Key.ID+"'s hash"], secrets[k.Key.ID+"'s salt"] = hash, salt
	}
	for i, r := range got {
		for what, secret := range secrets {
			if secret != "" && (strings.Contains(r.rawMetadata, secret) || strings.Contains(r.label, secret)) {
				t.Errorf("row %d (%s) holds %s", i, r.action, what)
			}
		}
	}
}

func storedHashAndSalt(t *testing.T, db *postgres.DB, id string) (hash, salt string) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRow(`SELECT key_hash, key_salt FROM api_keys WHERE id = $1`, id).Scan(&hash, &salt); err != nil {
		t.Fatalf("read key %s: %v", id, err)
	}
	return hash, salt
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

// waitForLockWait returns once a backend of this database is waiting on a
// lock in a statement on api_keys.
func waitForLockWait(t *testing.T, db *postgres.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.Pool.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%api_keys%'`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no statement on api_keys waited on a lock within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two immediate rotations of one key at once make one replacement. The first
// locks the key's row and holds its transaction open; the second waits on
// the lock, then finds the key revoked and answers invalid state (409).
func TestRotateKey_ConcurrentRotationsMakeOneReplacement(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Concurrent Rotations")
	ctx := postgres.WithLivemode(t.Context(), true)
	logger := audit.NewLogger(db)
	keys := auth.NewService(db, logger)
	original, err := keys.CreateKey(ctx, tenantID, auth.CreateKeyInput{Name: "Deploy"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	held := blockingAudit{inner: logger, entered: make(chan struct{}), release: make(chan struct{})}
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
		res, err := svc.RotateKey(ctx, tenantID, original.Key.ID, auth.RotateKeyInput{})
		done <- outcome{res, err}
	}
	firstDone, secondDone := make(chan outcome, 1), make(chan outcome, 1)
	go rotate(auth.NewService(db, held), firstDone)
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first rotation did not reach its audit write in 10s")
	}
	go rotate(keys, secondDone)
	waitForLockWait(t, db)
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

	listed, _, err := keys.ListKeys(ctx, auth.ListFilter{TenantID: tenantID, Limit: 100})
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("got %d keys, want the original and one replacement: %+v", len(listed), listed)
	}
	var rotations int
	for _, r := range apiKeyAuditRows(t, db, tenantID) {
		if r.action == domain.AuditActionRotate {
			rotations++
		}
	}
	if rotations != 1 {
		t.Errorf("got %d rotate audit rows, want 1", rotations)
	}
}
