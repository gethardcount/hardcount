package auth_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

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
		if strings.Contains(string(meta), "hc_secret_") {
			t.Errorf("an audit row's metadata holds a raw key: %s", meta)
		}
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

// Each change writes one row on its own transaction: create on the key,
// revoke on the key, rotate on the replacement with the old key's id. A
// refused change writes none.
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
		{domain.AuditActionCreate, created.Key.ID, "CI", map[string]any{"key_type": "ingest", "livemode": false}},
		{domain.AuditActionRotate, rotated.NewKey.ID, "CI", map[string]any{"key_type": "ingest", "old_key_id": created.Key.ID}},
		{domain.AuditActionRevoke, rotated.NewKey.ID, "CI", map[string]any{"key_type": "ingest"}},
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
		for k, v := range want[i].metadata {
			if got[i].metadata[k] != v {
				t.Errorf("row %d (%s): metadata %s = %v, want %v", i, want[i].action, k, got[i].metadata[k], v)
			}
		}
	}
}
