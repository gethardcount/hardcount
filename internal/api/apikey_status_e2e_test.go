package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The key list says each key's status: active, expired from its expires_at
// on, revoked once revoked. The old key of a rotation with a grace period is
// active until the grace ends.
func TestE2E_APIKeyList_SaysEachKeysStatus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "key-status-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@key-status.test", "key-status-pw-123"
	flowBootstrap(t, ts, "key-status-bootstrap", email, password)
	owner := login(t, ts, email, password)
	create := func(name string) string {
		t.Helper()
		body := flowDo(t, ts, "POST", "/v1/api-keys", flowReq{cookie: owner,
			body: map[string]any{"name": name, "key_type": "secret"}}).want(t, 201, "create "+name).body
		return flowStr(body, "key", "id")
	}
	active, expired, revoked, rotated := create("Active"), create("Expired"), create("Revoked"), create("Rotated")

	// The API refuses an expiry in the past, so the row is aged here.
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	flowDo(t, ts, "DELETE", "/v1/api-keys/"+revoked, flowReq{cookie: owner}).want(t, 200, "revoke")
	replacement := flowStr(flowDo(t, ts, "POST", "/v1/api-keys/"+rotated+"/rotate", flowReq{cookie: owner,
		body: map[string]any{"expires_in_seconds": 3600}}).want(t, 200, "rotate with grace").body, "new_key", "id")

	list := flowDo(t, ts, "GET", "/v1/api-keys?limit=100", flowReq{cookie: owner}).want(t, 200, "list keys").body
	status := map[string]string{}
	for _, k := range list["data"].([]any) {
		m := k.(map[string]any)
		s, _ := m["status"].(string)
		status[m["id"].(string)] = s
	}
	for id, want := range map[string]string{
		active: "active", expired: "expired", revoked: "revoked", rotated: "active", replacement: "active",
	} {
		if status[id] != want {
			t.Errorf("key %s: status %q, want %q", id, status[id], want)
		}
	}
}
