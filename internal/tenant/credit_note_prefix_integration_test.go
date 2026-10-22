package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestSettings_CreditNotePrefix (C2): the credit note prefix is set through
// PUT /v1/settings, read back by GET, and used for the next credit note
// number. Notes already numbered keep their numbers: the sequence carries on
// under the new prefix, it does not start again.
//
// Mutation-verify: drop credit_note_prefix from Upsert's column list — the
// read-back and the ACME-CN number assertions fail.
func TestSettings_CreditNotePrefix(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Credit note prefix")
	store := NewSettingsStore(db)
	h := NewSettingsHandler(store)

	nextNumber := func() string {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		num, err := store.NextCreditNoteNumberTx(ctx, tx, tenantID)
		if err != nil {
			t.Fatalf("next credit note number: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return num
	}
	put := func(body string) (int, map[string]any) {
		t.Helper()
		rr, req := settingsRequest(t, tenantID, body)
		h.upsert(rr, req)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	got, err := store.Get(ctx, tenantID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CreditNotePrefix != "CN" {
		t.Fatalf("default credit_note_prefix = %q, want CN", got.CreditNotePrefix)
	}
	if n := nextNumber(); n != "CN-000001" {
		t.Fatalf("first number = %q, want CN-000001", n)
	}

	code, body := put(`{"credit_note_prefix":"acme-cn"}`)
	if code != http.StatusOK {
		t.Fatalf("set prefix: status %d body=%v", code, body)
	}
	if body["credit_note_prefix"] != "acme-cn" {
		t.Errorf("PUT answered credit_note_prefix=%v, want acme-cn", body["credit_note_prefix"])
	}
	if got, _ := store.Get(ctx, tenantID); got.CreditNotePrefix != "acme-cn" {
		t.Errorf("GET credit_note_prefix = %q, want acme-cn", got.CreditNotePrefix)
	}
	if n := nextNumber(); n != "ACME-CN-000002" {
		t.Errorf("number after the change = %q, want ACME-CN-000002 (new prefix, same sequence)", n)
	}

	// A save that leaves the field out keeps it.
	if code, body := put(`{"tax_name":"VAT"}`); code != http.StatusOK || body["credit_note_prefix"] != "acme-cn" {
		t.Errorf("partial save: status %d credit_note_prefix=%v, want 200 and acme-cn kept", code, body["credit_note_prefix"])
	}

	// A bad value is refused on its own field and changes nothing.
	code, body = put(`{"credit_note_prefix":"ACME CN"}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("bad prefix: status %d, want 422 (body=%v)", code, body)
	}
	if errObj, _ := body["error"].(map[string]any); errObj == nil || errObj["param"] != "credit_note_prefix" {
		t.Errorf("bad prefix: error %v, want it on credit_note_prefix", body["error"])
	}
	if got, _ := store.Get(ctx, tenantID); got.CreditNotePrefix != "acme-cn" {
		t.Errorf("after a refused save credit_note_prefix = %q, want acme-cn", got.CreditNotePrefix)
	}

	// An empty value goes back to the default.
	if code, body := put(`{"credit_note_prefix":""}`); code != http.StatusOK || body["credit_note_prefix"] != "CN" {
		t.Errorf("empty prefix: status %d credit_note_prefix=%v, want 200 and CN", code, body["credit_note_prefix"])
	}
}
